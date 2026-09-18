// Package panfx 抓取「123分享社区」(123panfx.com / pan1.me，同一套 Xiuno BBS 数据) 的影视资源帖。
//
// 站点是 Xiuno BBS 4.0.4，前端用 query-string 伪静态路由：
//
//	首页   /                      版块  /forum-48-1.htm
//	搜索   /search-1.htm?keyword=XXX   （range: 1=主题贴 2=所有贴 3=用户）
//	帖子   /thread-63977.htm      用户  /user-32702.htm
//
// 关键约束（2026-09 实测）：
//   - 搜索结果页**无需登录**即可抓取，条目包含标题、标签、体积、作者、时间；
//   - 帖子详情页对游客返回「您所在的用户组权限不足」，正文与资源链接必须登录才可见；
//   - 因此「搜索」可以匿名做，「解锁」（读帖子正文里的网盘链接）必须带登录 Cookie。
//
// 解锁链路：搜索 → 拿 tid → 带 Cookie 取 thread 页 → 抽取正文里的网盘链接。
//
// ⚠️ 站点机制（2026-09 实测，与最初猜测完全不同）：**不是积分制**。
// 账号有金币，但帖子正文被隐藏的真实原因是「回复后再查看」——
// 服务端**根本不下发**那段 HTML，唯一途径是该账号在该帖下发过一条回复。
// 详见 unlock.go。
package panfx

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/httpx"
)

// 站点地址与路由模板。BaseURL 可通过配置覆盖（主站与备用域名数据互通）。
const (
	DefaultBase = "https://www.123panfx.com"
)

// ResultSize 单次搜索返回给前端的条目上限。
// 站点默认按相关度降序，取前 10 条已经能覆盖同一部片的常见规格
// （1080P / 4K / REMUX / 原盘），列表也不会长到要翻页。
const ResultSize = 10

// 短剧噪声：站点「短剧」版块发帖量极大，搜任何剧名都会掺进一批无关短剧
// （标题里带「短剧」且在短剧版块 forum-59）。这些不是用户要找的影视资源，直接剔除。
var (
	reThreadHref = regexp.MustCompile(`data-href="\?thread-(\d+)\.htm"`)
	reItemStart  = regexp.MustCompile(`<li class="media thread[^"]*"`)
	reSubject    = regexp.MustCompile(`(?s)<div class="style3_subject[^"]*"[^>]*>(.*?)</div>`)
	reAnchor     = regexp.MustCompile(`(?s)<a href="\?thread-(\d+)\.htm"[^>]*>(.*?)</a>`)
	reTag        = regexp.MustCompile(`(?s)<a href="\?forum-\d+-1\.htm[^"]*"[^>]*>(.*?)</a>`)
	reForum      = regexp.MustCompile(`\?forum-(\d+)-1\.htm`)
	reAuthor     = regexp.MustCompile(`<a href="\?user-\d+\.htm"[^>]*class="username[^"]*"[^>]*>([^<]*)<`)
	reDate       = regexp.MustCompile(`<span\s+class="date text-grey hidden-sm">\s*([^<]+?)\s*</span>`)
	reComments   = regexp.MustCompile(`\?thread-\d+\.htm#comments"[^>]*>.*?<span>\s*(\d+)\s*</span>`)
	reTagText    = regexp.MustCompile(`<[^>]+>`)
	reSpace      = regexp.MustCompile(`\s+`)
)

// 短剧版块 id（噪声来源）
const shortPlayForum = "59"

// Item 一条资源帖。
type Item struct {
	TID      string   `json:"tid"`      // 帖子 id
	Title    string   `json:"title"`    // 帖子标题（含规格描述）
	URL      string   `json:"url"`      // 帖子页绝对地址
	Tags     []string `json:"tags"`     // 质量/语言/类型标签
	Size     string   `json:"size"`     // 从标题里抽出来的体积，如 484.45GB
	Author   string   `json:"author"`   // 发帖人
	Date     string   `json:"date"`     // 发帖时间
	Comments int      `json:"comments"` // 评论数（热度参考）
}

// Client 资源站抓取客户端。
type Client struct {
	Base   string       // 站点根地址，空则用 DefaultBase
	Cookie string       // 登录 Cookie（解锁必需；只搜索可以留空）
	http   *httpx.Client

	// 自动回复（回复可见帖）的节流状态。
	// 站点是「回复后再查看」而非积分制，解锁要先以用户身份发一条回复，
	// 因此必须自己管住频率，否则短时间连点多个帖会像刷帖号。
	replyMu   sync.Mutex
	replied   map[string]time.Time // tid → 已回复时间（24h 内不重复回复）
	lastReply time.Time
	// 回复记录落盘文件。不持久化的话，进程重启会忘记「已回复过哪些帖」，
	// 用户重复点同一个资源就会重复回帖 —— 那是最容易被站方盯上的行为。
	stateFile string

	// minReplyGap 两次自动回复之间的最短间隔。
	minReplyGap time.Duration
}

// SetMinReplyGap 设置两次自动回复之间的最短间隔（默认 30s）。
// 调大更安全（不容易被风控），调小则解锁更快。
func (c *Client) SetMinReplyGap(d time.Duration) {
	if d > 0 {
		c.minReplyGap = d
	}
}

// ReplyGap 返回当前的最短回复间隔。
func (c *Client) ReplyGap() time.Duration { return c.minReplyGap }

// SetStateFile 设置回复记录的持久化路径（空则只存内存）。
// 建议指向数据目录，这样容器重启后仍记得「这个帖子已回过」。
func (c *Client) SetStateFile(path string) {
	c.stateFile = strings.TrimSpace(path)
	c.loadReplied()
}

// replyState 落盘结构。用 tid→时间戳，便于按 24h 过期清理。
type replyState struct {
	Replied map[string]int64 `json:"replied"`
}

// loadReplied 读取已有的回复记录（文件不存在/损坏都静默跳过，不影响功能）。
func (c *Client) loadReplied() {
	if c.stateFile == "" {
		return
	}
	data, err := os.ReadFile(c.stateFile)
	if err != nil {
		return
	}
	var st replyState
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	c.replyMu.Lock()
	defer c.replyMu.Unlock()
	if c.replied == nil {
		c.replied = map[string]time.Time{}
	}
	for tid, ts := range st.Replied {
		c.replied[tid] = time.Unix(ts, 0)
	}
}

// saveReplied 持久化回复记录（调用方需已持锁）。
func (c *Client) saveRepliedLocked() {
	if c.stateFile == "" {
		return
	}
	st := replyState{Replied: make(map[string]int64, len(c.replied))}
	for tid, t := range c.replied {
		st.Replied[tid] = t.Unix()
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.stateFile), 0o755)
	// 先写临时文件再改名：避免写一半崩溃留下半个 JSON，下次启动读不出来
	tmp := c.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("[Panfx] 回复记录写入失败 %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, c.stateFile); err != nil {
		log.Printf("[Panfx] 回复记录改名失败 %s: %v", c.stateFile, err)
	}
}

// New 创建客户端。cookie 可为空（匿名搜索）。
func New(base, cookie string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultBase
	}
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	return &Client{
		Base:   base,
		Cookie: strings.TrimSpace(cookie),
		// 站点是境外/带 CF 的普通站，给 20s；UA 照浏览器发，避免被当爬虫拦
		http: httpx.New(20 * time.Second),
		// 默认 30s：站点侧对高频回帖有风控，这是保守但够用的间隔
		minReplyGap: 30 * time.Second,
	}
}

// Configured 解锁能力是否就绪（配了登录 Cookie）。
func (c *Client) Configured() bool { return c != nil && c.Cookie != "" }

func (c *Client) headers(extra map[string]string) map[string]string {
	h := map[string]string{
		"User-Agent":      httpx.UA,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9",
	}
	if c.Cookie != "" {
		h["Cookie"] = c.Cookie
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// Search 按关键词整站搜索，返回清洗后的条目（最多 ResultSize 条）。
// 搜索失败返回 error；搜到 0 条返回空切片而不是 error。
//
// 站点是**字面子串匹配**：搜「F1 狂飙飞车」整串 0 条，但搜「狂飙飞车」有 11 条。
// 因此整串没结果时回落到按词拆分的探测（见 keywordProbes），保证中英混排片名也能搜到。
func (c *Client) Search(ctx context.Context, keyword string) ([]Item, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, fmt.Errorf("关键词为空")
	}

	for _, probe := range keywordProbes(keyword) {
		items, err := c.searchOnce(ctx, probe)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			if len(items) > ResultSize {
				items = items[:ResultSize]
			}
			return items, nil
		}
	}
	// 所有探测都 0 条：返回空结果（不是错误），前端显示「未找到资源」
	return []Item{}, nil
}

// keywordProbes 生成关键词的探测顺序。
//  1. 整串（最精确，命中即是完全匹配的帖子）；
//  2. 按空格/冒号切出的片段，从最长的开始 —— 「F1 狂飙飞车」→「狂飙飞车」命中；
//  3. 主标题（去掉副标题这类中文标点之后的部分）。
//
// 只做有限几次探测（最多 3 次），避免把一次搜索放大成十几次上游请求。
func keywordProbes(keyword string) []string {
	out := []string{keyword}
	seen := map[string]bool{keyword: true}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len([]rune(s)) < 2 || seen[s] || len(out) >= 3 {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	// 中英混排：英文名与中文名之间常以空格/冒号/全角冒号分隔，拆开后中文片段命中率最高
	seps := regexp.MustCompile(`[\s:：\-—–]+`)
	parts := seps.Split(keyword, -1)
	sortByLenDesc(parts)
	for _, p := range parts {
		add(p)
	}
	return out
}

// sortByLenDesc 按字符长度降序（长片段信息量更大，优先探测）。
func sortByLenDesc(parts []string) {
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			if len([]rune(parts[j])) > len([]rune(parts[i])) {
				parts[i], parts[j] = parts[j], parts[i]
			}
		}
	}
}

// searchOnce 单次关键词请求 + 解析。
func (c *Client) searchOnce(ctx context.Context, keyword string) ([]Item, error) {
	// range=1 只搜主题贴（range=2 会把回帖也翻出来，标题全是回复内容，没用）
	u := c.Base + "/search-1.htm?keyword=" + url.QueryEscape(keyword)
	body, status, err := c.http.Get(ctx, u, c.headers(map[string]string{"Referer": c.Base + "/search.htm"}))
	if err != nil {
		return nil, fmt.Errorf("请求资源站失败：%w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("资源站返回 HTTP %d", status)
	}
	html := string(body)
	// 站点对搜索引擎/未登录的兜底页：明确告知而不是当成 0 条结果
	if strings.Contains(html, "您所在的用户组") && !strings.Contains(html, "threadlist") {
		return nil, fmt.Errorf("资源站限制访问（用户组权限不足），请检查站点地址或稍后重试")
	}
	return parseSearch(html, c.Base), nil
}

// parseSearch 解析搜索结果页的条目列表。
// 按 <li class="media thread ..."> 切块，块内取标题/标签/作者/时间/评论数，
// 比整页正则更稳：标题里的标签（如 <span class="text-danger">关键词</span>）不会串到相邻条目。
func parseSearch(html, base string) []Item {
	idx := reItemStart.FindAllStringIndex(html, -1)
	items := make([]Item, 0, len(idx))
	for i, loc := range idx {
		end := len(html)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := html[loc[0]:end]

		// tid
		m := reThreadHref.FindStringSubmatch(block)
		if len(m) < 2 {
			continue
		}
		item := Item{TID: m[1]}

		// 标题：style3_subject 里的第一个 thread 链接
		subj := reSubject.FindStringSubmatch(block)
		if len(subj) < 2 {
			continue
		}
		anchor := reAnchor.FindStringSubmatch(subj[1])
		if len(anchor) < 3 {
			continue
		}
		item.Title = cleanText(anchor[2])
		if item.Title == "" {
			continue
		}

		// 标签：同一节点里的 forum-xx 链接
		for _, t := range reTag.FindAllStringSubmatch(subj[1], -1) {
			if txt := cleanText(t[1]); txt != "" {
				item.Tags = append(item.Tags, txt)
			}
		}
		item.Size = extractSize(item.Title)
		item.URL = fmt.Sprintf("%s/thread-%s.htm", base, item.TID)

		if a := reAuthor.FindStringSubmatch(block); len(a) > 1 {
			item.Author = cleanText(a[1])
		}
		if d := reDate.FindStringSubmatch(block); len(d) > 1 {
			item.Date = cleanText(d[1])
		}
		if n := reComments.FindStringSubmatch(block); len(n) > 1 {
			if v, err := strconv.Atoi(n[1]); err == nil {
				item.Comments = v
			}
		}
		// 噪声剔除（两类，见 isNoise）
		if item.isNoise(block) {
			continue
		}
		items = append(items, item)
	}
	return items
}

// requestTags 求资源帖的标志：站点用「求资源 / 未解决 / 已解决」标签区分求助帖，
// 标题本身也常以「求」开头。这类帖子里没有资源链接，混在列表里只会浪费用户一次点击。
var requestTags = map[string]bool{"求资源": true, "未解决": true, "已解决": true}

// isNoise 判断是否为应当剔除的噪声条目。
//
//	1. 短剧版块发的短剧帖 —— 站点短剧刷帖量极大，搜任何剧名都会掺进一批；
//	2. 求资源帖 —— 只有求助没有资源。
func (i Item) isNoise(block string) bool {
	for _, t := range i.Tags {
		if requestTags[t] {
			return true
		}
	}
	if strings.HasPrefix(i.Title, "求") {
		return true
	}
	inShortForum := false
	for _, f := range reForum.FindAllStringSubmatch(block, -1) {
		if f[1] == shortPlayForum {
			inShortForum = true
			break
		}
	}
	return inShortForum && strings.Contains(i.Title, "短剧")
}

// sizeRe 标题里的体积，如 484.45GB / 2.86G / 22.72GB / 1.67TB / 303M。
// 结尾要求是词边界，避免吃进「45.87GB」里「GB」后紧跟的字符；
// 关键约束是**数字与单位必须紧邻**（最多一个空格），否则「1080p 45.87GB」会被拆成两段误判。
var sizeRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s?(TB|GB|MB|G|M)(?:[\]】」)）\s]|$)`)

// extractSize 从标题里抽出体积，多个候选取最大的（通常是整包体积而非分集体积）。
//
// 只在合理区间内取值（>=100MB），过滤掉「1080P」被当成 1080G、
// 或「5」贴个 G 之类的伪体积；单位归一化成 GB/TB，便于列表里一眼比较。
func extractSize(title string) string {
	best := 0.0
	found := false
	for _, m := range sizeRe.FindAllStringSubmatch(title, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		unit := strings.ToUpper(m[2])
		mb := v
		switch unit {
		case "TB":
			mb = v * 1024 * 1024
		case "GB", "G":
			mb = v * 1024
		case "MB", "M":
			mb = v
		}
		// 合理区间：100MB ~ 200TB。低于 100MB 的要么是分集要么是误判，不展示。
		if mb < 100 || mb > 200*1024*1024 {
			continue
		}
		if !found || mb > best {
			found, best = true, mb
		}
	}
	if !found {
		return ""
	}
	// 归一化展示：>=1TB 用 TB，>=1GB 用 GB，否则 MB（都压掉多余小数位）
	if best >= 1024*1024 {
		return trimNum(best/1024/1024) + "TB"
	}
	if best >= 1024 {
		return trimNum(best/1024) + "GB"
	}
	return trimNum(best) + "MB"
}

// trimNum 去掉多余小数位（484.45 → "484.45"，2.80 → "2.8"，90.00 → "90"）。
func trimNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// cleanText 去标签、还原实体、压平空白。
func cleanText(s string) string {
	s = reTagText.ReplaceAllString(s, " ")
	s = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#039;", "'", "&#39;", "'",
	).Replace(s)
	return reSpacesTrim(s)
}

func reSpacesTrim(s string) string {
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}
