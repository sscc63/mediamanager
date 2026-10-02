package web

// 频道更新 API：从频道监控消息库(TG_monitor-123.db)读取近段时间识别出标题的影视，
// 逐条经 TMDB 搜索补上海报/tmdb_id，供首页「频道更新」横滑条与订阅探索页海报网格展示。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"mmbot/internal/bot"
	"mmbot/internal/transfer"
)

// channelRecentHours 频道更新默认展示窗口：近 24 小时。
//
// 频道消息是「更新/连载」性质的，超过一天的基本已经被后续集数顶下去了，
// 留着只会让海报墙越积越长、且多为过期集。窗口统一在服务端兜底（见 handleChannelRecent），
// 前端传什么都不会超出这个上限，避免以后有人改前端参数又把过期内容放回来。
const channelRecentHours = 24

// channelRecentMaxHours 前端可请求的最大窗口。留一点余量给「看更久以前的更新」这类需求，
// 但不能无限放大 —— 该接口每条都要打一次 TMDB 搜索补海报。
const channelRecentMaxHours = 72

// channelAIMaxPerRequest 单次请求最多做几次 AI 标题兜底。该接口是前端同步调用的，
// 每条 AI 要 1~3 秒，不设上限会让首次刷新明显卡住；剩下的留给下次请求（已处理的走缓存）。
const channelAIMaxPerRequest = 3

// handleChannelRecent GET /api/monitor/recent?hours=24&limit=30
// hours 默认 24（见 channelRecentHours），超过 channelRecentMaxHours 会被夹到上限。
func (s *Server) handleChannelRecent(w http.ResponseWriter, r *http.Request) {
	hours := intQuery(r, "hours", channelRecentHours)
	if hours > channelRecentMaxHours {
		hours = channelRecentMaxHours
	}
	limit := intQuery(r, "limit", 30)

	msgDB := bot.NewMessageDB("")
	rows := msgDB.ListRecentWithTitle(hours, limit)

	// TMDB 未配置时只回标题（前端用占位海报）
	var client *transfer.TmdbClient
	if tmdbAPIKey() != "" {
		client = tmdbClient()
	}

	items := make([]map[string]any, 0, len(rows))
	seen := map[string]bool{}
	aiBudget := channelAIMaxPerRequest // 本次请求剩余的 AI 兜底次数
	for _, c := range rows {
		key := normTitle(c.Title) + "|" + strings.TrimSpace(c.Year) // 归一化标题+年份去重，保留最新一条
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		item := map[string]any{
			"title":         c.Title,
			"year":          c.Year,
			"media_type":    c.Type,
			"tmdb_id":       0,
			"poster_path":   "",
			"message_url":   c.MessageURL,
			"transfer_time": c.TransferTime,
			"target_url":    c.TargetURL,
		}
		if client != nil {
			// 只固化了 id 的老数据不能信：类型可能是空或频道自报的错值，要重搜补齐并写回。
			if c.TmdbID > 0 && validMediaType(c.Type) {
				// 库内已持久化：直接读回，不再实时搜索（首次命中时已固化）
				item["tmdb_id"] = c.TmdbID
				item["poster_path"] = c.PosterPath
			} else if hit := channelMatch(client, c, &aiBudget); hit != nil {
				item["tmdb_id"] = hit.ID
				item["poster_path"] = hit.PosterPath
				// 一律采用命中结果自带的类型：tmdb_id 与类型必须配对，否则前端会拿
				// /movie/<剧集id> 去查详情（电影与剧的 id 是两套独立空间），显示成无关作品。
				// 识别出的类型为空时 channelSearchFirst 是 movie+tv 都搜，命中很可能是剧集。
				item["media_type"] = hit.MediaType
				// 写回库实现持久化：下次直接读库，且 24h 清理整行删除即随之过期
				msgDB.UpdateTMDBColumns(c.MessageURL, hit.ID, hit.PosterPath, hit.MediaType)
			} else if c.TmdbID > 0 {
				// 重搜没命中：保留已有 id 与海报，不因类型缺失丢掉条目。
				item["tmdb_id"] = c.TmdbID
				item["poster_path"] = c.PosterPath
			} else {
				// 识别/匹配失败（找不到对应 TMDB 条目）：不返回，避免在前端展示无海报的「幽灵」占位。
				// 这类记录堆积在 24h 窗口里只会让海报墙越拉越长且大多是识别错的标题。
				continue
			}
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hours":      hours,
		"total":      len(items),
		"configured": tmdbAPIKey() != "",
		"items":      items,
	})
}

// handleChannelTransfer POST /api/monitor/transfer；body {"target_url":"..."}
// 长按「频道更新」卡片「入库」：把该分享链接立即转存到库（复用手动转存链路，自带 TG 通知与去重）。
func (s *Server) handleChannelTransfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetURL string `json:"target_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TargetURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "target_url required"})
		return
	}
	ok := s.bot.RestoreTransferLink(req.TargetURL)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}

// validMediaType 判断类型能否用于配 tmdb_id（频道自报的值可能为空或标错）。
func validMediaType(t string) bool { return t == "movie" || t == "tv" }

// intQuery 解析整型查询参数，非法或非正数时用默认值。
func intQuery(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// normTitle 归一化片名用于去重：转小写、去掉所有标点与空白。
func normTitle(s string) string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	return strings.Join(fields, "")
}

// channelAlias 频道社区译名 → TMDB 可命中名称。
// 部分剧 TMDB 简体中文没有对应译名（如「流人」在 zh-CN 无中文名，官方为 Slow Horses），
// 直接搜中文会错配到含同字的无关作品，需人工映射后按其原名搜索。
var channelAlias = map[string]string{
	"流人": "Slow Horses",
}

// channelMatch 用识别出的标题搜 TMDB；搜不到时交给 AI 还原标题，再用还原名搜一次。
// aiBudget 是本次请求剩余的 AI 兜底次数，实际发起调用才扣减。
func channelMatch(client *transfer.TmdbClient, c bot.ChannelRecent, aiBudget *int) *transfer.TmdbListItem {
	if hit := channelSearchFirst(client, c.Title, c.Year, c.Type); hit != nil {
		return hit
	}
	if aiBudget == nil || *aiBudget <= 0 {
		return nil
	}
	ai := aiClient()
	if ai == nil {
		return nil
	}
	*aiBudget--
	year, _ := strconv.Atoi(c.Year)
	// 缓存键用归一化标题：该接口已按「标题+年份」去重，同一标题只会调一次。
	r := ai.Recognize("ch|"+normTitle(c.Title), c.Title, "", year)
	if r == nil || !r.Valid || r.Title == "" {
		return nil
	}
	y := c.Year
	if r.Year > 0 {
		y = strconv.Itoa(r.Year)
	}
	return channelSearchFirst(client, r.Title, y, c.Type)
}

// channelSearchFirst 用标题搜索 TMDB，最多 limit 条。
// 优先「标题严格按影视名精确匹配」，其次「发布日期年份与识别年份一致」，二者皆不命中则返回空（不回退错配）。
func channelSearchFirst(client *transfer.TmdbClient, title, year, mtype string) *transfer.TmdbListItem {
	if a, ok := channelAlias[normTitle(title)]; ok && a != "" {
		title = a
	}
	// 识别出的类型只作「优先」，不作「排他」。
	//
	// 消息里的类型来自频道自己写的「🎭 类型：X剧/X电影」字段，本身并不可靠：
	// 实测 A子计划(1986)、93航班(2006)、鬼上车(2026) 都被标成 tv，而它们在 TMDB
	// 是电影 —— 旧逻辑 case "tv" 只搜 /search/tv，必然 0 条，于是这条记录永远
	// 匹配不上、每次请求都白打一次搜索（与脏标题同一机制，都会让接口耗时线性增长）。
	// 先搜识别出的类型，没有可用命中再拿另一类型兜底：命中结果自带 media_type，
	// 调用方据此决定详情走哪个接口，所以兜底命中的电影不会被当成剧集展示。
	var order []string
	switch mtype {
	case "tv":
		order = []string{"tv", "movie"}
	case "movie":
		order = []string{"movie", "tv"}
	default:
		order = []string{"movie", "tv"}
	}
	// 汇总所有类型的候选，做一次统一的年份优先、再带海报回退。
	// 必须把两种类型的结果全部收齐再统一裁决：若按类型顺序「先搜到就返回」，
	// 次类型里更准确的同名/同年份条目会被主类型里的错配结果抢先。
	var all []transfer.TmdbListItem
	for _, t := range order {
		key := "channel:" + title + ":" + t
		if v, ok := listCache.get(key); ok {
			if items, ok := v.([]transfer.TmdbListItem); ok {
				all = append(all, items...)
				continue
			}
		}
		items := client.SearchMedia(title, t, 20)
		if len(items) > 0 {
			listCache.set(key, items)
			all = append(all, items...)
		}
	}
	return pickChannelMatch(all, title, year, mtype)
}

// pickChannelMatch 从候选里挑出最合适的一条（纯函数，便于单测）。
// mtype 是识别出的类型，只用于「同分时优先」，不作排他 —— 见 channelSearchFirst 的说明。
func pickChannelMatch(all []transfer.TmdbListItem, title, year, mtype string) *transfer.TmdbListItem {
	want := normTitle(title)
	// 优先①：标题严格按影视名匹配（忽略大小写与标点空白）且带海报，且类型与识别结果一致。
	// 先按识别类型筛一轮，避免 movie/tv 同名作品串味（同名电影的 id 与剧集是两套独立空间，
	// 拿错会让详情页显示成无关作品）。
	for i := range all {
		if all[i].PosterPath != "" && all[i].MediaType == mtype && normTitle(all[i].Title) == want {
			return &all[i]
		}
	}
	// 优先②：标题严格匹配且带海报，不限类型（识别出的类型本身可能是错的）
	for i := range all {
		if all[i].PosterPath != "" && normTitle(all[i].Title) == want {
			return &all[i]
		}
	}
	// 优先③：发布日期年份与识别年份一致且带海报，优先与识别类型一致者
	if year != "" {
		for i := range all {
			if all[i].PosterPath != "" && all[i].MediaType == mtype && strings.HasPrefix(all[i].ReleaseDate, year) {
				return &all[i]
			}
		}
		for i := range all {
			if all[i].PosterPath != "" && strings.HasPrefix(all[i].ReleaseDate, year) {
				return &all[i]
			}
		}
	}
	// 严格按影视名/年份都匹配不到：返回空占位，绝不回退到无关的同名/含字作品（避免错图）
	return nil
}
