// Package mediawarp MediaWarp 反代运行时（对应 mediawarp_engine/runtime.py）。
//
// - 二进制：db/mediawarp/MediaWarp[.exe]，version.txt 记录版本，不符自动重下
// - 配置：  db/mediawarp/config/config.yaml（启动前用最新 env 全量生成）
// - PID：   db/mediawarp/pid（启动时复用存活的旧进程，防端口冲突）
// - 日志：  不落盘，stdout/stderr 管道逐行写入主日志
// - 看门狗：每 30s 检查进程存活，意外退出自动重启
package mediawarp

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"mmbot/internal/httpx"
)

// MediaWarp 相关环境变量名列表（用于读写 user.env）。
var EnvKeys = []string{
	"ENV_MWARP_ENABLED",
	"ENV_MWARP_PORT",
	"ENV_MWARP_MEDIASERVER_TYPE",
	"ENV_MWARP_MEDIASERVER_ADDR",
	"ENV_MWARP_MEDIASERVER_AUTH",
	"ENV_MWARP_STRM_PREFIXES",
}

// 固定版本（version.txt 记录，版本不符自动重下）。
const Version = "0.1.12"

// 本场景固定取值，不暴露配置：
const (
	FinalURL  = true  // 折叠 /api/strm/redirect -> 123 CDN 重定向链
	Transcode = false // 强制直链播放、禁用转码
)

// 数据目录（相对 123bot 运行目录）。
const DataDir = "mediawarp"

// Runtime MediaWarp 反代运行时（全局单例）。
type Runtime struct {
	Enabled      bool
	Port         int
	MServerType  string
	MServerAddr  string
	MServerAuth  string
	StrmPrefixes string
	BinPath      string
	Error        string

	dataDir string
	cmd     *exec.Cmd
	stopCh  chan struct{}
	mu      sync.Mutex
	pidFile string
}

// Config MediaWarp 初始化配置。
type Config struct {
	Enabled      bool
	Port         string
	MServerType  string
	MServerAddr  string
	MServerAuth  string
	StrmPrefixes string
	DataDir      string // 数据目录（绝对路径），为空时使用默认相对路径
}

// NewRuntime 创建 MediaWarp 运行时。
func NewRuntime(cfg Config) *Runtime {
	dd := cfg.DataDir
	if dd == "" {
		dd = DataDir
	}
	r := &Runtime{
		Enabled:      cfg.Enabled,
		MServerType:  strings.ToLower(strings.TrimSpace(cfg.MServerType)),
		MServerAddr:  cfg.MServerAddr,
		MServerAuth:  cfg.MServerAuth,
		StrmPrefixes: cfg.StrmPrefixes,
		dataDir:      dd,
		stopCh:       make(chan struct{}),
		pidFile:      filepath.Join(dd, "pid"),
	}
	if r.MServerType == "" {
		r.MServerType = "emby"
	}
	r.Port = toPort(cfg.Port)
	return r
}

func toPort(val string) int {
	if val == "" {
		return 9000
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil || n <= 0 {
		return 9000
	}
	return n
}

// ---------- 平台判定与二进制下载 ----------

type platformInfo struct {
	osName string
	arch   string
	ext    string
}

func detectPlatform() (platformInfo, error) {
	var p platformInfo
	switch runtime.GOOS {
	case "windows":
		p.osName, p.ext = "windows", "zip"
	case "linux":
		p.osName, p.ext = "linux", "tar.gz"
	case "darwin":
		p.osName, p.ext = "darwin", "tar.gz"
	default:
		return p, fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		p.arch = "amd64"
	case "arm64":
		p.arch = "arm64"
	default:
		return p, fmt.Errorf("不支持的 CPU 架构: %s，请手动放置 MediaWarp 二进制到 %s/ 目录后重试", runtime.GOARCH, DataDir)
	}
	return p, nil
}

// downloadAndExtract 从 GitHub Releases 下载并解压 MediaWarp 二进制。
func downloadAndExtract(dataDir string) bool {
	p, err := detectPlatform()
	if err != nil {
		log.Printf("MediaWarp: %v", err)
		return false
	}
	url := fmt.Sprintf("https://github.com/DDSRem-Dev/MediaWarp/releases/download/v%s/MediaWarp_%s_%s_%s.%s",
		Version, Version, p.osName, p.arch, p.ext)

	tempDir, err := os.MkdirTemp("", "mwarp-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tempDir)

	tempFile := filepath.Join(tempDir, "MediaWarp."+p.ext)
	log.Printf("正在下载 MediaWarp %s: %s", Version, url)
	if err := downloadFile(url, tempFile); err != nil {
		log.Printf("MediaWarp 下载失败: %v", err)
		return false
	}

	binName := "MediaWarp"
	if p.osName == "windows" {
		binName = "MediaWarp.exe"
	}
	binPath := filepath.Join(dataDir, binName)

	var extracted string
	if p.ext == "zip" {
		extracted, err = extractZipMember(tempFile, "MediaWarp.exe", tempDir)
	} else {
		extracted, err = extractTarMember(tempFile, "MediaWarp", tempDir)
	}
	if err != nil {
		log.Printf("MediaWarp 解压失败: %v", err)
		return false
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return false
	}
	if err := copyFile(extracted, binPath); err != nil {
		return false
	}
	if p.osName != "windows" {
		_ = os.Chmod(binPath, 0o755)
	}
	_ = os.WriteFile(filepath.Join(dataDir, "version.txt"), []byte(Version), 0o644)
	log.Printf("MediaWarp %s 安装完成: %s", Version, binPath)
	return true
}

func downloadFile(url, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	raw, _, err := httpx.New(60*time.Second).Get(ctx, url, nil)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, raw, 0o644)
}

func extractZipMember(zipPath, suffix, destDir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, suffix) {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			out := filepath.Join(destDir, filepath.Base(f.Name))
			fw, err := os.Create(out)
			if err != nil {
				return "", err
			}
			defer fw.Close()
			if _, err := io.Copy(fw, rc); err != nil {
				return "", err
			}
			return out, nil
		}
	}
	return "", fmt.Errorf("压缩包中未找到 %s", suffix)
}

func extractTarMember(tarPath, suffix, destDir string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(hdr.Name, suffix) {
			out := filepath.Join(destDir, filepath.Base(hdr.Name))
			fw, err := os.Create(out)
			if err != nil {
				return "", err
			}
			defer fw.Close()
			if _, err := io.Copy(fw, tr); err != nil {
				return "", err
			}
			return out, nil
		}
	}
	return "", fmt.Errorf("压缩包中未找到 %s", suffix)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ensureBinary 确保二进制存在且版本正确；返回可执行文件路径，失败返回空串。
func ensureBinary(dataDir string) string {
	binName := "MediaWarp"
	if runtime.GOOS == "windows" {
		binName = "MediaWarp.exe"
	}
	binPath := filepath.Join(dataDir, binName)
	if _, err := os.Stat(binPath); err == nil {
		current := ""
		if verBytes, err := os.ReadFile(filepath.Join(dataDir, "version.txt")); err == nil {
			current = strings.TrimSpace(string(verBytes))
		}
		if current == Version {
			return binPath
		}
		cur := current
		if cur == "" {
			cur = "未知"
		}
		log.Printf("MediaWarp 版本不符（%s -> %s），尝试更新", cur, Version)
	} else {
		log.Printf("MediaWarp 二进制不存在，尝试下载")
	}
	if downloadAndExtract(dataDir) {
		return binPath
	}
	log.Printf("请手动放置 MediaWarp 二进制到 %s/ 目录后重试", dataDir)
	return ""
}

// ---------- 配置生成 ----------

// GenerateConfig 生成 config.yaml 文本（仅核心字段，附加功能显式关闭）。
func GenerateConfig(cfg map[string]any) string {
	var prefixes []string
	for _, line := range strings.Split(toString(cfg["strm_prefixes"]), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			prefixes = append(prefixes, line)
		}
	}
	mserverType := strings.ToLower(strings.TrimSpace(toString(cfg["mserver_type"])))
	if mserverType == "" {
		mserverType = "emby"
	}
	mserverTypeName := "Emby"
	if mserverType == "jellyfin" {
		mserverTypeName = "Jellyfin"
	}

	config := map[string]any{
		"Port": toPort(toString(cfg["port"])),
		"MediaServer": map[string]any{
			"Type": mserverTypeName,
			"ADDR": strings.TrimSpace(toString(cfg["mserver_addr"])),
			"AUTH": strings.TrimSpace(toString(cfg["mserver_auth"])),
		},
		"Logger": map[string]any{
			"Level":         "Warning",
			"AccessLogger":  map[string]any{"Console": false, "File": false},
			"ServiceLogger": map[string]any{"Console": true, "File": false},
		},
		"Web": map[string]any{
			"Enable": false, "Custom": false, "Index": false, "Head": "",
			"ExternalPlayerUrl": false, "Crx": false, "ActorPlus": false,
			"FanartShow": false, "Danmaku": false, "VideoTogether": false,
		},
		"ClientFilter": map[string]any{"Enable": false, "Mode": "BlackList", "ClientList": []any{}},
		"HTTPStrm": map[string]any{
			"Enable":     len(prefixes) > 0,
			"TransCode":  Transcode,
			"FinalURL":   FinalURL,
			"PrefixList": prefixes,
		},
		"AlistStrm": map[string]any{"Enable": false, "TransCode": false, "RawURL": false, "List": []any{}},
		"Subtitle":  map[string]any{"Enable": false, "SRT2ASS": false, "ASSStyle": []any{}, "SubSet": false},
	}
	out, _ := yaml.Marshal(config)
	return uppercaseYAMLBooleans(string(out))
}

// uppercaseYAMLBooleans 把 YAML 文本中的 true/false 词元替换为大写（对齐 Python SafeDumper 输出）。
func uppercaseYAMLBooleans(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			continue
		}
		val := strings.TrimSpace(line[idx+1:])
		if val == "true" {
			lines[i] = line[:idx+1] + " True"
		} else if val == "false" {
			lines[i] = line[:idx+1] + " False"
		}
	}
	return strings.Join(lines, "\n")
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ---------- 进程管理 ----------

func (r *Runtime) readPID() int {
	data, err := os.ReadFile(r.pidFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

func (r *Runtime) writePID() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = os.WriteFile(r.pidFile, []byte(strconv.Itoa(r.cmd.Process.Pid)), 0o644)
	}
}

func (r *Runtime) clearPID() {
	_ = os.Remove(r.pidFile)
}

// isProcessAlive 判断进程是否存活（跨平台）。
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		out, err := runCmd("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH")
		if err != nil {
			return false
		}
		return strings.Contains(out, strconv.Itoa(pid))
	}
	// Unix: 直接探测 /proc/<pid> 是否存在（BusyBox 的 ps 不支持 -p，且更轻量）
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// processName 获取进程名（跨平台）。
func processName(pid int) string {
	if runtime.GOOS == "windows" {
		out, _ := runCmd("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
		parts := strings.Split(out, ",")
		if len(parts) >= 2 {
			return strings.Trim(parts[0], `"`)
		}
		return ""
	}
	out, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return strings.TrimSpace(string(out))
}

// existingAlive 返回可复用的存活 MediaWarp 进程 PID；无则返回 0。
func (r *Runtime) existingAlive() int {
	// 1) 按 pid 文件
	if pid := r.readPID(); pid > 0 {
		if isProcessAlive(pid) && strings.Contains(strings.ToLower(processName(pid)), "mediawarp") {
			return pid
		}
	}
	// 2) 按端口反查
	if r.Port > 0 {
		if pid := portListenPID(r.Port); pid > 0 {
			if strings.Contains(strings.ToLower(processName(pid)), "mediawarp") {
				log.Printf("端口 %d 由 MediaWarp（PID %d）监听，直接复用", r.Port, pid)
				return pid
			}
		}
	}
	return 0
}

// portListenPID 反查监听指定端口的进程 PID（跨平台）。
func portListenPID(port int) int {
	if runtime.GOOS == "windows" {
		out, err := runCmd("netstat", "-ano")
		if err != nil {
			return 0
		}
		want := fmt.Sprintf(":%d", port)
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, want) || !strings.Contains(line, "LISTEN") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
					return pid
				}
			}
		}
		return 0
	}
	// Unix: 纯 /proc 解析，零外部命令依赖（BusyBox 无 ss，netstat 也无 PID 列）。
	// 1) 在 /proc/net/tcp、/proc/net/tcp6 中找 LISTEN 且端口匹配的 socket inode
	inode := procListenInode(port)
	if inode <= 0 {
		return 0
	}
	// 2) 遍历 /proc/<pid>/fd 的 socket 符号链接反查持有者 PID
	target := fmt.Sprintf("socket:[%d]", inode)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, proc := range procs {
		if !proc.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(proc.Name()); err != nil {
			continue
		}
		fds, err := os.ReadDir("/proc/" + proc.Name() + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink("/proc/" + proc.Name() + "/fd/" + fd.Name())
			if err == nil && link == target {
				if pid, err := strconv.Atoi(proc.Name()); err == nil {
					return pid
				}
			}
		}
	}
	return 0
}

// procListenInode 返回监听 port 的 TCP socket inode；未监听返回 0。
func procListenInode(port int) int {
	want := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			if fields[3] != "0A" { // 非 LISTEN
				continue
			}
			addr := fields[1]
			colon := strings.LastIndex(addr, ":")
			if colon < 0 || addr[colon+1:] != want {
				continue
			}
			if inode, err := strconv.ParseInt(fields[9], 10, 64); err == nil {
				return int(inode)
			}
		}
	}
	return 0
}

// freePort 释放端口：若被 MediaWarp 进程占用则终止。
func (r *Runtime) freePort(port int) {
	if pid := portListenPID(port); pid > 0 {
		if strings.Contains(strings.ToLower(processName(pid)), "mediawarp") {
			log.Printf("端口 %d 被 MediaWarp（PID %d）占用，先停止旧进程", port, pid)
			killProcess(pid)
		}
	}
}

func killProcess(pid int) {
	if runtime.GOOS == "windows" {
		_, _ = runCmd("taskkill", "/PID", strconv.Itoa(pid), "/F")
		return
	}
	_, _ = runCmd("kill", "-9", strconv.Itoa(pid))
}

// spawnReaders stdout/stderr 读取线程：MediaWarp 日志逐行汇入主日志。
func (r *Runtime) spawnReaders() {
	if r.cmd == nil {
		return
	}
	if stdout, err := r.cmd.StdoutPipe(); err == nil {
		go readStream(stdout)
	}
	if stderr, err := r.cmd.StderrPipe(); err == nil {
		go readStream(stderr)
	}
}

func readStream(rc io.ReadCloser) {
	defer rc.Close()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if isBannerLine(line) {
			continue
		}
		if strings.Contains(line, "【INFO】") || strings.Contains(line, "[INFO]") {
			continue
		}
		log.Printf("[MediaWarp] %s", line)
	}
}

// isBannerLine 过滤 MediaWarp 启动 logo 与标题分隔线。
func isBannerLine(line string) bool {
	return strings.Contains(line, "█") || strings.Contains(line, "═") || strings.Count(line, "=") >= 8
}

// Start 启动 MediaWarp 进程；返回是否成功（失败时 Error 有原因）。
func (r *Runtime) Start() bool {
	r.Error = ""
	if r.MServerAddr == "" || r.MServerAuth == "" {
		r.Error = "未配置媒体服务器地址/API Key，无法启动"
		log.Printf("MediaWarp: %s", r.Error)
		return false
	}
	// 1) 确保二进制可用
	r.BinPath = ensureBinary(r.dataDir)
	if r.BinPath == "" {
		r.Error = "MediaWarp 二进制不可用（自动下载失败）"
		return false
	}
	// 2) 用最新配置全量生成 config.yaml
	configDir := filepath.Join(r.dataDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		r.Error = fmt.Sprintf("创建配置目录失败: %v", err)
		return false
	}
	configPath := filepath.Join(configDir, "config.yaml")
	cfgText := GenerateConfig(map[string]any{
		"port":          r.Port,
		"mserver_type":  r.MServerType,
		"mserver_addr":  r.MServerAddr,
		"mserver_auth":  r.MServerAuth,
		"strm_prefixes": r.StrmPrefixes,
	})
	if err := os.WriteFile(configPath, []byte(cfgText), 0o644); err != nil {
		r.Error = fmt.Sprintf("写入 config.yaml 失败: %v", err)
		return false
	}
	// 3) 复用存活的旧进程（防端口冲突）
	if pid := r.existingAlive(); pid > 0 {
		log.Printf("MediaWarp 进程已存在（PID %d），直接复用", pid)
		return true
	}
	// 3.5) 无可复用进程时先释放端口
	r.freePort(r.Port)
	// 4) 启动新进程（管道必须在 Start 前设置）
	cmd := exec.Command(r.BinPath, "--config", configPath)
	cmd.Dir = r.dataDir
	r.cmd = cmd
	r.spawnReaders()
	if err := cmd.Start(); err != nil {
		r.Error = fmt.Sprintf("启动失败: %v", err)
		log.Printf("MediaWarp: %s", r.Error)
		return false
	}
	r.writePID()

	// 5) 启动 Wait goroutine 获取退出状态、防止僵尸进程
	exitCh := make(chan int, 1)
	go func() {
		waitErr := cmd.Wait()
		code := -1
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
				log.Printf("MediaWarp 进程退出（PID %d, 退出码 %d）: %v", cmd.Process.Pid, code, exitErr)
			} else {
				log.Printf("MediaWarp 进程 Wait 返回: %v", waitErr)
			}
		} else {
			code = 0
			log.Printf("MediaWarp 进程正常退出（PID %d）", cmd.Process.Pid)
		}
		exitCh <- code
	}()

	// 6) 等待 2 秒检测进程是否立即退出
	select {
	case exitCode := <-exitCh:
		r.Error = fmt.Sprintf("启动后立即退出（退出码 %d），请检查 config.yaml 与上游媒体服务器配置", exitCode)
		log.Printf("MediaWarp: %s", r.Error)
		r.clearPID()
		return false
	case <-time.After(2 * time.Second):
		// 进程仍在运行，继续
	}

	log.Printf("✅ MediaWarp 反代已启动（端口 %d，PID %d）", r.Port, cmd.Process.Pid)
	return true
}

// Stop 停止 MediaWarp 进程。
func (r *Runtime) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}
	pid := 0
	if r.cmd != nil && r.cmd.Process != nil {
		pid = r.cmd.Process.Pid
	} else if p := r.readPID(); p > 0 {
		pid = p
	}
	if pid > 0 && isProcessAlive(pid) {
		killProcess(pid)
		log.Printf("🛑 MediaWarp 已停止（PID %d）", pid)
		r.clearPID()
	}
	r.cmd = nil
}

// Restart 重启 MediaWarp。
func (r *Runtime) Restart() bool {
	r.Stop()
	r.stopCh = make(chan struct{})
	return r.Start()
}

// ApplyEnv 用 user.env 的最新值更新运行时字段。
func (r *Runtime) ApplyEnv(get func(string, string) string) {
	if get == nil {
		get = func(k, d string) string { return d }
	}
	r.Port = toPort(get("ENV_MWARP_PORT", ""))
	r.MServerType = strings.ToLower(strings.TrimSpace(get("ENV_MWARP_MEDIASERVER_TYPE", "emby")))
	r.MServerAddr = get("ENV_MWARP_MEDIASERVER_ADDR", "")
	r.MServerAuth = get("ENV_MWARP_MEDIASERVER_AUTH", "")
	r.StrmPrefixes = get("ENV_MWARP_STRM_PREFIXES", "")
}

// Status 返回运行时状态。
func (r *Runtime) Status() map[string]any {
	running := false
	pid := 0
	if p := r.readPID(); p > 0 && isProcessAlive(p) && strings.Contains(strings.ToLower(processName(p)), "mediawarp") {
		running = true
		pid = p
	} else if r.cmd != nil && r.cmd.Process != nil {
		running = true
		pid = r.cmd.Process.Pid
	}
	binaryOK := r.BinPath != ""
	if !binaryOK {
		binName := "MediaWarp"
		if runtime.GOOS == "windows" {
			binName = "MediaWarp.exe"
		}
		if _, err := os.Stat(filepath.Join(r.dataDir, binName)); err == nil {
			binaryOK = true
		}
	}
	return map[string]any{
		"enabled":      r.Enabled,
		"running":      running,
		"pid":          pid,
		"port":         r.Port,
		"mserver_type": r.MServerType,
		"mserver_addr": r.MServerAddr,
		"binary_ok":    binaryOK,
		"version":      Version,
		"error":        r.Error,
	}
}

// ToConfig 返回运行时配置。
func (r *Runtime) ToConfig() map[string]string {
	boolStr := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		"ENV_MWARP_ENABLED":          boolStr(r.Enabled),
		"ENV_MWARP_PORT":             strconv.Itoa(r.Port),
		"ENV_MWARP_MEDIASERVER_TYPE": r.MServerType,
		"ENV_MWARP_MEDIASERVER_ADDR": r.MServerAddr,
		"ENV_MWARP_MEDIASERVER_AUTH": r.MServerAuth,
		"ENV_MWARP_STRM_PREFIXES":    r.StrmPrefixes,
	}
}

// StartWatchdog 启动看门狗（进程意外退出自动重启）。
func (r *Runtime) StartWatchdog() {
	go r.watchdogLoop()
}

func (r *Runtime) watchdogLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	crashCount := 0
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			pid := r.readPID()
			if pid > 0 && !isProcessAlive(pid) {
				crashCount++
				backoff := time.Duration(crashCount) * 30 * time.Second
				if backoff > 5*time.Minute {
					backoff = 5 * time.Minute
				}
				log.Printf("MediaWarp 进程意外退出（累计崩溃 %d 次），%v 后尝试自动重启...", crashCount, backoff)
				r.clearPID()
				time.Sleep(backoff)
				if r.Start() {
					crashCount = 0
				}
			}
		}
	}
}

// ---------- 工具 ----------

func runCmd(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------- 全局实例 ----------

var (
	rtMu   sync.RWMutex
	rtInst *Runtime
)

// GetRuntime 获取全局 MediaWarp 运行时。
func GetRuntime() *Runtime {
	rtMu.RLock()
	defer rtMu.RUnlock()
	return rtInst
}

// SetRuntime 设置全局 MediaWarp 运行时。
func SetRuntime(r *Runtime) {
	rtMu.Lock()
	rtInst = r
	rtMu.Unlock()
}

// InitFromEnv 从环境变量初始化 MediaWarp 反代；返回是否启用。
// dataDir 为 MediaWarp 数据目录的绝对路径，传空串使用默认相对路径。
func InitFromEnv(get func(string, string) string, dataDir string) bool {
	if get == nil {
		get = func(k, d string) string { return d }
	}
	enabled := get("ENV_MWARP_ENABLED", "0") == "1"
	if !enabled {
		log.Printf("🚫 MediaWarp 反代未启用（ENV_MWARP_ENABLED=0）")
		SetRuntime(nil)
		return false
	}
	cfg := Config{
		Enabled:      true,
		Port:         get("ENV_MWARP_PORT", "9000"),
		MServerType:  get("ENV_MWARP_MEDIASERVER_TYPE", "emby"),
		MServerAddr:  get("ENV_MWARP_MEDIASERVER_ADDR", ""),
		MServerAuth:  get("ENV_MWARP_MEDIASERVER_AUTH", ""),
		StrmPrefixes: get("ENV_MWARP_STRM_PREFIXES", ""),
		DataDir:      dataDir,
	}
	rt := NewRuntime(cfg)
	if rt.Start() {
		rt.StartWatchdog()
		SetRuntime(rt)
		return true
	}
	SetRuntime(rt) // 保留实例，便于前端展示错误原因
	return false
}

// Shutdown 123bot 退出/优雅重启时停止 MediaWarp 进程。
func Shutdown() {
	if r := GetRuntime(); r != nil {
		r.Stop()
	}
}
