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

// handleChannelRecent GET /api/monitor/recent?hours=3&limit=30
func (s *Server) handleChannelRecent(w http.ResponseWriter, r *http.Request) {
	hours := intQuery(r, "hours", 3)
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
			if hit := channelSearchFirst(client, c.Title, c.Type); hit != nil {
				item["tmdb_id"] = hit.ID
				item["poster_path"] = hit.PosterPath
				if item["media_type"] == "" {
					item["media_type"] = "movie" // 未识别类型时按 movie 展示，详情页据 tmdb 结果可再细分
				}
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

// channelSearchFirst 用标题搜索 TMDB，返回第一条带海报的结果；类型不确定时先 movie 后 tv。
func channelSearchFirst(client *transfer.TmdbClient, title, mtype string) *transfer.TmdbListItem {
	order := []string{"tv"}
	if mtype == "tv" {
		order = []string{"tv"}
	} else if mtype == "movie" {
		order = []string{"movie"}
	} else {
		order = []string{"movie", "tv"}
	}
	for _, t := range order {
		key := "channel:" + title + ":" + t
		var items []transfer.TmdbListItem
		if v, ok := listCache.get(key); ok {
			items, _ = v.([]transfer.TmdbListItem)
		} else {
			items = client.SearchMedia(title, t)
			if len(items) > 0 {
				listCache.set(key, items)
			}
		}
		for i := range items {
			if items[i].PosterPath != "" {
				return &items[i]
			}
		}
	}
	return nil
}