package panfx

import "testing"

// 解析相关的回归测试：站点模板改版时这几条会先失败，比线上搜出乱数据早一步发现。
// 用例取自 2026-09 真实页面的片段（已裁剪为最小的结构骨架）。

func TestParseSearch(t *testing.T) {
	// 两块：一个正常影视帖（含标签/体积/作者/日期/评论数）+ 一个短剧噪声帖
	html := `
<ul class="list-unstyled threadlist mb-0">
	<li class="media thread tap " data-href="?thread-63977.htm" data-tid="63977">
		<a href="?user-32702.htm" class="username mr-1">作者A</a>
		<div class="media-body">
			<div class="style3_subject break-all">
				<a href="?thread-63977.htm"><span class="text-danger">怪奇物语</span>1-5季 4K杜比/1080P全集 484.45GB</a>
				<a href="?forum-48-1.htm&tagids=24" class="badge">1080p</a>
				<a href="?forum-48-1.htm&tagids=26" class="badge">4K</a>
			</div>
			<a href="?user-32702.htm" class="username text-muted mr-1 hidden-sm" uid="32702">劳白沙</a>
			<span class="date text-grey hidden-sm">2026-08-07 23:26</span>
			<a href="?thread-63977.htm#comments" class="ml-2"><span><span class="far fa-comment-dots"></span><span> 34</span></span></a>
		</div>
	</li>
	<li class="media thread tap " data-href="?thread-68030.htm" data-tid="68030">
		<div class="media-body">
			<div class="style3_subject break-all">
				<a href="?thread-68030.htm">短剧《徒儿，你无敌了》61集 720P 1.08G</a>
				<a href="?forum-59-1.htm&tagids=1" class="badge">短剧</a>
			</div>
			<span class="date text-grey hidden-sm">16小时前</span>
		</div>
	</li>
</ul>`

	items := parseSearch(html, "https://www.123panfx.com")
	if len(items) != 1 {
		t.Fatalf("短剧噪声应被剔除，期望 1 条，实际 %d 条", len(items))
	}
	it := items[0]
	if it.TID != "63977" {
		t.Errorf("tid = %q, 期望 63977", it.TID)
	}
	if it.Title != "怪奇物语 1-5季 4K杜比/1080P全集 484.45GB" {
		t.Errorf("title = %q", it.Title)
	}
	if it.URL != "https://www.123panfx.com/thread-63977.htm" {
		t.Errorf("url = %q", it.URL)
	}
	if it.Size != "484.45GB" {
		t.Errorf("size = %q, 期望 484.45GB", it.Size)
	}
	if len(it.Tags) != 2 || it.Tags[0] != "1080p" || it.Tags[1] != "4K" {
		t.Errorf("tags = %v", it.Tags)
	}
	if it.Date != "2026-08-07 23:26" {
		t.Errorf("date = %q", it.Date)
	}
	if it.Comments != 34 {
		t.Errorf("comments = %d, 期望 34", it.Comments)
	}
}

// 求资源帖没有资源链接，混在列表里只会浪费点击，必须剔除。
func TestParseSearchSkipsRequestPosts(t *testing.T) {
	html := `
<li class="media thread" data-href="?thread-50212.htm" data-tid="50212">
	<div class="media-body">
		<div class="style3_subject"><a href="?thread-50212.htm">求怪奇物语第五季4k</a>
			<a href="?forum-48-1.htm&tagids=1">求资源</a>
			<a href="?forum-48-1.htm&tagids=2">未解决</a>
		</div>
	</div>
</li>`
	if items := parseSearch(html, "https://x"); len(items) != 0 {
		t.Fatalf("求资源帖应被剔除，实际 %d 条: %+v", len(items), items)
	}
}

// 体积抽取：要挡住分辨率误判（1080P、2160p），并归一化单位。
func TestExtractSize(t *testing.T) {
	cases := map[string]string{
		"怪奇物语 1-5季 4K杜比/1080P全集已完结 484.45GB":   "484.45GB",
		"Netflix《怪奇物语》全5季 「1.67TB」":            "1.67TB",
		"疯狂动物城 2 4K DVp5 双语字幕 20g「附第一部 50g」":     "50GB",
		"F1：狂飙飞车 外挂字幕 4K TRUEHD":                "",   // 无体积
		"某剧 1080P 2160p":                          "",   // 只有分辨率，不该误判成体积
		"短剧《X》（61集）（720P）2026【1.08G】":             "1.08GB",
		"电影 2160p REMUX DV HDR10 「36.57GB」 内封":     "36.57GB",
		"某片 637g 已刮削":                             "637GB",
		"小文件 300M":                               "300MB",
	}
	for in, want := range cases {
		if got := extractSize(in); got != want {
			t.Errorf("extractSize(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 「F1 狂飙飞车」这类中英混排片名整串在站点上是 0 条（字面子串匹配），
// 必须能回落到拆词探测，否则前端永远搜不到。
func TestKeywordProbes(t *testing.T) {
	got := keywordProbes("F1 狂飙飞车")
	if len(got) < 2 {
		t.Fatalf("应生成多次探测，实际 %v", got)
	}
	if got[0] != "F1 狂飙飞车" {
		t.Errorf("首次探测应是整串，实际 %q", got[0])
	}
	found := false
	for _, g := range got {
		if g == "狂飙飞车" {
			found = true
		}
	}
	if !found {
		t.Errorf("应包含拆出的「狂飙飞车」，实际 %v", got)
	}
	if len(got) > 3 {
		t.Errorf("探测次数应受限（最多 3 次），实际 %d: %v", len(got), got)
	}
}

func TestExtractLinks(t *testing.T) {
	html := `<div class="message"><p>下载：<a href="https://www.123pan.com/s/abcde-fghi?pwd=ABCD">点我</a>
		夸克：https://pan.quark.cn/s/xyz123
		密码：WXYZ</p></div>`
	links := extractLinks(html)
	if len(links) < 2 {
		t.Fatalf("应抽出至少 2 条链接，实际 %d: %+v", len(links), links)
	}
	if links[0].Pan != "123" || links[0].Pwd != "ABCD" {
		t.Errorf("123 链接解析异常: %+v", links[0])
	}
	if links[1].Pan != "夸克" {
		t.Errorf("夸克链接解析异常: %+v", links[1])
	}
}
