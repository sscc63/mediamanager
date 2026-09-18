package bot

// 频道监控模块（对应 123bot.py 的 get_latest_messages + main() 频道监控主循环）。
// 职责：抓取 t.me/s/{channel} 页面 → 解析消息 → 过滤匹配 → 转存（分享/秒传）→ 记录数据库。

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"mmbot/internal/gcguard"
	"mmbot/internal/httpx"
	"mmbot/internal/transfer"
	_ "modernc.org/sqlite"
)

// ChannelMessage 频道中的一条消息。
type ChannelMessage struct {
	MessageID   string
	DateStr     string
	MessageURL  string
	TargetURL   string
	MessageText string
	RawHTML     string // 消息 div 的完整 HTML（用于提取 href 中的 123 分享链接，对应 Python 的 str(msg)）
}

// MessageDB 频道消息记录数据库（对应 TG_monitor-123.db 的 messages 表）。
type MessageDB struct {
	dbPath string
	db     *sql.DB
}

var reExcludePwd = regexp.MustCompile(`(?i)提取码\s*[:：]\s*(\w+)`)

// reChannelMeta 匹配电报频道消息中的「片名（年份）」，兼容中英文括号（如 黑客帝国（1999） / 黑客帝国 (1999)）。
var reChannelMeta = regexp.MustCompile(`[（(]\s*(\d{4})\s*[)）]`)

// 以下正则为干净提取 bot 型频道「🎬 片名 (年份) 已更新 + 🎭 类型：X剧」的结构化消息。
var reLink = regexp.MustCompile(`https?://\S+`)
var reEmoji = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{FE0F}\x{3000}]`)
var reBracketContent = regexp.MustCompile(`[（(【\[][^）)】\]\n]*[）)】\]]`)
var reTailWord = regexp.MustCompile(`(已更新|更新|连载|更新至|HDTV|高清|第\d+[季集]|全集)$`)
var reChannelType = regexp.MustCompile(`类型[:：]\s*([^，,\n]+)`)

// reChannelLabel 片名前的前导内容标签（「剧集：成也萧河 (2026)」「电影：A子计划 (2026)」）。
//
// shareme123pan 这类频道的首行格式是「<标签>：<片名> (<年份>) <SxxExx>」，标签是频道自己
// 加的归类词而不是片名的一部分。channelCleanHead 只 Trim 首尾的「：」，标签在中间削不掉，
// 于是识别出的标题变成「剧集：成也萧河」，拿去搜 TMDB 必然搜不到（频道更新里显示为
// 匹配不到 TMDB 的占位卡，或者匹配到风马牛不相及的作品）。
//
// 必须锚定在行首（^\s*）且限定这几个词：像「名侦探柯南：独眼的残像」「流浪地球：飞跃2020特别版」
// 这种标题自带的冒号绝不能被削。允许「剧集 :」这种冒号前带空格、以及「剧集:」半角冒号的变体。
var reChannelLabel = regexp.MustCompile(`^\s*(剧集|电影|动漫|综艺|纪录片|动画|电视剧|美剧|英剧|日剧|韩剧|国剧|港剧|台剧)\s*[:：]\s*`)

// reVideoMark 影视标记（🎬/🎥/🎞/📽/🎦）：bot 型频道把片名写在带这个标记的那一行。
var reVideoMark = regexp.MustCompile(`[\x{1F3AC}\x{1F3A5}\x{1F39E}\x{1F4FD}\x{1F3A6}]`)

// reProgress 更新进度标记（更新至第05集 / 全30集 / 第12话）。
// 与 reTailWord 的区别：后者只匹配行尾，而「🎬 更新至第05集 繁花 (2023)」这种把进度
// 写在片名前面的消息漏不掉，识别出的标题会带上「更新至第05集」，TMDB 必然搜不到。
// 只剥「集/话/期」，不碰「第N季」—— 季数对剧集识别是有用信息。
var reProgress = regexp.MustCompile(`(?:已)?更新(?:至)?\s*第?\s*\d+\s*(?:-\s*\d+)?\s*[集话期]|全\s*\d+\s*[集话期]|第\s*\d+\s*[集话期]`)

// reSeasonEpisode 季集编号（S01E09 / s2e11 / S01.E09 / 第1季第9集）。
// shareme123pan 的首行是「剧集：成也萧河 (2026) S01E09」，reProgress 只认「第N集/话/期」，
// 削不掉 SxxExx，于是清洗后残留「成也萧河 S01E09」。当前主路径因为在年份括号处就把字符串
// 截断了、侥幸看不出来，但任何走到整行清洗的路径（或以后改了截断逻辑）都会把这个尾巴带进标题。
var reSeasonEpisode = regexp.MustCompile(`(?i)\bS\s*\d{1,2}\s*[._-]?\s*E\s*\d{1,4}\b|第\s*\d+\s*季\s*第\s*\d+\s*[集话期]`)

// messageDBMu / messageDBCache 同一路径共用一个实例，且随进程常驻。
//
// 必须复用而不能每次新建：database/sql 的 connectionOpener goroutine 会一直持有 *sql.DB，
// 不调用 Close 就永远不会被回收 —— 每个未关闭的实例常驻 1 个 goroutine + 1 条 sqlite 连接
// （含其 page cache，实测约 170KB），并且一直占着数据库文件句柄。
// 「频道更新」接口每打开一次页面就调一次 NewMessageDB，原先等于按访问次数无界泄漏。
var (
	messageDBMu    sync.Mutex
	messageDBCache = map[string]*MessageDB{}
)

// NewMessageDB 打开/创建消息记录数据库；同一路径复用同一实例。
func NewMessageDB(dbPath string) *MessageDB {
	if dbPath == "" {
		// 与 mem_history.json / user_states.db 一致，放在挂载的 data/ 目录以持久化去重记录
		dbPath = filepath.Join("data", "TG_monitor-123.db")
	}
	messageDBMu.Lock()
	defer messageDBMu.Unlock()
	if m, ok := messageDBCache[dbPath]; ok {
		return m
	}
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	m := &MessageDB{dbPath: dbPath}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Printf("[监控] 打开消息数据库失败: %v", err)
		return m // 打开失败不缓存，下次调用重试
	}
	applySQLiteLimits(db)
	m.db = db
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS messages (
		msg_id INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT,
		date TEXT,
		message_url TEXT,
		target_url TEXT,
		transfer_status TEXT,
		transfer_time TEXT,
		transfer_result TEXT,
		media_title TEXT,
		media_year TEXT,
		media_type TEXT,
		recognized_at TEXT,
		target_pid INTEGER)`)
	// 兼容旧库：补齐标题相关列
	m.ensureColumn("media_title", "TEXT")
	m.ensureColumn("media_year", "TEXT")
	m.ensureColumn("media_type", "TEXT")
	m.ensureColumn("recognized_at", "TEXT")
	// 扫描时算好的目标目录（含 ENV_SECOND_FILTER 二次过滤结果），供「立即入库」复用
	m.ensureColumn("target_pid", "INTEGER")
	messageDBCache[dbPath] = m
	return m
}

// ensureColumn 若列不存在则补齐（兼容已存在的 TG_monitor-123.db）。
func (m *MessageDB) ensureColumn(col, ddl string) {
	if m == nil || m.db == nil {
		return
	}
	// 必须先放掉 PRAGMA 查询占用的连接再执行 ALTER：SetMaxOpenConns(1) 下，
	// rows 还开着时 Exec 拿不到连接，会永久阻塞。
	if m.hasColumn(col) {
		return
	}
	_, _ = m.db.Exec(fmt.Sprintf("ALTER TABLE messages ADD COLUMN %s %s", col, ddl))
}

// hasColumn 查询 messages 表是否已有该列；返回前关闭 rows 以释放连接。
func (m *MessageDB) hasColumn(col string) bool {
	rows, err := m.db.Query("PRAGMA table_info(messages)")
	if err != nil {
		return false
	}
	defer rows.Close()
	// PRAGMA table_info 的列顺序是 cid, name, type, notnull, dflt_value, pk。
	// 原先把 dflt_value 扫进了 int 的 pk：没有 DEFAULT 的列 dflt_value 是 NULL，
	// Scan 直接报错被 continue 掉，导致本函数对任何列都返回 false ——
	// 迁移逻辑于是每次启动都重跑一遍 ALTER（错误被丢弃所以一直没暴露）。
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name == col {
			return true
		}
	}
	return false
}

// IsProcessed 检查消息是否已处理（无论转存是否成功）。
func (m *MessageDB) IsProcessed(messageURL string) bool {
	if m == nil || m.db == nil || messageURL == "" {
		return false
	}
	var one int
	err := m.db.QueryRow("SELECT 1 FROM messages WHERE message_url = ?", messageURL).Scan(&one)
	return err == nil
}

// Save 记录/更新消息处理结果。targetPID 是本次扫描为该消息算出的目标目录，
// 落库供「立即入库」复用，保证手动与自动落到同一目录。
func (m *MessageDB) Save(messageID, date, messageURL, targetURL, title, year, mtype, status, result string, targetPID int) {
	if m == nil || m.db == nil || messageURL == "" {
		return
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	// 与 Python 原版一致：纯 INSERT（id 非唯一约束，去重由调用方 IsProcessed 按 message_url 判断）
	_, err := m.db.Exec(`INSERT INTO messages (id, date, message_url, target_url, transfer_status, transfer_time, transfer_result, media_title, media_year, media_type, recognized_at, target_pid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, date, messageURL, targetURL, status, now, result, title, year, mtype, now, targetPID)
	if err != nil {
		log.Printf("[监控] 保存消息记录失败: %v", err)
	}
}

// ChannelRecent 频道近期更新影视（一条消息一行，含识别标题）。
type ChannelRecent struct {
	Title        string
	Year         string
	Type         string
	MessageURL   string
	TransferTime string
	TargetURL    string
	TargetPID    int
}

// ListRecentWithTitle 查询近 hours 小时内、已识别出标题的频道消息（按时间倒序）。
// 用于首页/订阅探索页展示「频道更新」片单。
func (m *MessageDB) ListRecentWithTitle(hours int, limit int) []ChannelRecent {
	if m == nil || m.db == nil {
		return nil
	}
	if hours <= 0 {
		hours = 3
	}
	if limit <= 0 {
		limit = 30
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02T15:04:05")
	rows, err := m.db.Query(`SELECT media_title, COALESCE(media_year,''), COALESCE(media_type,''), message_url, transfer_time, target_url, COALESCE(target_pid,0)
		FROM messages WHERE media_title <> '' AND recognized_at >= ?
		ORDER BY transfer_time DESC LIMIT ?`, since, limit)
	if err != nil {
		log.Printf("[监控] ListRecentWithTitle 查询失败: %v", err)
		return nil
	}
	defer rows.Close()
	var out []ChannelRecent
	for rows.Next() {
		var c ChannelRecent
		if err := rows.Scan(&c.Title, &c.Year, &c.Type, &c.MessageURL, &c.TransferTime, &c.TargetURL, &c.TargetPID); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// TargetPIDByURL 按分享链接反查扫描时算好的目标目录（已含二次过滤结果），查不到返回 0。
// 供「立即入库」复用自动监控的目录决策：同一条链接不该因为手动触发就落到别的目录。
func (m *MessageDB) TargetPIDByURL(targetURL string) int {
	if m == nil || m.db == nil || targetURL == "" {
		return 0
	}
	var pid int
	err := m.db.QueryRow(`SELECT COALESCE(target_pid,0) FROM messages
		WHERE target_url = ? AND COALESCE(target_pid,0) > 0
		ORDER BY transfer_time DESC, msg_id DESC LIMIT 1`, targetURL).Scan(&pid)
	if err != nil {
		return 0
	}
	return pid
}

// Cleanup 清理 retention 天前的历史记录（对应 cleanup_db）。
func (m *MessageDB) Cleanup(retentionDays int) {
	if m == nil || m.db == nil {
		return
	}
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format("2006-01-02T15:04:05")
	res, err := m.db.Exec("DELETE FROM messages WHERE transfer_time < ?", cutoff)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[监控] 已清理 %d 条 %d 天前的消息记录", n, retentionDays)
		}
	}
}

// CleanupRecentHours 删除超过指定小时数的频道消息记录（按识别时间 recognized_at 判定）。
// 与 Cleanup 的区别：以识别时间为准，能覆盖未转存成功（transfer_time 为空）的已识别条目。
func (m *MessageDB) CleanupRecentHours(hours int) {
	if m == nil || m.db == nil {
		return
	}
	if hours <= 0 {
		hours = 24
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02T15:04:05")
	res, err := m.db.Exec("DELETE FROM messages WHERE recognized_at <> '' AND recognized_at < ?", cutoff)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[监控] 已清理 %d 条 %d 小时前的频道消息记录", n, hours)
		}
	}
}

// monitorHTTP 频道抓取共享 HTTP 客户端。
var monitorHTTP = httpx.New(15 * time.Second)

// fetchChannelMessages 抓取单个 t.me/s/ 频道页面并解析消息（对应 get_latest_messages 单频道部分）。
// 返回所有解析到的消息（含已处理过的，由调用方过滤）。
func fetchChannelMessages(channelURL string) []ChannelMessage {
	// 预处理：https://t.me/xxx → https://t.me/s/xxx
	channelURL = strings.TrimSpace(channelURL)
	if strings.HasPrefix(channelURL, "https://t.me/") && !strings.Contains(channelURL, "/s/") {
		name := strings.TrimPrefix(channelURL, "https://t.me/")
		name = strings.Trim(name, "/")
		channelURL = "https://t.me/s/" + name
	}
	body, status, err := monitorHTTP.Get(context.Background(), channelURL, nil)
	if err != nil || status != 200 {
		log.Printf("[监控] 抓取频道失败 %s: status=%d err=%v", channelURL, status, err)
		return nil
	}
	messages := parseTgmeMessages(body)
	log.Printf("[监控] 频道 %s 解析到 %d 条消息（最新的在最后）", channelURL, len(messages))
	return messages
}

// parseTgmeMessages 解析 t.me/s/ 页面的消息块（div.tgme_widget_message）。
// 用 bytes.NewReader 直接吃原始字节：频道页动辄几百 KB，string(body) 会再复制一整份。
func parseTgmeMessages(body []byte) []ChannelMessage {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		log.Printf("[监控] 解析频道 HTML 失败: %v", err)
		return nil
	}
	var out []ChannelMessage
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "tgme_widget_message") {
			if cm := parseTgmeMessage(n); cm != nil {
				out = append(out, *cm)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// parseTgmeMessage 解析单个消息 div。
func parseTgmeMessage(div *html.Node) *ChannelMessage {
	cm := &ChannelMessage{}
	// data-post 作为消息 ID
	for _, a := range div.Attr {
		if a.Key == "data-post" {
			parts := strings.Split(a.Val, "/")
			cm.MessageID = parts[len(parts)-1]
		}
	}
	// 渲染消息 div 完整 HTML，用于提取 href 中的 123 分享链接（对应 Python 的 str(msg)）
	var buf strings.Builder
	if err := html.Render(&buf, div); err == nil {
		cm.RawHTML = buf.String()
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch {
		case n.Data == "time":
			for _, a := range n.Attr {
				if a.Key == "datetime" {
					cm.DateStr = a.Val
				}
			}
		case n.Data == "a" && hasClass(n, "tgme_widget_message_date"):
			for _, a := range n.Attr {
				if a.Key == "href" {
					cm.MessageURL = strings.TrimPrefix(a.Val, "/")
				}
			}
		case n.Data == "div" && hasClass(n, "tgme_widget_message_text"):
			cm.MessageText = nodeText(n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(div)

	if cm.MessageID == "" && cm.MessageURL == "" && cm.MessageText == "" {
		return nil
	}
	return cm
}

// nodeText 提取节点纯文本（\n 分隔，对应 bs4 get_text(separator='\n', strip=True)）。
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var rec func(n *html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.TrimSpace(sb.String())
}

func hasClass(n *html.Node, cls string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == cls {
					return true
				}
			}
		}
	}
	return false
}

// 二次过滤规则：ENV_SECOND_FILTER 形如 "DV:1,DOLBY VISION:2" → 关键词:文件夹ID
type secondFilterRule struct {
	keyword string
	folder  int
}

// parseSecondFilters 解析二次过滤规则。
func parseSecondFilters(raw string) []secondFilterRule {
	var rules []secondFilterRule
	for _, rule := range strings.Split(raw, ",") {
		if idx := strings.Index(rule, ":"); idx > 0 {
			kw := strings.TrimSpace(rule[:idx])
			id := 0
			fmt.Sscanf(strings.TrimSpace(rule[idx+1:]), "%d", &id)
			if kw != "" && id > 0 {
				rules = append(rules, secondFilterRule{keyword: kw, folder: id})
			}
		}
	}
	return rules
}

// matchSecondFilter 二次过滤：命中规则返回目标文件夹 ID，否则返回 defaultID。
func matchSecondFilter(text string, rules []secondFilterRule, defaultID int) int {
	for _, r := range rules {
		if strings.Contains(text, r.keyword) {
			log.Printf("[监控] 消息匹配二次过滤关键词 '%s'，将转存到文件夹ID: %d", r.keyword, r.folder)
			return r.folder
		}
	}
	return defaultID
}

// checkChannelSafe 包一层 panic 兜底再调用 checkChannel。
// checkChannel 解析的是外部抓来的 HTML，越界/空指针都可能发生；而它的两个调用方
// （StartMonitor 主循环、TriggerCheck 手动检查）都在独立 goroutine 中，
// 未 recover 的 panic 会直接终止整个进程。兜住之后最坏只是放弃本轮扫描。
func (b *Bot) checkChannelSafe() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[监控] 频道扫描异常: %v", r)
		}
	}()
	b.checkChannel()
}

// checkChannel 单次频道检查：抓取全部配置频道并处理未处理消息。
// 返回处理的消息数。
func (b *Bot) checkChannel() int {
	if b.env.GetInt("ENV_AUTHORIZATION", 0) == 0 {
		return 0
	}
	channelRaw := b.env.Get("ENV_TG_CHANNEL", "")
	channelURLs := strings.Split(channelRaw, "|")
	if len(channelURLs) == 0 || (len(channelURLs) == 1 && strings.TrimSpace(channelURLs[0]) == "") {
		log.Printf("[监控] 未设置 ENV_TG_CHANNEL 环境变量")
		return 0
	}
	// 123 客户端未就绪时，转存内部会按需 initClient，这里直接检查授权开关即可
	excludeFilter := b.env.Get("ENV_EXCLUDE_FILTER", "")
	secondRules := parseSecondFilters(b.env.Get("ENV_SECOND_FILTER", ""))
	defaultPID := b.env.GetInt("ENV_123_UPLOAD_PID", 0)
	// 单例实例与 Web 侧共用，这里不能 Close，否则会把正在被使用的连接关掉
	msgDB := NewMessageDB("")

	processed := 0
	for _, channelURL := range channelURLs {
		channelURL = strings.TrimSpace(channelURL)
		if channelURL == "" {
			continue
		}
		log.Printf("[监控] ===== 处理频道: %s =====", channelURL)
		messages := fetchChannelMessages(channelURL)
		for _, cm := range messages {
			// 消息可能含多个 123 链接，逐一处理；提取码自动附加
			// 用消息 div 完整 HTML 提取（Python 原版 extract_target_url(str(msg))，链接在 href 属性中）
			targetURLs := extractTargetURL(cm.RawHTML)
			for _, url := range targetURLs {
				// 提取码：消息文本中有"提取码:xxx"且 URL 无 pwd 参数则附加
				if pm := reExcludePwd.FindStringSubmatch(cm.MessageText); pm != nil && !strings.Contains(url, "pwd=") {
					if strings.Contains(url, "?") {
						url = url + "&pwd=" + pm[1]
					} else {
						url = url + "?pwd=" + pm[1]
					}
					log.Printf("[监控] 已为URL添加提取码: %s", url)
				}
				if msgDB.IsProcessed(cm.MessageURL) {
					log.Printf("[监控] 消息已处理，跳过")
					continue
				}
				cm.TargetURL = url
				b.processChannelMessage(cm, msgDB, excludeFilter, secondRules, defaultPID)
				processed++
			}
			// 无 123 链接但消息未处理时也尝试秒传链接
			if len(targetURLs) == 0 {
				if msgDB.IsProcessed(cm.MessageURL) {
					continue
				}
				b.processChannelMessage(cm, msgDB, excludeFilter, secondRules, defaultPID)
				processed++
			}
		}
		time.Sleep(1 * time.Second) // 频道间限速
	}
	return processed
}

// processChannelMessage 处理单条频道消息：过滤 → 二次过滤 → 转存 → 记录。
func (b *Bot) processChannelMessage(cm ChannelMessage, msgDB *MessageDB, excludeFilter string, secondRules []secondFilterRule, defaultPID int) {
	// 目标目录在过滤判断之前就算好：不管这条消息最终转存与否，都要随记录落库，
	// 这样用户在「频道更新」里点「立即入库」时才能复用同一个目录决策。
	transferID := matchSecondFilter(cm.MessageText, secondRules, defaultPID)
	// 过滤条件匹配（消息标题/链接命中任一过滤词）
	if !b.filterMatch(cm.TargetURL, cm.MessageText) {
		b.recordChannelResult(cm, msgDB, "未转存", fmt.Sprintf("未匹配过滤条件（%s），跳过转存", b.Filter()), transferID)
		time.Sleep(1 * time.Second)
		return
	}
	// 排除关键词
	if excludeFilter != "" && (strings.Contains(cm.TargetURL, excludeFilter) || strings.Contains(cm.MessageText, excludeFilter)) {
		b.recordChannelResult(cm, msgDB, "未转存", fmt.Sprintf("包含排除关键词（%s），跳过转存", excludeFilter), transferID)
		time.Sleep(1 * time.Second)
		return
	}

	log.Printf("[监控] 消息匹配过滤条件（%s），开始转存...", b.Filter())

	if cm.TargetURL != "" {
		if b.transferSharedLinkOptimize(cm.TargetURL, transferID) {
			title := b.matchedFilterTitle(cm.TargetURL, cm.MessageText)
			msg := fmt.Sprintf("📡 监控到『<a href=\"%s\">%s</a>』已转存✅", cm.MessageURL, htmlEscape(title))
			b.SubmitSend(func() { b.SendMessage(msg) })
			b.triggerTransferAfterSave(transferID)
			b.recordChannelResult(cm, msgDB, "转存成功", msg, transferID)
		} else {
			msg := fmt.Sprintf("❌123云盘转存失败\n消息内容: %s\n链接: %s", cm.MessageURL, cm.TargetURL)
			b.SubmitSend(func() { b.SendMessage(msg) })
			b.recordChannelResult(cm, msgDB, "转存失败", msg, transferID)
		}
		// 与秒传分支对齐：每条分享链接转存后间隔 10 秒，避免触发 123pan 账号级限流
		time.Sleep(10 * time.Second)
		return
	}

	// 无分享链接 → 尝试秒传链接
	fullLinks := extract123LinksFromFullText(cm.MessageText)
	if len(fullLinks) > 0 {
		ok := false
		for _, link := range fullLinks {
			if b.saveShareLinkLink(link, transferID) {
				ok = true
			}
		}
		if ok {
			msg := "✅123云盘秒传链接转存成功\n消息内容: " + cm.MessageURL + "\n"
			b.SubmitSend(func() { b.SendMessage(msg) })
			b.triggerTransferAfterSave(transferID)
			b.recordChannelResult(cm, msgDB, "转存成功", msg, transferID)
		} else {
			msg := "❌123云盘秒传链接转存失败\n消息内容: " + cm.MessageURL + "\n"
			b.recordChannelResult(cm, msgDB, "转存失败", msg, transferID)
		}
	} else {
		b.recordChannelResult(cm, msgDB, "转存失败", "❌123云盘秒传链接转存失败\n消息内容: "+cm.MessageURL, transferID)
	}
	time.Sleep(10 * time.Second)
}

// saveShareLinkLink 转存单条秒传链接，返回是否成功。
func (b *Bot) saveShareLinkLink(link string, targetPID int) bool {
	entries := parseShareLink(link)
	if len(entries) == 0 {
		return false
	}
	files := make([]FileItem, 0, len(entries))
	usesV2 := false
	for _, e := range entries {
		files = append(files, FileItem{Path: e.Path, Etag: e.Etag, Size: e.Size})
		if e.IsV2Etag {
			usesV2 = true
		}
	}
	s, _ := b.saveFilesTo123(nil, "", files, len(files), 0, usesV2, false, targetPID, nil, nil, "秒传")
	return s > 0
}

// recordChannelResult 记录频道消息处理结果；targetPID 一并落库，供「立即入库」复用。
func (b *Bot) recordChannelResult(cm ChannelMessage, msgDB *MessageDB, status, result string, targetPID int) {
	title, year, mtype := recognizeChannelTitle(cm.MessageText)
	msgDB.Save(cm.MessageID, cm.DateStr, cm.MessageURL, cm.TargetURL, title, year, mtype, status, result, targetPID)
	log.Printf("[监控] 已记录: %s | %s | 状态: %s | 标题: %s", cm.MessageID, cm.TargetURL, status, title)
}

// recognizeChannelTitle 从频道消息文本提取影视标题、年份、类型。
// 优先处理 bot 型结构化消息「🎬 片名 (年份) 已更新 + 🎭 类型：X剧」，失败时退化用 transfer.Recognize 识别文件名式命名。
func recognizeChannelTitle(text string) (title, year, mtype string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	mtype = channelTypeFromText(text) // 优先用消息自带的「类型：X剧/X电影」字段
	idx := reChannelMeta.FindStringSubmatchIndex(text)
	if idx != nil {
		if y, err := strconv.Atoi(text[idx[2]:idx[3]]); err == nil && saneYear(y) > 0 {
			year = text[idx[2]:idx[3]]
		}
		pre := text[:idx[0]]
		title = channelCleanHead(channelLastLine(pre))
		if title == "" {
			title = channelCleanHead(pre)
		}
		if title != "" {
			return
		}
	}
	// 回退：把「片名所在的那一行」交给文件名识别器，而不是整段消息文本。
	// transfer.Recognize 是给单个文件名设计的，多行消息里的 emoji、「类型：X剧」、分享链接
	// 会被一并当成标题残留（实测标题会变成 "🎬 某剧 已更新 🔗 https://..."），年份还会从
	// 链接里的 4 位数字被抓成 1234。先挑行缩小范围，再用 channelCleanHead 双重收口。
	line := channelPickTitleLine(text)
	if line == "" {
		line = text
	}
	meta := transfer.Recognize(line, "", nil)
	title = channelCleanHead(meta.Name)
	if title == "" {
		return
	}
	if y := saneYear(meta.Year); y > 0 {
		year = strconv.Itoa(y)
	}
	if mtype == "" {
		switch {
		case meta.Type != "":
			mtype = meta.Type
		case meta.Season > 0 || meta.Episode > 0:
			mtype = "tv"
		default:
			mtype = "movie"
		}
	}
	return
}

// channelTypeFromText 从 bot 消息「🎭 类型：国产剧/欧美剧/日韩剧/电影」判定 media_type。
func channelTypeFromText(text string) string {
	m := reChannelType.FindStringSubmatch(text)
	if len(m) == 2 {
		if strings.Contains(m[1], "剧") {
			return "tv"
		}
		if strings.Contains(m[1], "电影") {
			return "movie"
		}
	}
	return ""
}

// saneYear 过滤不可能的年份：识别器会把分享链接里的 4 位数字（如 Abcd-1234）当成
// 年份，实测会输出 1234。超出合理范围的一律丢弃，避免污染 TMDB 年份匹配与前端展示。
func saneYear(y int) int {
	if y < 1900 || y > time.Now().Year()+2 {
		return 0
	}
	return y
}

// channelLineIsMeta 判断一行是不是元信息（类型 / 更新进度 / 链接 / 提取码 / 简介），
// 而不是片名行。宁可漏滤也不要错滤：挑不出行时调用方会退回整段文本，
// 最坏等于修复前的行为，不会更差。
func channelLineIsMeta(s string) bool {
	if reLink.MatchString(s) || reChannelType.MatchString(s) {
		return true
	}
	for _, kw := range []string{"提取码", "链接", "地址", "简介", "豆瓣", "评分"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// channelPickTitleLine 从消息文本里挑出最可能承载片名的那一行。
//
// 为什么需要它：reChannelMeta 只认括号里的年份，消息里没有「(YYYY)」时旧代码会把
// 整段消息喂给 transfer.Recognize（那是给文件名用的），emoji、「类型：X剧」、分享链接
// 全都变成标题残留。优先取带影视标记（🎬 等）的那一行；都没有标记时退回最后一个
// 非元信息行（与 channelLastLine 的取向一致）。
func channelPickTitleLine(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	fallback := ""
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		if s == "" || channelLineIsMeta(s) {
			continue
		}
		if reVideoMark.MatchString(s) {
			return s
		}
		fallback = s
	}
	return fallback
}

// channelLastLine 取文本最后一个非空行（通常即「🎬 片名 已更新」所在行）。
func channelLastLine(pre string) string {
	pre = strings.ReplaceAll(pre, "\r", "")
	lines := strings.Split(pre, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// channelCleanHead 清洗片名候选：去前导标签/emoji/链接/括号内容/「已更新」等尾词与首尾符号。
func channelCleanHead(s string) string {
	// 前导标签最先剥：「剧集：成也萧河 (2026)」要先变成「成也萧河 (2026)」，
	// 后面 reBracketContent 才会把 (2026) 也一并去掉。放在最后剥则年份已被吃掉了。
	s = reChannelLabel.ReplaceAllString(s, "")
	s = reEmoji.ReplaceAllString(s, " ")
	s = reLink.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "\u200b", "")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = reBracketContent.ReplaceAllString(s, " ")
	s = reSeasonEpisode.ReplaceAllString(s, " ")
	s = reProgress.ReplaceAllString(s, " ")
	s = reTailWord.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, " ·:：,，。、-—_&/")
}

// RestoreTransferLink 将「频道更新」里的某条 123 分享链接立即转存入库。
// 供 /api/monitor/transfer 调用（长按菜单「入库」）。
//
// 目标目录与自动监控保持一致：按 target_url 反查扫描时算好的目录（已含 ENV_SECOND_FILTER
// 二次过滤结果），这样同一条分享链接不管走自动还是手动，都落到同一个地方。
// 查不到记录（老库、或链接不来自频道）才退回 ENV_123_UPLOAD_PID（监控转存目录）——
// 不能退回 ENV_123_LINK_UPLOAD_PID，那是 TG 里直接贴链接转存那一族用的目录。
//
// 注意：这条路径【没有去重】。自动监控那条路靠 IsProcessed 拦重复，这里没有；
// transferSharedLinkOptimize 只负责转存，失败才发 TG，成功仅写日志。
// 所以前端必须自己防连点（见 index.html 的 _chTransferBusy），否则会重复转存两份。
func (b *Bot) RestoreTransferLink(url string) bool {
	pid := NewMessageDB("").TargetPIDByURL(url)
	if pid <= 0 {
		pid = b.env.GetInt("ENV_123_UPLOAD_PID", 0)
	}
	if !b.transferSharedLinkOptimize(url, pid) {
		return false
	}
	b.triggerTransferAfterSave(pid)
	return true
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;",
	)
	return r.Replace(s)
}

// StartMonitor 启动频道监控主循环（对应 main() 的频道检查循环）。
// checkInterval 单位：分钟（ENV_CHECK_INTERVAL）。
func (b *Bot) StartMonitor() {
	interval := b.env.GetInt("ENV_CHECK_INTERVAL", 0)
	log.Printf("[监控] 123转存目标目录ID: %d | 检查间隔: %d分钟", b.env.GetInt("ENV_123_UPLOAD_PID", 0), interval)
	b.monitorMu.Lock()
	b.monitorActive = true
	b.monitorNext = time.Time{}
	b.monitorMu.Unlock()
	go func() {
		lastCleanup := time.Now()
		for {
			select {
			case <-b.ctx.Done():
				b.monitorMu.Lock()
				b.monitorActive = false
				b.monitorMu.Unlock()
				return
			default:
			}
			b.monitorScanMu.Lock()
			b.monitorMu.Lock()
			b.monitorScanning = true
			b.monitorMu.Unlock()
			b.checkChannelSafe()
			b.monitorScanMu.Unlock()
			b.monitorMu.Lock()
			b.monitorScanning = false
			b.monitorLast = time.Now()
			if interval > 0 {
				b.monitorNext = b.monitorLast.Add(time.Duration(interval) * time.Minute)
			} else {
				b.monitorNext = time.Time{}
			}
			b.monitorMu.Unlock()
			// 每日清理数据库记录
			if time.Since(lastCleanup) >= 24*time.Hour {
				edb := NewMessageDB("")
				edb.Cleanup(b.env.GetInt("ENV_DB_RETENTION_DAYS", 30))
				edb.CleanupRecentHours(24) // 频道更新记录保留 24 小时
				lastCleanup = time.Now()
			}
			// 一轮扫描的任务边界：此时频道页正文、消息列表、识别中间结果都已随 checkChannel
			// 返回而失去引用，主动回收一次，避免这些临时大对象长期抬高容器 RSS。
			// 必须放在调用方而不是 checkChannel 内部 —— 函数尚未返回时它的局部变量仍然存活，
			// 那样等于回收不到东西，只白付一次 STW 的代价。
			gcguard.Reclaim()
			log.Printf("[监控] 休息%d分钟...", interval)
			select {
			case <-b.ctx.Done():
				b.monitorMu.Lock()
				b.monitorActive = false
				b.monitorMu.Unlock()
				return
			case <-time.After(time.Duration(interval) * time.Minute):
			}
		}
	}()
}

// MonitorStatus 返回频道监控运行状态（扫描中、是否激活、上次/下次检查时间，零值表示无）。
func (b *Bot) MonitorStatus() (scanning, active bool, last, next time.Time) {
	b.monitorMu.Lock()
	defer b.monitorMu.Unlock()
	return b.monitorScanning, b.monitorActive, b.monitorLast, b.monitorNext
}

// TriggerCheck 手动触发一次频道检查；与定时扫描共用 monitorScanMu 互斥，
// 已有扫描在进行时返回 started=false，避免并发扫描重复转存。
// 扫描在后台 goroutine 中执行，立即返回，避免阻塞调用方（如 Web 接口）。
func (b *Bot) TriggerCheck() bool {
	if !b.monitorScanMu.TryLock() {
		return false
	}
	go func() {
		// defer 是后进先出：先注册 Reclaim、后注册 Unlock，实际执行顺序就是先放扫描锁、
		// 再回收内存。回收期间不能占着 monitorScanMu，否则用户点「立即检查」会因
		// TryLock 失败而被误报成「已有扫描在进行」。
		defer gcguard.Reclaim()
		defer b.monitorScanMu.Unlock()
		b.monitorMu.Lock()
		b.monitorScanning = true
		b.monitorMu.Unlock()
		b.checkChannelSafe()
		b.monitorMu.Lock()
		b.monitorScanning = false
		b.monitorLast = time.Now()
		b.monitorMu.Unlock()
	}()
	return true
}
