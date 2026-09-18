package web

import (
	"net/http/httptest"
	"testing"

	"mmbot/internal/transfer"
)

// TestPickChannelMatchTypeFallback 验证识别出的类型只作「优先」不作「排他」。
//
// 背景：频道消息的类型来自频道自己写的「🎭 类型：X剧/X电影」字段，并不可靠。
// NAS 实测 A子计划(1986)、93航班(2006)、鬼上车(2026) 都被标成 tv，而它们在 TMDB
// 是电影。旧逻辑 case "tv" 只搜 /search/tv 必然 0 条，于是这几条记录永远匹配不上，
// 每次打开频道更新都白打一次失败搜索（实测 20 条这类记录把 /api/monitor/recent
// 第二次请求从 0.39s 拖到 17.43s）。下面用的候选数据全部来自 NAS 上的真实 TMDB 返回。
func TestPickChannelMatchTypeFallback(t *testing.T) {
	item := func(title, mtype, poster, date string, id int) transfer.TmdbListItem {
		return transfer.TmdbListItem{ID: id, Title: title, MediaType: mtype, PosterPath: poster, ReleaseDate: date}
	}

	cases := []struct {
		name    string
		cands   []transfer.TmdbListItem
		title   string
		year    string
		mtype   string
		wantID  int
		wantNil bool
	}{
		{
			// 类型被误标成 tv，实际是电影：必须兜底到 movie
			name:   "类型误标 tv 兜底命中电影",
			cands:  []transfer.TmdbListItem{item("A子计划", "movie", "/p.jpg", "1986-01-01", 42025)},
			title:  "A子计划", year: "1986", mtype: "tv", wantID: 42025,
		},
		{
			name:   "93航班 类型误标 tv 兜底命中电影",
			cands:  []transfer.TmdbListItem{item("93航班", "movie", "/p.jpg", "2006-01-01", 9829)},
			title:  "93航班", year: "2006", mtype: "tv", wantID: 9829,
		},
		{
			name: "鬼上车 类型误标 tv：同类型错配不与兜底电影抢",
			cands: []transfer.TmdbListItem{
				item("鬼上你架车", "tv", "/wrong.jpg", "2021-01-01", 320136),
				item("鬼上车", "movie", "/right.jpg", "2026-01-01", 1368314),
			},
			title: "鬼上车", year: "2026", mtype: "tv", wantID: 1368314,
		},
		{
			// 类型标对时不得被另一类型的同名结果抢走
			name: "类型正确时不串味",
			cands: []transfer.TmdbListItem{
				item("名侦探柯南", "tv", "/tv.jpg", "1996-01-01", 30984),
				item("名侦探柯南：独眼的残像", "movie", "/mv.jpg", "2025-01-01", 99999),
			},
			title: "名侦探柯南", year: "1996", mtype: "tv", wantID: 30984,
		},
		{
			// 标题精确匹配优先于年份匹配：同类型里年份对不上的也要让位给标题对的
			name: "标题精确匹配优先于年份",
			cands: []transfer.TmdbListItem{
				item("玫瑰丛生", "tv", "/a.jpg", "2026-01-01", 240446),
				item("生逢其时", "tv", "/b.jpg", "2026-01-01", 286686),
			},
			title: "生逢其时", year: "2026", mtype: "tv", wantID: 286686,
		},
		{
			// 无海报的候选不可用，宁可返回 nil 也不给占位错图
			name:    "无海报不回退",
			cands:   []transfer.TmdbListItem{item("A子计划", "movie", "", "1986-01-01", 42025)},
			title:   "A子计划", year: "1986", mtype: "tv", wantNil: true,
		},
		{
			// 标题与年份都对不上：不许回退到含同字的无关作品
			name: "同名含字不回退错配",
			cands: []transfer.TmdbListItem{
				item("精子A计划", "movie", "/p.jpg", "1992-01-01", 34756),
				item("A子计划5：战之灰", "movie", "/p.jpg", "1990-01-01", 134832),
			},
			title: "A子计划", year: "1986", mtype: "movie", wantNil: true,
		},
		{
			// 类型为空（消息里没有类型字段）：movie/tv 都应在候选里被裁决
			name:   "类型为空时两类型都可命中",
			cands:  []transfer.TmdbListItem{item("牧神记", "tv", "/p.jpg", "2024-01-01", 101172)},
			title:  "牧神记", year: "2024", mtype: "", wantID: 101172,
		},
	}

	for _, c := range cases {
		got := pickChannelMatch(c.cands, c.title, c.year, c.mtype)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s: 期望 nil，实际命中 id=%d (%s)", c.name, got.ID, got.Title)
			} else {
				t.Logf("OK   %s -> nil", c.name)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: 期望命中 id=%d，实际 nil（类型兜底失效）", c.name, c.wantID)
			continue
		}
		if got.ID != c.wantID {
			t.Errorf("%s: 命中 id=%d (%s/%s)，期望 id=%d", c.name, got.ID, got.Title, got.MediaType, c.wantID)
			continue
		}
		t.Logf("OK   %-28s -> %s [%s] id=%d", c.name, got.Title, got.MediaType, got.ID)
	}
}

func TestChannelRecentHoursClamp(t *testing.T) {
	// handleChannelRecent 的前置参数处理（不触发 DB/TMDB 查询的部分）
	cases := []struct {
		query    string
		wantHour int
	}{
		{"", channelRecentHours},                 // 不带参数 -> 24
		{"?hours=24", 24},
		{"?hours=1", 1},
		{"?hours=180", channelRecentMaxHours},    // 旧前端的 180 被夹到上限（不再放任 7.5 天）
		{"?hours=9999", channelRecentMaxHours},   // 超大值夹到上限
		{"?hours=0", channelRecentHours},         // 非正数 -> 回落默认 24
		{"?hours=-5", channelRecentHours},        // 负数 -> 回落默认 24
		{"?hours=abc", channelRecentHours},       // 非法 -> 回落默认 24
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/monitor/recent"+c.query, nil)
		hours := intQuery(r, "hours", channelRecentHours)
		if hours > channelRecentMaxHours {
			hours = channelRecentMaxHours
		}
		if hours != c.wantHour {
			t.Errorf("query=%q -> hours=%d, want %d", c.query, hours, c.wantHour)
		} else {
			t.Logf("OK   query=%-14q -> hours=%d", c.query, hours)
		}
	}
	if channelRecentHours != 24 {
		t.Errorf("默认窗口应为 24 小时，实际 %d", channelRecentHours)
	}
}
