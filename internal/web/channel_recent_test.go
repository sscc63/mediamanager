package web

import (
	"net/http/httptest"
	"testing"
)

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
