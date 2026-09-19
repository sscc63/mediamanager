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

// TestBackfillUseDate 老库必须回填 usedate，否则过期条目会靠回退赖在 24h 窗口里。
func TestBackfillUseDate(t *testing.T) {
	path := filepath.Join(tempDir(t), "t.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开裸库失败: %v", err)
	}
	// 建没有 usedate 列的旧表
	_, err = raw.Exec(`CREATE TABLE messages (
		msg_id INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT, date TEXT, message_url TEXT,
		target_url TEXT, transfer_status TEXT, transfer_time TEXT, transfer_result TEXT,
		media_title TEXT, media_year TEXT, media_type TEXT, recognized_at TEXT, target_pid INTEGER)`)
	if err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	now := time.Now()
	recog := now.Format("2006-01-02T15:04:05") // 三行入库时刻相同
	ins := `INSERT INTO messages (date, message_url, transfer_status, transfer_time, media_title, recognized_at)
		VALUES (?, ?, '转存成功', ?, ?, ?)`
	for _, c := range []struct{ pub, title string }{
		{now.Add(-12 * 24 * time.Hour).Format(time.RFC3339), "老片子"}, // 应被逐出
		{now.Add(-1 * time.Hour).Format(time.RFC3339), "新片子"},      // 应保留
		{"", "无时间戳"},                                              // date 解析不了，靠回退保留
	} {
		if _, err := raw.Exec(ins, c.pub, "https://t.me/c/"+c.title, recog, c.title, recog); err != nil {
			t.Fatalf("插入 %s 失败: %v", c.title, err)
		}
	}
	raw.Close()

	db := NewMessageDB(path) // 补列时自动回填
	if db == nil || db.db == nil {
		t.Fatal("打开库失败")
	}
	var filled int
	db.db.QueryRow("SELECT COUNT(*) FROM messages WHERE usedate <> ''").Scan(&filled)
	if filled != 2 {
		t.Errorf("应回填 2 条（老帖+新帖），实际 %d 条", filled)
	}

	// 老帖回填后必须被逐出；「无时间戳」的 target_url 为 NULL，顺带覆盖 COALESCE 缺失时的静默丢行。
	got := db.ListRecentWithTitle(24, 30)
	if len(got) != 2 {
		t.Fatalf("窗口应剩 2 条，实际 %v", titles(got))
	}
	for _, r := range got {
		if r.Title == "老片子" {
			t.Errorf("12 天前的老帖回填后不应在窗口内: %v", titles(got))
		}
	}
	// 幂等
	db.backfillUseDate()
	filled = 0
	db.db.QueryRow("SELECT COUNT(*) FROM messages WHERE usedate <> ''").Scan(&filled)
	if filled != 2 {
		t.Errorf("回填应幂等，实际 %d 条", filled)
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

// tempDir 返回不会被自动清理的临时目录。不能用 t.TempDir()：连接常开，
// 它在 Windows 上会因文件占用而清理失败。
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mmbot-msgdb-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	return dir
}

// openTempMessageDB 每个用例独占一个库。MessageDB 按路径缓存且无 Close，只能靠唯一路径隔离。
func openTempMessageDB(t *testing.T) *MessageDB {
	t.Helper()
	db := NewMessageDB(filepath.Join(tempDir(t), "t.db"))
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
