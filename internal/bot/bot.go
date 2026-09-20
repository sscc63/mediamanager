// Package bot 实现 Telegram Bot 主程序（对应 Python 123bot.py 的 Bot 部分）。
// 职责：命令处理（start/info/oauth/restart/add/remove/organize/history/transfer_status/delete）、
// 通用消息处理（123 分享链接转存、秒传链接、夸克/115 转存、磁力、搜索分享）、文档处理、频道监控。
package bot

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"mmbot/internal/config"
	"mmbot/internal/pan123"
	"mmbot/internal/transfer"
)

// Bot 123bot Telegram 机器人。
type Bot struct {
	env      *config.Config
	client   *pan123.Client // 123 云盘客户端（init_123_client）
	shareBot *tgbotapi.BotAPI
	tg       *tgbotapi.BotAPI

	adminID     int64
	token       string
	shareToken  string
	shareChatID int64

	// 过滤器（/add /remove 动态更新）
	filterMu   sync.RWMutex
	filter     string
	filterPats []string // 拆分为关键词列表，忽略大小写匹配

	states *UserStateManager

	// 转存锁（确保文件依次转存）
	transferMu sync.Mutex

	// /update 命令互斥（防止并发重建容器）
	updateMu sync.Mutex

	// 频道监控状态（StartMonitor 记录，供前端后台页展示）
	monitorMu       sync.Mutex
	monitorActive   bool
	monitorScanning bool
	monitorLast     time.Time
	monitorNext     time.Time

	// 频道扫描互斥（定时扫描与手动触发共用，避免并发扫描重复转存）
	monitorScanMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	// 退出标志（每日 03:00 自动重启用）
	shouldExit chan struct{}

	// 上次发送消息时间（send_reply_delete 节流）
	lastSendMu sync.Mutex
	lastSend   time.Time

	// 首次连接成功后的回调（main 注入，用于每次启动向管理员发送启动通知）
	onStarted func()
}

// New 创建 Bot（不校验 token，token 无效时在 Start 轮询中重试）。
func New(env *config.Config) *Bot {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bot{
		env:         env,
		adminID:     int64(env.GetInt("ENV_TG_ADMIN_USER_ID", 0)),
		token:       env.Get("ENV_TG_BOT_TOKEN", ""),
		shareToken:  env.Get("TOKEN", ""),
		shareChatID: int64(env.GetInt("TARGET_CHAT_ID", 0)),
		filter:      env.Get("ENV_FILTER", ""),
		states:      NewUserStateManager("data/user_states.db"),
		ctx:         ctx,
		cancel:      cancel,
		shouldExit:  make(chan struct{}),
	}
	// 初始化过滤器关键词列表
	for _, k := range strings.Split(b.filter, "|") {
		k = strings.TrimSpace(k)
		if k != "" {
			b.filterPats = append(b.filterPats, strings.ToLower(k))
		}
	}
	return b
}

// SetClient 设置 123 云盘客户端（主程序 init_123_client 后注入）。
func (b *Bot) SetClient(c *pan123.Client) { b.client = c }

// SetOnStarted 设置首次成功连接 TG 后的回调（进程启动通知）。
func (b *Bot) SetOnStarted(fn func()) { b.onStarted = fn }

// Client 返回当前 123 客户端。
func (b *Bot) Client() *pan123.Client { return b.client }

// AdminID 返回管理员用户 ID。
func (b *Bot) AdminID() int64 { return b.adminID }

// SetFilter 更新过滤词（/add /remove 动态更新）。
func (b *Bot) SetFilter(f string) {
	b.filterMu.Lock()
	defer b.filterMu.Unlock()
	b.filter = f
	b.filterPats = b.filterPats[:0]
	for _, k := range strings.Split(f, "|") {
		k = strings.TrimSpace(k)
		if k != "" {
			b.filterPats = append(b.filterPats, strings.ToLower(k))
		}
	}
}

// Filter 返回当前过滤词。
func (b *Bot) Filter() string {
	b.filterMu.RLock()
	defer b.filterMu.RUnlock()
	return b.filter
}

// filterMatch 判断 target/text 是否命中任一过滤关键词。
func (b *Bot) filterMatch(target, text string) bool {
	b.filterMu.RLock()
	defer b.filterMu.RUnlock()
	lowerT := strings.ToLower(target)
	lowerS := strings.ToLower(text)
	for _, k := range b.filterPats {
		if strings.Contains(lowerT, k) || strings.Contains(lowerS, k) {
			return true
		}
	}
	return false
}

// matchedFilterTitle 返回命中的过滤关键词（用于监控通知标题）。
func (b *Bot) matchedFilterTitle(target, text string) string {
	b.filterMu.RLock()
	defer b.filterMu.RUnlock()
	lowerT := strings.ToLower(target)
	lowerS := strings.ToLower(text)
	for _, k := range b.filterPats {
		if strings.Contains(lowerT, k) || strings.Contains(lowerS, k) {
			return k
		}
	}
	return "影视资源"
}

// Start 启动 Bot：注册命令菜单并开始轮询（含 30 秒退避重连）。
func (b *Bot) Start() {
	go b.pollLoop()
}

// tgHTTPClient 返回复用的 HTTP 客户端（Client/Transport 只创建一次）。
// 关键措施：
//   - DisableKeepAlives: true  — 每次请求新建 TCP+TLS 连接，不复用连接（避免代理破坏 TLS 会话）
//   - TLSNextProto: empty map  — 彻底禁用 HTTP/2
//   - TLS 1.2 + 禁用会话复用  — 避免 TLS 1.3 和会话票证与透明代理的兼容问题
//   - Client/Transport 对象复用 — 仅持有配置，不持有连接（DisableKeepAlives 保证每请求新连接）
var (
	tgClientOnce   sync.Once
	tgCachedClient *http.Client
)

func tgHTTPClient() *http.Client {
	tgClientOnce.Do(func() {
		tgCachedClient = &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 0,
				}).DialContext,
				DisableKeepAlives:     true,
				MaxIdleConns:          0,
				IdleConnTimeout:       0,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				ForceAttemptHTTP2:     false,
				TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
				TLSClientConfig: &tls.Config{
					MinVersion:             tls.VersionTLS12,
					MaxVersion:             tls.VersionTLS12,
					SessionTicketsDisabled: true,
				},
			},
			Timeout: 180 * time.Second,
		}
	})
	return tgCachedClient
}

// freshHTTPClient 复用同一个 http.Client（每请求仍新建 TCP+TLS 连接，但 Client/Transport 对象不重建）。
type freshHTTPClient struct{}

func (c *freshHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return tgHTTPClient().Do(req)
}

// pollLoop 手动轮询 Telegram API。
// 采用短轮询（不设长轮询 timeout），避免长连接被透明代理干扰导致 tls: bad record MAC。
// 对 TLS 错误使用指数退避，最长 120 秒。
func (b *Bot) pollLoop() {
	var offset int
	first := true
	retry := time.Second * 3 // 当前退避间隔
	const maxRetry = time.Second * 120

	for {
		select {
		case <-b.ctx.Done():
			return
		default:
		}

		// BotAPI 只创建一次（HTTP Client 已复用，DisableKeepAlives 保证每请求新 TCP+TLS 连接）
		if b.tg == nil {
			tg, err := tgbotapi.NewBotAPIWithClient(b.token, tgbotapi.APIEndpoint, &freshHTTPClient{})
			if err != nil {
				log.Printf("[Bot] 创建 TG Bot 失败（%v后重试）: %v", retry, err)
				if !b.sleepCtx(retry) {
					return
				}
				retry = scaleRetry(retry, maxRetry)
				continue
			}
			b.tg = tg
		}

		if first {
			b.registerCommands()
			if b.onStarted != nil {
				b.onStarted()
			}
			log.Printf("✅ [Bot] TG Bot 轮询已启动")
			first = false
		}

		// 短轮询：timeout=0 表示立即返回，避免长连接被代理截断
		u := tgbotapi.NewUpdate(offset)
		u.Timeout = 0
		updates, err := b.tg.GetUpdates(u)
		if err != nil {
			// 对 TLS 错误使用指数退避，其他错误使用短退避
			if isTLSError(err) {
				log.Printf("[Bot] getUpdates TLS 错误（%v后重试）: %v", retry, err)
				if !b.sleepCtx(retry) {
					return
				}
				retry = scaleRetry(retry, maxRetry)
			} else {
				log.Printf("[Bot] getUpdates 失败（3秒后重试）: %v", err)
				if !b.sleepCtx(3 * time.Second) {
					return
				}
			}
			continue
		}

		// 成功：重置退避
		retry = time.Second * 3

		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			if update.Message == nil {
				continue
			}
			// 每条消息在一个独立 goroutine 中处理（与 Python 线程池一致）
			b.processMessage(update.Message)
		}

		// 短轮询间隔：100ms，避免 CPU 空转的同时保证及时响应
		if !b.sleepCtx(100 * time.Millisecond) {
			return
		}
	}
}

// isTLSError 判断错误是否为 TLS 相关错误。
func isTLSError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "tls:") || strings.Contains(msg, "bad record MAC") ||
		strings.Contains(msg, "handshake failure") || strings.Contains(msg, "TLS")
}

// scaleRetry 指数退避：乘以 2，但不超过 max。
func scaleRetry(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func (b *Bot) sleepCtx(d time.Duration) bool {
	select {
	case <-b.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Close 停止 Bot。
func (b *Bot) Close() {
	b.cancel()
}

// registerCommands 注册 TG 命令菜单。
func (b *Bot) registerCommands() {
	cmds := []tgbotapi.BotCommand{
		{Command: "start", Description: "开始使用机器人"},
		{Command: "info", Description: "打印当前账户的信息"},
		{Command: "delete", Description: "删除媒体"},
		{Command: "add", Description: "添加监控过滤词"},
		{Command: "remove", Description: "删除监控过滤词"},
		{Command: "oauth", Description: "Litepan OAuth认证"},
		{Command: "restart", Description: "❗重启服务"},
		{Command: "update", Description: "🔄拉取最新镜像并更新容器"},
	}
	if _, err := b.tg.Request(tgbotapi.NewSetMyCommands(cmds...)); err != nil {
		log.Printf("[Bot] 设置命令菜单失败: %v", err)
	} else {
		log.Printf("[Bot] 已设置命令菜单")
	}
}

// processMessage 分发消息到对应处理器（命令优先）。
func (b *Bot) processMessage(msg *tgbotapi.Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Bot] 处理消息异常: %v", r)
		}
	}()

	if msg.From == nil {
		return
	}

	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			b.handleStart(msg)
		case "info":
			b.handleInfo(msg)
		case "oauth":
			b.handleOAuth(msg)
		case "restart":
			b.handleRestart(msg)
		case "update":
			b.handleUpdate(msg)
		case "add":
			b.handleAddFilter(msg)
		case "remove":
			b.handleRemoveFilter(msg)
		case "organize":
			b.cmdOrganize(msg)
		case "history":
			b.cmdHistory(msg)
		case "transfer_status":
			b.cmdTransferStatus(msg)
		case "delete":
			b.handleDeleteCommand(msg)
		default:
			b.handleGeneralMessage(msg)
		}
		return
	}

	if msg.Document != nil {
		// JSON 秒传文件
		if msg.Document.MimeType == "application/json" || strings.HasSuffix(msg.Document.FileName, ".json") {
			b.processJSONFile(msg)
			return
		}
		// 其他文档（纯文本链接）
		b.processTextDocument(msg)
		return
	}

	b.handleGeneralMessage(msg)
}

// isAdmin 校验管理员权限。
func (b *Bot) isAdmin(msg *tgbotapi.Message) bool {
	if msg.From.ID == b.adminID {
		return true
	}
	b.SubmitSend(func() {
		b.SendReply(msg, "您没有权限使用此机器人。")
	})
	return false
}

// ---------- 命令处理器 ----------

func (b *Bot) handleStart(msg *tgbotapi.Message) {
	if msg.From.ID != b.adminID {
		b.SubmitSend(func() { b.SendReply(msg, "您没有权限使用此机器人。") })
		return
	}
	b.SubmitSend(func() { b.SendReply(msg, "机器人已启动") })
}

func (b *Bot) handleInfo(msg *tgbotapi.Message) {
	if msg.From.ID != b.adminID {
		b.SubmitSend(func() { b.SendReply(msg, "您没有权限使用此机器人。") })
		return
	}
	client := b.initClient()
	resp, err := client.UserInfo(context.Background())
	if err != nil {
		b.SubmitSend(func() { b.SendReply(msg, fmt.Sprintf("获取账户信息失败: %v", err)) })
		return
	}
	var data struct {
		Nickname       string `json:"Nickname"`
		Vip            bool   `json:"Vip"`
		UID            int64  `json:"UID"`
		Passport       string `json:"Passport"`
		BindWechat     bool   `json:"BindWechat"`
		SpaceUsed      int64  `json:"SpaceUsed"`
		SpacePermanent int64  `json:"SpacePermanent"`
		FileCount      int64  `json:"FileCount"`
		VipInfo        []struct {
			VipLabel  string `json:"vip_label"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
		} `json:"VipInfo"`
		DirectTraffic  int64 `json:"DirectTraffic"`
		ShareTraffic   int64 `json:"ShareTraffic"`
		StraightLink   bool  `json:"StraightLink"`
		BackupFileInfo struct {
			MobileTerminalBackupFileName  string `json:"MobileTerminalBackupFileName"`
			DesktopTerminalBackupFileName string `json:"DesktopTerminalBackupFileName"`
		} `json:"BackupFileInfo"`
	}
	if err := jsonUnmarshal(resp.Data, &data); err != nil {
		b.SubmitSend(func() { b.SendReply(msg, "解析账户信息失败") })
		return
	}

	maskUID := func(uid int64) string {
		s := fmt.Sprintf("%d", uid)
		if len(s) >= 6 {
			return s[:3] + "****" + s[len(s)-3:]
		}
		return s
	}
	maskMobile := func(m string) string {
		if len(m) == 11 {
			return m[:3] + "****" + m[len(m)-4:]
		}
		return m
	}
	spaceProgress := func(used, total int64) string {
		if total == 0 {
			return "□□□□□□□□□□ (0%)"
		}
		ratio := float64(used) / float64(total)
		filled := int(ratio * 10)
		bar := strings.Repeat("▓", filled) + strings.Repeat("░", 10-filled)
		return fmt.Sprintf("%s (%.1f%%)", bar, ratio*100)
	}

	msgText := "🚀 123云盘信息\n\n"
	msgText += fmt.Sprintf("👤 账户信息\n    ├─ 昵称：%s %s\n", data.Nickname, boolEmoji(data.Vip, "🎖️VIP", ""))
	msgText += fmt.Sprintf("    ├─ 账户ID：%s\n", maskUID(data.UID))
	msgText += fmt.Sprintf("    ├─ 手机号：%s\n", maskMobile(data.Passport))
	msgText += fmt.Sprintf("    └─ 微信绑定：%s\n\n", boolEmoji(data.BindWechat, "✅已绑", "❌未绑"))
	msgText += fmt.Sprintf("💾 存储空间 %s\n", spaceProgress(data.SpaceUsed, data.SpacePermanent))
	msgText += fmt.Sprintf("    ├─ 已用：%s\n", FormatSize(data.SpaceUsed, "GB"))
	msgText += fmt.Sprintf("    ├─ 永久：%s\n", FormatSize(data.SpacePermanent, "GB"))
	msgText += fmt.Sprintf("    └─ 文件总数：%s 个\n\n", thousands(data.FileCount))
	msgText += "💎 VIP会员\n"
	for i, vip := range data.VipInfo {
		symbol := "    └─"
		if i < len(data.VipInfo)-1 {
			symbol = "    ├─"
		}
		msgText += fmt.Sprintf("%s %s：%s → %s\n", symbol, vip.VipLabel, vip.StartTime, vip.EndTime)
	}
	msgText += "\n🚀 流量与功能\n"
	msgText += fmt.Sprintf("    ├─ 直连流量：%s\n", FormatSize(data.DirectTraffic, "GB"))
	msgText += fmt.Sprintf("    ├─ 分享流量：%s\n", FormatSize(data.ShareTraffic, "GB"))
	msgText += fmt.Sprintf("    └─ 直链功能：%s\n\n", boolEmoji(data.StraightLink, "✅开启", "❌关闭"))
	msgText += "📦 备份配置\n"
	msgText += fmt.Sprintf("    ├─ 移动端：%s\n", data.BackupFileInfo.MobileTerminalBackupFileName)
	msgText += fmt.Sprintf("    └─ 桌面端：%s", data.BackupFileInfo.DesktopTerminalBackupFileName)

	b.SubmitSend(func() { b.SendReply(msg, msgText) })
}

func (b *Bot) cmdOrganize(msg *tgbotapi.Message) {
	if msg.From.ID != b.adminID {
		b.SubmitSend(func() { b.SendReply(msg, "您没有权限使用此机器人。") })
		return
	}
	if !b.env.GetBool("ENV_TRANSFER_ENABLED", false) {
		b.SubmitSend(func() {
			b.SendMessage("❌ 文件整理功能未启用，请在配置中设置 ENV_TRANSFER_ENABLED=1")
		})
		return
	}
	executor := transfer.GetTransferExecutor()
	if executor == nil {
		b.SubmitSend(func() {
			b.SendMessage("❌ 文件整理功能未启用，请在配置中设置 ENV_TRANSFER_ENABLED=1")
		})
		return
	}

	args := strings.Fields(msg.CommandArguments())
	if len(args) == 0 {
		// 列出可整理的目录
		dirs := executor.DirHelper.GetMonitorDirs()
		if len(dirs) == 0 {
			b.SubmitSend(func() {
				b.SendMessage("❌ 无监控目录配置，请在 Web 界面或 config/directories.json 中配置")
			})
			return
		}
		lines := []string{"📂 可整理的目录（使用 /organize <PID> 触发）：", ""}
		for _, d := range dirs {
			lines = append(lines, fmt.Sprintf("📁 %s", d.Name))
			lines = append(lines, fmt.Sprintf("   源 PID: %d → 目标 PID: %d", d.SourcePID, d.LibraryPID))
			lines = append(lines, fmt.Sprintf("   类型: %s | 方式: %s", d.MediaType, d.TransferType))
			lines = append(lines, "")
		}
		b.SubmitSend(func() { b.SendMessage(strings.Join(lines, "\n")) })
		return
	}

	sourcePID, err := parseInt(args[0])
	if err != nil {
		b.SubmitSend(func() { b.SendMessage("❌ PID 必须是数字，用法: /organize <PID>") })
		return
	}
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
	}
	b.SubmitSend(func() { b.SendMessage(fmt.Sprintf("🔄 开始整理目录 %d（force=%v）...", sourcePID, force)) })

	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.SubmitSend(func() { b.SendMessage(fmt.Sprintf("❌ 整理失败: %v", r)) })
			}
		}()
		stats := executor.TransferDirectory(context.Background(), sourcePID, true, force, nil, "", true, nil)
		msg := fmt.Sprintf("✅ 整理完成: 目录 %d\n📊 成功 %d | 失败 %d | 跳过 %d", sourcePID, stats.Success, stats.Fail, stats.Skip)
		if len(stats.FailList) > 0 {
			n := len(stats.FailList)
			if n > 5 {
				n = 5
			}
			fails := make([]string, 0, n)
			for _, f := range stats.FailList[:n] {
				fails = append(fails, fmt.Sprintf("  • %s: %s", f[0], f[1]))
			}
			msg += "\n❌ 失败列表（前5条）:\n" + strings.Join(fails, "\n")
		}
		b.SubmitSend(func() { b.SendMessage(msg) })
	}()
}

func (b *Bot) cmdHistory(msg *tgbotapi.Message) {
	if msg.From.ID != b.adminID {
		b.SubmitSend(func() { b.SendReply(msg, "您没有权限使用此机器人。") })
		return
	}
	if !b.env.GetBool("ENV_TRANSFER_ENABLED", false) {
		b.SubmitSend(func() { b.SendMessage("❌ 文件整理功能未启用") })
		return
	}
	executor := transfer.GetTransferExecutor()
	if executor == nil {
		b.SubmitSend(func() { b.SendMessage("❌ 文件整理功能未启用") })
		return
	}

	args := strings.Fields(msg.CommandArguments())
	keyword := ""
	page := 1
	for _, arg := range args {
		if p, err := parseInt(arg); err == nil {
			page = p
		} else {
			keyword = arg
		}
	}
	items := executor.History.List(20, (page-1)*20, "", keyword)
	total := executor.History.Count("")
	if len(items) == 0 {
		b.SubmitSend(func() { b.SendMessage("📭 暂无整理历史记录") })
		return
	}
	lines := []string{fmt.Sprintf("📋 整理历史（第 %d 页，共 %d 条）:", page, total), ""}
	for _, item := range items {
		statusEmoji := "❌"
		if item.Status == "success" {
			statusEmoji = "✅"
		}
		title := item.MediaTitle
		if title == "" {
			title = item.FileName
		}
		if item.MediaYear != "" {
			lines = append(lines, fmt.Sprintf("%s %s (%s)", statusEmoji, title, item.MediaYear))
		} else {
			lines = append(lines, fmt.Sprintf("%s %s", statusEmoji, title))
		}
		if item.TargetPath != "" {
			lines = append(lines, fmt.Sprintf("   → %s", item.TargetPath))
		}
		lines = append(lines, fmt.Sprintf("   🕐 %s", truncateStr(item.TransferTime, 19)))
		lines = append(lines, "")
	}
	lines = append(lines, "使用 /history <关键词> <页码> 查看更多")
	b.SubmitSend(func() { b.SendMessage(strings.Join(lines, "\n")) })
}

func (b *Bot) cmdTransferStatus(msg *tgbotapi.Message) {
	if msg.From.ID != b.adminID {
		b.SubmitSend(func() { b.SendReply(msg, "您没有权限使用此机器人。") })
		return
	}
	if !b.env.GetBool("ENV_TRANSFER_ENABLED", false) {
		b.SubmitSend(func() { b.SendMessage("❌ 文件整理功能未启用（设置 ENV_TRANSFER_ENABLED=1）") })
		return
	}
	executor := transfer.GetTransferExecutor()
	if executor == nil {
		b.SubmitSend(func() { b.SendMessage("❌ 文件整理功能未启用（设置 ENV_TRANSFER_ENABLED=1）") })
		return
	}
	scheduler := transfer.GetTransferScheduler()

	lines := []string{"📊 文件整理功能状态:", ""}
	schedRunning := scheduler != nil && scheduler.IsRunning()
	scanning := scheduler != nil && scheduler.IsScanning()
	lines = append(lines, fmt.Sprintf("🔧 调度器: %s", boolEmoji(schedRunning, "运行中", "未运行")))
	interval := 0
	if scheduler != nil {
		interval = scheduler.ScanInterval()
	}
	lines = append(lines, fmt.Sprintf("⏱️ 扫描间隔: %d 分钟", interval))
	lines = append(lines, fmt.Sprintf("🔄 正在扫描: %s", boolEmoji(scanning, "是", "否")))
	lines = append(lines, fmt.Sprintf("🎬 TMDB: %s", boolEmoji(executor.TMDB != nil, "已配置", "未配置")))
	lines = append(lines, fmt.Sprintf("📄 刮削: %s", boolEmoji(executor.Scraper != nil, "启用", "禁用")))
	lines = append(lines, fmt.Sprintf("📈 历史记录: %d 条", executor.History.Count("")))

	dirs := executor.DirHelper.GetMonitorDirs()
	if len(dirs) > 0 {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("📂 监控目录 (%d 个):", len(dirs)))
		for _, d := range dirs {
			lines = append(lines, fmt.Sprintf("  • %s: %d → %d", d.Name, d.SourcePID, d.LibraryPID))
		}
	}
	b.SubmitSend(func() { b.SendMessage(strings.Join(lines, "\n")) })
}

func boolEmoji(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func thousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func parseInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}
