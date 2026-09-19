package bot

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNormalizeMsgTime 覆盖 t.me 的 RFC3339 发布时间归一化。
//
// 这是「24h 窗口」的判定基准：此前窗口与清理都用 recognized_at（扫描时刻），
// 导致频道页上几天前的历史帖子每轮扫描都被刷新成「刚更新」，在频道更新里滞留 N+1 天。
func TestNormalizeMsgTime(t *testing.T) {
	cases := []struct {
		in   string
		want string
		desc string
	}{
		// t.me/s/ 实际下发格式：带 +00:00，必须按 UTC 解析后转本地，而不是当裸本地时间。
		{"2026-09-16T14:58:29+00:00", time.Date(2026, 9, 16, 14, 58, 29, 0, time.UTC).Local().Format("2006-01-02T15:04:05"), "UTC 转为本地"},
		{"2026-08-31T10:54:38+00:00", time.Date(2026, 8, 31, 10, 54, 38, 0, time.UTC).Local().Format("2006-01-02T15:04:05"), "更早的日期"},
		// 其他时区偏移也要正确换算
		{"2026-09-16T22:58:29+08:00", time.Date(2026, 9, 16, 14, 58, 29, 0, time.UTC).Local().Format("2006-01-02T15:04:05"), "东八区偏移"},
		// 无时区后缀的旧数据按本地时间理解
		{"2026-09-16T14:58:29", "2026-09-16T14:58:29", "无时区按本地"},
		// 缺失/非法一律留空，由调用方回退
		{"", "", "空串"},
		{"   ", "", "纯空白"},
		{"不是时间", "", "非法输入"},
		{"2026-09-16", "", "只有日期"},
	}
	for _, c := range cases {
		if got := normalizeMsgTime(c.in); got != c.want {
			t.Errorf("normalizeMsgTime(%q) [%s] = %q, want %q", c.in, c.desc, got, c.want)
		}
	}
}

// TestChannelRecentWindowUsesPublishTime 端到端验证窗口以「消息发布时间」为准。
//
// 构造两条记录：
//   - 老帖：7 天前发布，但今天才被扫描入库（recognized_at=now）——旧逻辑会把它当成新鲜内容；
//   - 新帖：1 小时前发布，同样今天入库。
//
// 期望窗口内只剩新帖，且清理只删老帖。
func TestChannelRecentWindowUsesPublishTime(t *testing.T) {
	db := openTempMessageDB(t)

	now := time.Now()
	oldPublish := now.Add(-7 * 24 * time.Hour)
	newPublish := now.Add(-1 * time.Hour)

	// 两条都是「现在」被扫描到的
	db.Save("1", oldPublish.Format(time.RFC3339), "https://t.me/c/1", "", "老片子", "2019", "movie", "转存成功", "ok", 0)
	db.Save("2", newPublish.Format(time.RFC3339), "https://t.me/c/2", "", "新片子", "2026", "movie", "转存成功", "ok", 0)

	rows := db.ListRecentWithTitle(24, 30)
	if len(rows) != 1 {
		t.Fatalf("24h 窗口应只剩 1 条（新帖），实际 %d 条: %+v", len(rows), titles(rows))
	}
	if rows[0].Title != "新片子" {
		t.Errorf("窗口内应为「新片子」，实际 %q", rows[0].Title)
	}

	// 清理应删掉老帖、保留新帖
	db.CleanupRecentHours(24)
	rest := db.ListRecentWithTitle(24, 30)
	if len(rest) != 1 || rest[0].Title != "新片子" {
		t.Errorf("清理后应只剩「新片子」，实际 %+v", titles(rest))
	}
	if got := countAll(t, db); got != 1 {
		t.Errorf("清理后表内应剩 1 行，实际 %d 行", got)
	}
}

// TestBackfillUseDate 老库补 usedate 列后必须回填，否则过期条目会靠 COALESCE 回退
// 一直赖在 24h 窗口里（实测升级前入库的 66 条全部如此，最早的是 12 天前发布）。
func TestBackfillUseDate(t *testing.T) {
	dir, err := os.MkdirTemp("", "mmbot-backfill-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	path := filepath.Join(dir, "t.db")

	// 先手工建一张「没有 usedate 列」的旧表，模拟升级前的库
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开裸库失败: %v", err)
	}
	_, err = raw.Exec(`CREATE TABLE messages (
		msg_id INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT, date TEXT, message_url TEXT,
		target_url TEXT, transfer_status TEXT, transfer_time TEXT, transfer_result TEXT,
		media_title TEXT, media_year TEXT, media_type TEXT, recognized_at TEXT, target_pid INTEGER)`)
	if err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	now := time.Now()
	oldPublish := now.Add(-12 * 24 * time.Hour)  // 12 天前发布
	recentPublish := now.Add(-1 * time.Hour)     // 1 小时前发布
	recog := now.Format("2006-01-02T15:04:05")   // 但都是「刚刚」入库的
	ins := `INSERT INTO messages (date, message_url, transfer_status, transfer_time, media_title, recognized_at)
		VALUES (?, ?, '转存成功', ?, ?, ?)`
	if _, err := raw.Exec(ins, oldPublish.Format(time.RFC3339), "https://t.me/c/1", recog, "老片子", recog); err != nil {
		t.Fatalf("插入老帖失败: %v", err)
	}
	if _, err := raw.Exec(ins, recentPublish.Format(time.RFC3339), "https://t.me/c/2", recog, "新片子", recog); err != nil {
		t.Fatalf("插入新帖失败: %v", err)
	}
	// 一条 date 为空的（解析不了），回填后应保持为空、继续走回退。
	// 这条同时把 target_url 留成 NULL：ListRecentWithTitle 必须 COALESCE 掉它，
	// 否则 Scan 报错后该行会被静默跳过，表现为「条目莫名不出现在频道更新里」。
	if _, err := raw.Exec(ins, "", "https://t.me/c/3", recog, "无时间戳", recog); err != nil {
		t.Fatalf("插入空 date 失败: %v", err)
	}
	raw.Close()

	// 经由 NewMessageDB 打开 → 补列 + 自动回填
	db := NewMessageDB(path)
	if db == nil || db.db == nil {
		t.Fatal("打开库失败")
	}

	var filled int
	if err := db.db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE usedate <> ''").Scan(&filled); err != nil {
		t.Fatalf("统计回填失败: %v", err)
	}
	if filled != 2 {
		t.Errorf("应回填 2 条（老帖+新帖），实际 %d 条", filled)
	}

	// 关键断言：老帖回填后必须被逐出 24h 窗口
	got := titles(db.ListRecentWithTitle(24, 30))
	if len(got) != 2 {
		t.Fatalf("窗口应剩 2 条（新帖 + 无时间戳回退），实际 %v", got)
	}
	found := map[string]bool{}
	for _, g := range got {
		found[g] = true
	}
	if found["老片子"] {
		t.Errorf("12 天前发布的老帖回填后不应在窗口内，实际 %v", got)
	}
	if !found["新片子"] {
		t.Errorf("1 小时前的新帖应保留，实际 %v", got)
	}
	if !found["无时间戳"] {
		t.Errorf("date 为空的记录应靠回退保留，实际 %v", got)
	}

	// 重复调用不应重复回填（幂等）
	db.backfillUseDate()
	var again int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM messages WHERE usedate <> ''").Scan(&again); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if again != 2 {
		t.Errorf("回填应幂等，实际 %d 条", again)
	}
}

// TestChannelRecentOrderedByPublishTime 窗口内应按「真实发布时间」倒序，而不是入库时刻。
//
// 一轮扫描会把频道页整页消息在同一分钟内全部入库，transfer_time 几乎没有区分度；
// 若按它排序，真实发布时间被打乱，旧帖甚至会挤掉新帖占用的 limit 名额。
func TestChannelRecentOrderedByPublishTime(t *testing.T) {
	db := openTempMessageDB(t)
	now := time.Now()

	// 故意按「入库先后」与「真实发布先后」相反的顺序写入
	order := []struct {
		id      string
		title   string
		publish time.Time
	}{
		{"1", "最老片", now.Add(-20 * time.Hour)},
		{"2", "次新片", now.Add(-5 * time.Hour)},
		{"3", "最新片", now.Add(-1 * time.Hour)},
	}
	for _, o := range order {
		db.Save(o.id, o.publish.Format(time.RFC3339), "https://t.me/c/"+o.id, "", o.title, "2026", "movie", "转存成功", "ok", 0)
	}

	got := titles(db.ListRecentWithTitle(24, 30))
	want := []string{"最新片", "次新片", "最老片"}
	if len(got) != len(want) {
		t.Fatalf("返回 %d 条，期望 %d 条: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 位 = %q，期望 %q（完整结果: %v）", i, got[i], want[i], got)
		}
	}
}

// TestChannelRecentFallsBackToRecognizedAt 旧记录 usedate 为空时回退到 recognized_at，
// 不能因为新列没值就被整批漏掉或误删。
func TestChannelRecentFallsBackToRecognizedAt(t *testing.T) {
	db := openTempMessageDB(t)

	now := time.Now()
	// 发布时间缺失（parseTgmeMessage 没拿到 <time datetime>）：Save 传空
	db.Save("1", "", "https://t.me/c/1", "", "无时间戳片子", "2026", "movie", "转存成功", "ok", 0)
	// 老帖：7 天前发布
	db.Save("2", now.Add(-7*24*time.Hour).Format(time.RFC3339), "https://t.me/c/2", "", "老片子", "2019", "movie", "转存成功", "ok", 0)

	if got := db.ListRecentWithTitle(24, 30); len(got) != 1 || got[0].Title != "无时间戳片子" {
		t.Errorf("usedate 为空应回退 recognized_at 保留，实际 %+v", titles(got))
	}
	db.CleanupRecentHours(24)
	if got := db.ListRecentWithTitle(24, 30); len(got) != 1 || got[0].Title != "无时间戳片子" {
		t.Errorf("清理后应保留无时间戳的记录，实际 %+v", titles(got))
	}
}

// openTempMessageDB 每个用例独立临时库。MessageDB 按路径缓存且无 Close（单例供
// Web 与扫描共用），所以靠唯一路径隔离，不去动共享缓存。
// 用 MkdirTemp 而非 t.TempDir()：连接一直开着，t.TempDir 的自动清理在 Windows 上
// 会因文件被占用而失败（用例本身已通过，却被清理阶段的报错判成 FAIL）。
func openTempMessageDB(t *testing.T) *MessageDB {
	t.Helper()
	dir, err := os.MkdirTemp("", "mmbot-msgdb-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	db := NewMessageDB(filepath.Join(dir, "t.db"))
	if db == nil || db.db == nil {
		t.Fatal("打开测试库失败")
	}
	return db
}

func titles(rows []ChannelRecent) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

func countAll(t *testing.T, db *MessageDB) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	return n
}
