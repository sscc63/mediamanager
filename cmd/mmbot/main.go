// 123bot Go 重构版入口：加载配置、初始化日志、启动 Telegram Bot / 频道监控 / Web 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mmbot/internal/bot"
	"mmbot/internal/config"
	"mmbot/internal/logx"
	"mmbot/internal/mediawarp"
	"mmbot/internal/pan123"
	"mmbot/internal/strm"
	"mmbot/internal/transfer"
	"mmbot/internal/web"
)

var (
	envFile        string
	tplFile        string
	webDir         string
	dataDir        string
	logPath        string
	scanDuplicates bool
	startTime      = time.Now()
)

func init() {
	// 默认相对可执行文件定位（与工作目录无关）
	exe, _ := os.Executable()
	base := filepath.Dir(exe)
	flag.StringVar(&envFile, "env", filepath.Join(base, "config", "user.env"), "用户配置文件路径")
	flag.StringVar(&tplFile, "template", filepath.Join(base, "config", "templete.env"), "配置模板路径")
	flag.StringVar(&webDir, "web", filepath.Join(base, "static"), "前端静态目录")
	flag.StringVar(&dataDir, "data", base, "数据目录")
	flag.StringVar(&logPath, "log", filepath.Join(base, "log", "log.log"), "日志文件路径")
	flag.BoolVar(&scanDuplicates, "scan-duplicates", false, "查重扫描子进程模式：执行扫描后退出")
}

func main() {
	flag.Parse()

	// 查重扫描子进程模式：一次性扫描后退出，完整回收内存
	if scanDuplicates {
		initLogging(logPath)
		strm.RunScanCLI(envFile)
		return
	}

	// 初始化日志（按天 + 按大小轮转）
	initLogging(logPath)
	log.Printf("[启动] mediamanager-go 版本 %s 开始启动", version)

	env, err := config.Load(envFile)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("[配置] 配置文件: %s", envFile)

	// 同步配置到进程环境变量，供 Web 层 envGet（os.Getenv 优先）等读取，
	// 避免重启后前端读不到已保存的 key（如 ENV_TMDB_API_KEY、ENV_MWARP_MEDIASERVER_AUTH）。
	// Config 已是"文件优先"的最终值（文件缺失时才用进程环境变量补充），此处以 Config 为准覆盖。
	for k, v := range env.All() {
		_ = os.Setenv(k, v)
	}

	run(env)
}

func run(env *config.Config) {
	// ===== 初始化 123 云盘客户端 =====
	client := init123Client(env)
	if client != nil {
		log.Printf("✅ [客户端] 123 云盘客户端已就绪")
	}

	// ===== 初始化文件整理引擎 =====
	transfer.InitFromEnv(client, env)
	// 整理完成后联动生成 STRM（若 STRM 功能启用）
	if sched := transfer.GetScheduler(); sched != nil {
		sched.OnTransferCompleted = strm.OnTransferCompleted
	}

	// ===== 初始化 STRM 功能 =====
	strm.InitFromEnv(client, func(k string) string { return env.Get(k, "") })
	// 设置 Emby 运行时（STRM 联动/查重通知使用）
	if ec := strm.NewEmbyClient(env.Get("ENV_MWARP_MEDIASERVER_ADDR", ""), env.Get("ENV_MWARP_MEDIASERVER_AUTH", "")); ec != nil {
		strm.SetEmbyRuntime(ec)
	}
	// 注册整理去重覆盖时的旧版本删除回调（完整四件套：本地 STRM + 网盘回收站 + Emby + 整理历史）
	strm.SetupTransferDeleteCallback()

	// ===== 初始化 MediaWarp 反代 =====
	mediawarp.InitFromEnv(env.Get, filepath.Join(dataDir, "mediawarp"))

	// ===== 初始化并启动 Telegram Bot =====
	b := bot.New(env)
	if client != nil {
		b.SetClient(client)
	}
	// 每次启动（容器重启/进程重启）向管理员发送启动通知
	b.SetOnStarted(func() {
		b.SubmitSend(func() {
			b.SendMessage(fmt.Sprintf("🚀 Media/manager已启动(%s)", version))
		})
	})
	b.Start()
	log.Printf("✅ [Bot] Telegram Bot 已启动（admin=%d）", env.GetInt("ENV_TG_ADMIN_USER_ID", 0))

	// 注入整理通知回调：发送到管理员私聊（有海报 URL 时带图，对应 Python 的 _transfer_notify）
	transfer.SetNotifyCallback(func(message, imageURL string) {
		b.SubmitSend(func() {
			if imageURL != "" {
				b.SendPhoto(b.AdminID(), imageURL, message)
			} else {
				b.SendMessage(message)
			}
		})
	})
	log.Printf("[通知] 整理通知回调已注入")

	// ===== 启动频道监控 =====
	if env.GetInt("ENV_AUTHORIZATION", 0) == 1 {
		b.StartMonitor()
		log.Printf("✅ [监控] 频道监控已启动")
	}

	// ===== Web 服务 =====
	templatesDir := "templates"
	if webDir != "" {
		templatesDir = filepath.Join(filepath.Dir(webDir), "templates")
	}
	tplFileActual := tplFile
	if _, err := os.Stat(tplFileActual); err != nil {
		if _, err := os.Stat("templete.env"); err == nil {
			tplFileActual = "templete.env"
		}
	}
	srv := web.New(env,
		web.WithBot(b),
		web.WithPaths(envFile, tplFileActual, webDir, templatesDir),
		web.WithMwDataDir(filepath.Join(dataDir, "mediawarp")),
		web.WithLogPath(logPath),
	)
	web.SetRestartCallback(func() {
		log.Printf("[Web] 收到重启请求，进程退出...")
		mediawarp.Shutdown()
		os.Exit(0)
	})
	webPort := env.Get("ENV_WEB_PORT", "8000")
	if !strings.HasPrefix(webPort, ":") {
		webPort = ":" + webPort
	}
	go func() {
		if err := srv.Start(webPort); err != nil {
			log.Printf("[Web] Web 服务异常退出: %v", err)
		}
	}()
	log.Printf("✅ [启动] Web 面板监听 %s", webPort)

	// 主进程常驻
	select {}
}

// init123Client 初始化 123 云盘客户端（优先 config/config.txt 持久化 token，其次 client_id/client_secret）。
func init123Client(env *config.Config) *pan123.Client {
	tokenPath := filepath.Join(dataDir, "config", "config.txt")
	if data, err := os.ReadFile(tokenPath); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			c := pan123.New(token)
			if resp, err := c.UserInfo(context.Background()); err == nil && resp.Code == 0 {
				log.Printf("✅ [客户端] 使用持久化 token 初始化成功")
				return c
			}
			log.Printf("[客户端] 持久化 token 已失效，将重新获取")
			_ = os.Remove(tokenPath)
		}
	}
	clientID := env.Get("ENV_123_CLIENT_ID", "")
	clientSecret := env.Get("ENV_123_CLIENT_SECRET", "")
	if clientID == "" || clientSecret == "" {
		log.Printf("[客户端] 未配置 ENV_123_CLIENT_ID/SECRET 或持久化 token，稍后由 Bot 按需初始化")
		return nil
	}
	c, err := pan123.NewWithSecret(clientID, clientSecret)
	if err != nil {
		log.Printf("[客户端] 获取 123 token 失败: %v", err)
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(tokenPath), 0o755)
	_ = os.WriteFile(tokenPath, []byte(c.Token), 0o644)
	log.Printf("✅ [客户端] 通过 client_id/client_secret 获取 token 成功")
	return c
}

func initLogging(path string) {
	w, err := logx.NewRotatingWriter(path, 10*1024*1024)
	if err != nil {
		log.Printf("[日志] 无法打开日志文件 %s: %v", path, err)
		return
	}
	// 同时输出到文件 + stdout（让 docker logs 能看到完整日志）
	multi := io.MultiWriter(w, os.Stdout)
	log.SetOutput(multi)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("")
}

var version = "0.9.42"

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
