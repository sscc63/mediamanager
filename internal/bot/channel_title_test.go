package bot

import (
	"strings"
	"testing"
)

func TestChannelCleanHeadStripsLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"剧集：成也萧河 (2026) S01E09", "成也萧河"},
		{"电影：A子计划 (2026)", "A子计划"},
		{"电影：超时空辉夜姬！ (2026)", "超时空辉夜姬！"},
		{"剧集：骸骨骑士大人异世界冒险中 (2022) S02E11", "骸骨骑士大人异世界冒险中"},
		{"剧集：炼气十万年 (2023) S01E37", "炼气十万年"},
		{"剧集：待到重逢时 (2019) S01E02", "待到重逢时"},
		{"剧集：微风襟袖同卿心 (2026) S01E01", "微风襟袖同卿心"},
		{"剧集：朱丽叶与朱丽叶 (2026) S01E02", "朱丽叶与朱丽叶"},
		{"动漫：某片 (2020)", "某片"},
		{"纪录片：某片 (2020)", "某片"},
		{"电视剧：某剧 (2020)", "某剧"},
		{"美剧：某剧 (2020) S01E01", "某剧"},
		{"剧集 : 空格冒号 (2020)", "空格冒号"},
		{"剧集:半角冒号 (2020)", "半角冒号"},
		// —— 反例：标题自身含冒号/季集字样，绝不能被削 ——
		{"名侦探柯南：独眼的残像 (2025)", "名侦探柯南：独眼的残像"},
		{"流浪地球：飞跃2020特别版 (2020)", "流浪地球：飞跃2020特别版"},
		{"鬼灭之刃：无限城篇 (2025)", "鬼灭之刃：无限城篇"},
		{"SE7EN 七宗罪 (1995)", "SE7EN 七宗罪"},
		{"S1m0ne 虚拟偶像 (2002)", "S1m0ne 虚拟偶像"},
	}
	for _, c := range cases {
		got := channelCleanHead(c.in)
		if got != c.want {
			t.Errorf("channelCleanHead(%q) = %q, want %q", c.in, got, c.want)
		} else {
			t.Logf("OK   in=%-46q got=%q", c.in, got)
		}
	}
}

func TestRecognizeChannelTitleNoLabelLeak(t *testing.T) {
	msgs := []string{
		"剧集：成也萧河 (2026) S01E09  \n简介  \n正文……\n\nTMDB评分：0.0/10  \n播出日期：2026-08-03  \n分类：动漫  \n类型：#成也萧河 #动画  \n版本：2160p.WEB-DL.H265.AAC-UBWEB  \n体积：590.03 MB  \n链接：https://x.share.123pan.cn/123pan/iWcfvd-MMZ1H",
		"剧集：骸骨骑士大人异世界冒险中 (2022) S02E11  \n简介\n正文\n\n播出日期：2022-04-07  \n分类：动漫  \n链接：https://x",
		"电影：A子计划 (2026)  \n简介\n正文\n\n播出日期：2026-01-01  \n分类：电影  \n链接：https://x",
		"剧集：警与囚 (2021) S01E01  \n简介\n正文\n\n播出日期：2021-06-06  \n链接：https://x",
		"剧集：微风襟袖同卿心 (2026) S01E01  \n简介\n正文\n\n播出日期：2026-09-08  \n链接：https://x",
	}
	for _, m := range msgs {
		title, year, mt := recognizeChannelTitle(m)
		t.Logf("title=%-32q year=%-6q type=%q", title, year, mt)
		if title == "" {
			t.Errorf("识别出的标题为空，消息=%q", m)
			continue
		}
		// 标签/季集不得残留在标题里
		for _, bad := range []string{"剧集", "电影：", "动漫：", "S0", "S1E", "S02E", "S01E"} {
			if strings.Contains(title, bad) {
				t.Errorf("标题 %q 残留了 %q，消息=%q", title, bad, m)
			}
		}
	}
}
