package transfer

// AI 标题清洗客户端：TMDB 全链路（文件名 → 父目录重试）失败后，
// 用 OpenAI 兼容接口把脏文件名还原成可搜索的正式标题，再回搜 TMDB。
//
// AI 只做「标题还原」一件事，不碰分类、不构造 MediaInfo：
// 命中后仍走原 TMDB 链路，海报/NFO/分类全部照旧；回搜再失败就照旧落「未分类」。
//
// 防滥用两级：① 缓存按剧集级 key 去重，整部剧只调一次；② 每日配额硬上限。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mmbot/internal/httpx"
)

const (
	aiCacheMax     = 512
	aiRequestDelay = 300 * time.Millisecond // 相邻请求最小间隔，避免触发上游限流
	aiDefaultQuota = 50
)

// AIConfig AI 兜底配置，由 env 注入。
type AIConfig struct {
	Enabled    bool
	APIKey     string
	BaseURL    string
	Model      string
	DailyQuota int
}

// aiResult AI 返回的标题还原结果。
type aiResult struct {
	Title string `json:"title"`
	Year  int    `json:"year"`
	// Valid=false 表示 AI 明确回答「认不出」，可负缓存；请求失败（超时/5xx）不缓存。
	Valid bool `json:"-"`
}

// AIClient OpenAI 兼容的标题清洗客户端。
type AIClient struct {
	BaseURL string
	APIKey  string
	Model   string
	Quota   int

	http *httpx.Client

	mu    sync.Mutex
	cache map[string]*aiResult

	// 配额：原子计数 + 日期字符串，跨 goroutine 安全（整理任务与 Web 单文件整理并发）。
	quotaUsed atomic.Int64
	quotaDay  atomic.Value // string

	// lastCall 限流用，mu 保护。
	lastCall time.Time
}

// NewAIClient 创建 AI 客户端；未启用或 apiKey 为空返回 nil（不启用）。
func NewAIClient(cfg AIConfig) *AIClient {
	if !cfg.Enabled || cfg.APIKey == "" {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.deepseek.com"
	}
	// 兼容用户填 https://api.deepseek.com/v1 或 .../v1/chat/completions
	if strings.HasSuffix(base, "/chat/completions") {
		base = strings.TrimSuffix(base, "/chat/completions")
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = "deepseek-chat"
	}
	quota := cfg.DailyQuota
	if quota <= 0 {
		quota = aiDefaultQuota
	}
	c := &AIClient{
		BaseURL: base,
		APIKey:  cfg.APIKey,
		Model:   model,
		Quota:   quota,
		http:    httpx.New(30 * time.Second),
		cache:   map[string]*aiResult{},
	}
	c.quotaDay.Store(time.Now().Format("2006-01-02"))
	return c
}

// quotaExhausted 检查并占用一次配额（含跨日重置）。
func (c *AIClient) quotaExhausted() bool {
	today := time.Now().Format("2006-01-02")
	if c.quotaDay.Load() != today {
		c.quotaDay.Store(today)
		c.quotaUsed.Store(0)
	}
	if c.quotaUsed.Load() >= int64(c.Quota) {
		return true
	}
	c.quotaUsed.Add(1)
	return false
}

// Usage 当前用量（供日志展示）。
func (c *AIClient) Usage() (used, quota int) { return int(c.quotaUsed.Load()), c.Quota }

func (c *AIClient) cacheGet(key string) (*aiResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.cache[key]
	return r, ok
}

func (c *AIClient) cachePut(key string, r *aiResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= aiCacheMax {
		for k := range c.cache {
			delete(c.cache, k)
			break
		}
	}
	c.cache[key] = r
}

// aiCacheKey 剧集级缓存键：优先 TMDBID（同剧共享），否则用父目录名 + 季号。
// 不用文件清洗结果 —— 不同发布组的剧集清洗结果可能不一致，会导致缓存击穿重复调用。
func aiCacheKey(meta MetaInfo, parentDir string) string {
	if meta.TMDBID > 0 {
		return "id|" + strconv.Itoa(meta.TMDBID)
	}
	key := parentDir
	if key == "" {
		key = meta.Name
	}
	if meta.Season > 0 {
		key += "|S" + strconv.Itoa(meta.Season)
	}
	return "dir|" + key
}

// Recognize 把脏标题还原成正式标题。cacheKey 为剧集级缓存键。
// nil = 不可用（未启用/请求失败/配额耗尽，均不缓存）；Valid=false = AI 明确认不出（已负缓存）。
func (c *AIClient) Recognize(cacheKey, cleanName, parentDir string, year int) *aiResult {
	if c == nil || cleanName == "" {
		return nil
	}
	if r, ok := c.cacheGet(cacheKey); ok {
		return r
	}

	if c.quotaExhausted() {
		used, quota := c.Usage()
		log.Printf("AI 识别已达每日配额上限（今日 %d/%d），本次跳过", used, quota)
		return nil
	}

	r := c.request(cleanName, parentDir, year)
	if r == nil {
		// 请求失败（网络/上游错误）：不缓存，下次仍可重试
		return nil
	}
	// AI 明确回答（含「认不出」）才缓存，避免超时把整部剧锁死
	c.cachePut(cacheKey, r)
	return r
}

// request 调 /chat/completions。
func (c *AIClient) request(cleanName, parentDir string, year int) *aiResult {
	// 相邻请求最小间隔，避免短时间连打上游
	c.mu.Lock()
	if wait := aiRequestDelay - time.Since(c.lastCall); wait > 0 && !c.lastCall.IsZero() {
		c.mu.Unlock()
		time.Sleep(wait)
		c.mu.Lock()
	}
	c.lastCall = time.Now()
	c.mu.Unlock()

	hintYear := ""
	if year > 0 {
		hintYear = strconv.Itoa(year)
	}

	sysPrompt := "你是影视文件名识别助手。用户给你一个从网盘文件名清洗出来的标题，" +
		"它可能有错别字、是意译名、残留发布组或质量标记、或缺失原文名。" +
		"请还原出这部影视作品的正式名称，用于在 TMDB 上检索。只输出 JSON，不要解释。"
	userPrompt := fmt.Sprintf(`原始标题：%s
父目录名：%s
已知年份：%s

JSON 字段：
- title：还原后的正式名称。优先给中文正式名；若是外语片且中文名不通用，给原文名
- year：上映/首播年份，不确定填 0
若完全无法判断这是哪部作品，返回 {"title":"","year":0}`, cleanName, parentDir, hintYear)

	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature":     0.1,
		"response_format": map[string]string{"type": "json_object"},
	}
	data, code, err := c.http.PostJSON(context.Background(),
		c.BaseURL+"/chat/completions",
		map[string]string{"Authorization": "Bearer " + c.APIKey},
		body)
	if err != nil {
		log.Printf("AI 识别请求失败: %v", err)
		return nil
	}
	if code != 200 {
		log.Printf("AI 识别返回 HTTP %d: %s", code, truncateStr(string(data), 200))
		return nil
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || len(resp.Choices) == 0 {
		log.Printf("AI 识别响应解析失败: %v", err)
		return nil
	}

	content := stripCodeFence(strings.TrimSpace(resp.Choices[0].Message.Content))
	var r aiResult
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		log.Printf("AI 识别结果非 JSON: %s", truncateStr(content, 200))
		return nil
	}
	r.Title = strings.TrimSpace(r.Title)
	if r.Title == "" {
		log.Printf("AI 无法识别: %s", cleanName)
		return &aiResult{Valid: false}
	}
	r.Valid = true
	log.Printf("AI 标题还原: '%s' -> '%s' (%d)", cleanName, r.Title, r.Year)
	return &r
}

var codeFenceRe = regexp.MustCompile("(?s)^```[a-zA-Z]*\\s*(.*?)\\s*```$")

func stripCodeFence(s string) string {
	if m := codeFenceRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
