package web

// 详情页「资源」区块 API：代理 123panfx 资源站的搜索与解锁。
//
// 拆成两步是刻意的：
//   - 搜索（匿名，可缓存）不打上游站点以外的任何东西；
//   - 解锁必须由用户点按钮显式触发，绝不自动做。
// 这样「不点搜索就不搜这个站、不点解锁就不动那个帖」在架构层面成立，
// 前端漏判也不会误伤账号。
//
// 关于解锁：站点真实机制是「回复后再查看」（不是积分制，详见 internal/panfx/unlock.go）。
// 默认会以配置账号的身份在该帖下发一条预设快捷回复，从而拿到隐藏的网盘链接；
// 可用 ENV_PANFX_AUTO_REPLY=0 改为只读模式（只探测是否已解锁，绝不发帖）。

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mmbot/internal/panfx"
)

// 搜索缓存 10 分钟：同一部片在详情页反复进出不用重复打站点；
// 条目本身变化很慢（新发资源才会变），10 分钟已是保守值。
var panfxCache = newTTLCache(10*time.Minute, 64)

// panfxClient 按当前配置构造客户端（站点地址/Cookie 都可能被改，不做长驻实例）。
//
// 注意：回复节流状态（「哪些帖已回过」）存在 stateFile 里，每次新建实例都会读回来，
// 所以即便这里每次都新建客户端，也不会因为重启而重复回帖。
func panfxClient() *panfx.Client {
	c := panfx.New(
		envGet("ENV_PANFX_BASE", panfx.DefaultBase),
		envGet("ENV_PANFX_COOKIE", ""),
	)
	if d := envGet("ENV_PANFX_REPLY_GAP", ""); d != "" {
		if sec, err := strconv.Atoi(strings.TrimSpace(d)); err == nil && sec >= 0 {
			c.SetMinReplyGap(time.Duration(sec) * time.Second)
		}
	}
	c.SetStateFile(panfxStateFile())
	return c
}

// panfxStateFile 回复记录的落盘路径。
//
// 约定：user.env 位于 <root>/config/user.env 时，数据目录是 <root>/data。
// 但启动参数允许把 env 文件直接放在项目根下（调试时的 -env _e2e.env），
// 此时 filepath.Dir 返回 "."，若再往上跳一级就会写到项目的父目录里去。
// 判据用「env 同级是否有 config/ 或 data/ 目录」来区分这两种布局。
func panfxStateFile() string {
	envDir := filepath.Dir(configFilePath)
	if envDir == "" {
		envDir = "."
	}
	// 布局一：env 在项目根，data/ 与它同级
	if envDir == "." || !fileExists(filepath.Join(envDir, "..", "config")) {
		return filepath.Join(envDir, "data", "panfx_replied.json")
	}
	// 布局二：env 在 <root>/config/ 下，data/ 与 config/ 同级
	return filepath.Join(envDir, "..", "data", "panfx_replied.json")
}

// fileExists 判断路径是否存在（目录或文件都算）。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// panfxReplyEnabled 是否允许自动回复解锁（默认开启，可用 ENV_PANFX_AUTO_REPLY=0 关闭）。
func panfxReplyEnabled() bool {
	v := strings.TrimSpace(envGet("ENV_PANFX_AUTO_REPLY", "1"))
	return v != "0" && !strings.EqualFold(v, "false")
}

// handlePanfxSearch GET /api/panfx/search?keyword=xxx[&title=xxx][&year=2025]
//
// title/year 用于在站点关键词之外做一次本地相关性收敛：站点搜索是全文匹配，
// 搜「怪奇物语」会把标题里恰好出现该词的其它片子也带回来。
func (s *Server) handlePanfxSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(q.Get("title"))
	}
	if keyword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少搜索关键词"})
		return
	}

	client := panfxClient()
	// 匿名搜索；解锁能力单独上报，前端据此决定按钮提示文案
	unlockReady := client.Configured()

	cacheKey := "search:" + keyword
	val, _ := panfxCache.do(cacheKey, q.Get("refresh") == "1", func() (any, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		items, err := client.Search(ctx, keyword)
		if err != nil {
			log.Printf("[Panfx] 搜索 %q 失败：%v", keyword, err)
			return nil, false
		}
		// 空结果也缓存（负缓存 10 分钟）：站点偶发返回 0 条时，
		// 用户反复点搜索不应该每次都打一次上游。
		return items, true
	})

	items, _ := val.([]panfx.Item)
	if val == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           false,
			"error":        "资源站搜索失败，请稍后重试（可在「用户配置」检查站点地址）",
			"items":        []panfx.Item{},
			"unlock_ready": unlockReady,
			"site":         client.Base,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"keyword":      keyword,
		"items":        items,
		"total":        len(items),
		"unlock_ready": unlockReady,
		"site":         client.Base,
		// 站点搜索页地址：面板内没结果时给用户一条直达路径
		"search_url": client.Base + "/search-1.htm?keyword=" + urlQueryEscape(keyword),
	})
}

// handlePanfxUnlock POST /api/panfx/unlock  body: {"tid":"63977"}
//
// 只有用户点击条目右侧的「解锁」按钮才会走到这里 —— 发帖时机完全由用户控制，
// 后端不会在搜索或渲染详情页时顺手解锁。
//
// 默认走 Unlock（必要时以配置账号身份回一帖）；ENV_PANFX_AUTO_REPLY=0 时走
// UnlockReadOnly，只判断当前是否已可见，绝不发帖。
func (s *Server) handlePanfxUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TID string `json:"tid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.TID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少资源 id"})
		return
	}

	client := panfxClient()
	if !client.Configured() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "尚未配置资源站 Cookie，无法解锁。请在「用户配置 → 影视资源」填入 ENV_PANFX_COOKIE。",
		})
		return
	}

	// 回帖是网络写操作，比只读探测多给一点时间
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	autoReply := panfxReplyEnabled()
	var (
		res *panfx.UnlockResult
		err error
	)
	if autoReply {
		res, err = client.Unlock(ctx, body.TID)
	} else {
		res, err = client.UnlockReadOnly(ctx, body.TID)
	}
	if err != nil {
		log.Printf("[Panfx] 解锁 %s 失败：%v", body.TID, err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"result":     res,
		"auto_reply": autoReply,
	})
}

// urlQueryEscape 转义查询串（避免为了一处调用引入整包 net/url 的别名冲突）。
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		const hex = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
