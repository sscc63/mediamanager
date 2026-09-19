// Package web 实现 Web 面板（对应 server.py）。
// 使用标准库 net/http 提供服务：登录/会话、配置管理、TMDB 榜单、整理控制、STRM/MediaWarp/Emby 状态与操作、系统内存趋势。
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/bot"
	"mmbot/internal/config"
)

// 除登录/登出与 STRM 跳转（播放器带 apikey 直连）外，其余 /api/* 一律要求已登录。
var publicAPIPaths = []string{"/api/login", "/api/logout", "/api/strm/redirect"}

// Server Web 面板服务。
type Server struct {
	env        *config.Config
	bot        *bot.Bot // 可选：用于 ENV_FILTER 热更新
	envPath    string   // db/config/user.env
	tplEnvPath string   // templete.env
	staticDir  string
	tplDir     string
	mwDataDir  string // MediaWarp 数据目录绝对路径
	logPath    string // 日志文件路径（日志动态接口读取）

	adminUser string
	adminPass string

	// 会话采用客户端签名 cookie（非持久化 secret），容器重启后旧 cookie 失效，强制重新登录
	sessionSecret []byte

	memMu       sync.Mutex
	memHistory  [][2]float64
	memLastSave time.Time
	memStop     chan struct{}

	httpSrv *http.Server
}

// configFilePath 实际 user.env 绝对路径（envGet 兜底读取用，New 时同步）。
var configFilePath = "config/user.env"

// New 创建 Web 服务。
func New(env *config.Config, opts ...Option) *Server {
	s := &Server{
		env:        env,
		envPath:    env.Get("ENV_FILE_PATH", "config/user.env"),
		tplEnvPath: "config/templete.env",
		staticDir:  "static",
		tplDir:     "templates",
		memStop:    make(chan struct{}),
		memHistory: make([][2]float64, 0, memHistoryMax),
	}
	for _, o := range opts {
		o(s)
	}
	configFilePath = s.envPath
	s.adminUser = env.Get("ENV_WEB_PASSPORT", "admin")
	s.adminPass = env.Get("ENV_WEB_PASSWORD", "password")
	s.ensureSecret()
	return s
}

// ensureSecret 每次启动生成随机密钥，不持久化，容器重启后旧 cookie 失效，强制重新登录。
func (s *Server) ensureSecret() {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	s.sessionSecret = buf
}

// Option 配置项。
type Option func(*Server)

// WithBot 注入 Bot 实例（用于 ENV_FILTER 热更新）。
func WithBot(b *bot.Bot) Option { return func(s *Server) { s.bot = b } }

// WithPaths 设置配置文件/静态目录路径。
func WithPaths(envPath, tplEnvPath, staticDir, tplDir string) Option {
	return func(s *Server) {
		if envPath != "" {
			s.envPath = envPath
		}
		if tplEnvPath != "" {
			s.tplEnvPath = tplEnvPath
		}
		if staticDir != "" {
			s.staticDir = staticDir
		}
		if tplDir != "" {
			s.tplDir = tplDir
		}
	}
}

// WithMwDataDir 设置 MediaWarp 数据目录绝对路径。
func WithMwDataDir(dir string) Option {
	return func(s *Server) { s.mwDataDir = dir }
}

// WithLogPath 设置日志文件路径（日志动态接口读取）。
func WithLogPath(path string) Option {
	return func(s *Server) { s.logPath = path }
}

// Start 启动 HTTP 服务（addr 如 ":8090"），并启动内存采样协程。
func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	go s.memSamplerLoop()

	s.httpSrv = &http.Server{Addr: addr, Handler: mux}
	log.Printf("[Web] 面板启动，监听 %s", addr)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown 停止服务。
func (s *Server) Shutdown(ctx context.Context) {
	close(s.memStop)
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(ctx)
	}
}

// registerRoutes 注册全部路由。
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// 页面
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /transfer", s.handleTransferRedirect)
	// 静态文件
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.staticDir))))

	// 认证
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	// 配置
	mux.HandleFunc("GET /api/env", s.authWrap(s.handleGetEnv))
	mux.HandleFunc("POST /api/env", s.authWrap(s.handleSaveEnv))
	mux.HandleFunc("POST /api/restart", s.authWrap(s.handleRestart))

	// TMDB
	mux.HandleFunc("GET /api/tmdb", s.authWrap(s.handleTMDBSearch))
	mux.HandleFunc("POST /api/tmdb/subscribe", s.authWrap(s.handleTMDBSubscribe))
	mux.HandleFunc("GET /api/tmdb/detail", s.authWrap(s.handleTMDBDetail))
	mux.HandleFunc("GET /api/maoyan", s.authWrap(s.handleMaoyanRank))
	mux.HandleFunc("GET /api/media/episodes", s.authWrap(s.handleMediaEpisodes))
	mux.HandleFunc("GET /api/media/calendar", s.authWrap(s.handleMediaCalendar))

	// 资源站（123panfx）：详情页「资源」区块的搜索与解锁
	mux.HandleFunc("GET /api/panfx/search", s.authWrap(s.handlePanfxSearch))
	mux.HandleFunc("POST /api/panfx/unlock", s.authWrap(s.handlePanfxUnlock))

	// 整理
	mux.HandleFunc("GET /api/transfer/status", s.authWrap(s.handleTransferStatus))
	mux.HandleFunc("GET /api/media/stats", s.authWrap(s.handleMediaStats))
	mux.HandleFunc("GET /api/transfer/dirs", s.authWrap(s.handleTransferGetDirs))
	mux.HandleFunc("POST /api/transfer/dirs", s.authWrap(s.handleTransferSaveDirs))
	mux.HandleFunc("POST /api/transfer/run", s.authWrap(s.handleTransferRun))
	mux.HandleFunc("GET /api/transfer/history", s.authWrap(s.handleTransferHistory))
	mux.HandleFunc("DELETE /api/transfer/history/{id}", s.authWrap(s.handleTransferDeleteHistory))
	mux.HandleFunc("POST /api/transfer/history/batch-delete", s.authWrap(s.handleTransferBatchDelete))
	mux.HandleFunc("POST /api/transfer/history/retry", s.authWrap(s.handleHistoryRetry))
	mux.HandleFunc("GET /api/transfer/categories", s.authWrap(s.handleTransferCategories))
	mux.HandleFunc("GET /api/transfer/config", s.authWrap(s.handleTransferGetConfig))
	mux.HandleFunc("POST /api/transfer/config", s.authWrap(s.handleTransferSaveConfig))

	// OAuth / 系统
	mux.HandleFunc("GET /api/oauth/status", s.authWrap(s.handleOAuthStatus))
	mux.HandleFunc("GET /api/system/usage", s.authWrap(s.handleSystemUsage))
	mux.HandleFunc("GET /api/logs", s.authWrap(s.handleLogs))

	// STRM
	mux.HandleFunc("GET /api/strm/status", s.authWrap(s.handleStrmStatus))
	mux.HandleFunc("GET /api/strm/config", s.authWrap(s.handleStrmGetConfig))
	mux.HandleFunc("POST /api/strm/config", s.authWrap(s.handleStrmSaveConfig))
	mux.HandleFunc("POST /api/strm/run", s.authWrap(s.handleStrmRun))
	mux.HandleFunc("GET /api/strm/redirect", s.handleStrmRedirect)

	// MediaWarp
	mux.HandleFunc("GET /api/mediawarp/status", s.authWrap(s.handleMwareStatus))
	mux.HandleFunc("GET /api/mediawarp/config", s.authWrap(s.handleMwareGetConfig))
	mux.HandleFunc("POST /api/mediawarp/config", s.authWrap(s.handleMwareSaveConfig))

	// Emby 查重
	mux.HandleFunc("GET /api/emby/status", s.authWrap(s.handleEmbyStatus))
	mux.HandleFunc("POST /api/emby/scan", s.authWrap(s.handleEmbyScan))
	mux.HandleFunc("POST /api/emby/delete", s.authWrap(s.handleEmbyDelete))

	// 后台任务 / Emby 观看统计
	mux.HandleFunc("GET /api/tasks/status", s.authWrap(s.handleTasksStatus))
	mux.HandleFunc("POST /api/monitor/trigger", s.authWrap(s.handleMonitorTrigger))
	mux.HandleFunc("GET /api/monitor/recent", s.authWrap(s.handleChannelRecent))
	mux.HandleFunc("POST /api/monitor/transfer", s.authWrap(s.handleChannelTransfer))
	mux.HandleFunc("GET /api/emby/usage", s.authWrap(s.handleEmbyUsage))
	mux.HandleFunc("GET /api/emby/usage/detail", s.authWrap(s.handleEmbyUsageDetail))
}

// authWrap 校验登录态（/api/* 除公开路径外）。
func (s *Server) authWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, p := range publicAPIPaths {
			if r.URL.Path == p {
				next(w, r)
				return
			}
		}
		if !s.isLoggedIn(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		next(w, r)
	}
}

// ---------- 会话管理（客户端签名 cookie，无服务端状态，重启后 session 不失效） ----------

const sessionCookieName = "session"

// hmacSHA256 计算 HMAC-SHA256 十六进制摘要。
func hmacSHA256(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

// sessionPayload 签名 cookie 载荷：username.expiry
func (s *Server) signSession(payload string) string {
	return hmacSHA256(s.sessionSecret, payload)
}

func (s *Server) isLoggedIn(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	token := cookie.Value
	idx := strings.LastIndex(token, ".")
	if idx < 0 {
		return false
	}
	payload, sig := token[:idx], token[idx+1:]
	if s.signSession(payload) != sig {
		return false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

func (s *Server) createSession(w http.ResponseWriter) {
	exp := time.Now().Add(24 * time.Hour).Unix()
	payload := fmt.Sprintf("%s.%d", s.adminUser, exp)
	token := payload + "." + s.signSession(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
	})
}

func (s *Server) destroySession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
}

// ---------- 页面 ----------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.isLoggedIn(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.tplDir, "index.html"))
	if err != nil {
		http.Error(w, "index.html 不存在", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	_, _ = w.Write(data)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.tplDir, "login.html"))
	if err != nil {
		http.Error(w, "login.html 不存在", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	_, _ = w.Write(data)
}

func (s *Server) handleTransferRedirect(w http.ResponseWriter, r *http.Request) {
	if !s.isLoggedIn(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/#transfer", http.StatusSeeOther)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
		return
	}
	if data.Username == s.adminUser && data.Password == s.adminPass {
		s.createSession(w)
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": false})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.destroySession(w, r)
	if r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// envGet 读取环境变量（进程环境变量优先，其次配置文件 user.env）。
func envGet(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v, ok := readEnvFileValue(configFilePath, key); ok {
		return v
	}
	return def
}

// readEnvFileValue 读取 user.env 中单个键的值（支持值内换行的多行值）。
func readEnvFileValue(path, key string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if strings.TrimSpace(k) != key {
			continue
		}
		value := strings.TrimSpace(v)
		// 收集续行（后续不以 # 开头且不含 = 的行）
		for j := i + 1; j < len(lines); j++ {
			ns := strings.TrimSpace(lines[j])
			if ns == "" || strings.HasPrefix(ns, "#") || strings.Contains(ns, "=") {
				break
			}
			value += "\n" + ns
		}
		return unquoteEnv(value), true
	}
	return "", false
}

func unquoteEnv(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
