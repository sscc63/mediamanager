package web

// OAuth 状态、系统占用、STRM/MediaWarp/Emby 状态与操作 API（对应 server.py 剩余路由）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"mmbot/internal/gcguard"
	"mmbot/internal/mediawarp"
	"mmbot/internal/pan123"
	"mmbot/internal/strm"
	"mmbot/internal/transfer"
)

// getEnvAdapter 适配 strm 包的 getEnv 签名。
func getEnvAdapter(key, def string) string { return envGet(key, def) }

// ---------- GET /api/oauth/status ----------

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile("data/oauth_token.json")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"valid":      false,
			"message":    "未认证",
		})
		return
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if json.Unmarshal(data, &token) != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"valid":      false,
			"message":    "未认证",
		})
		return
	}
	if token.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"valid":      false,
			"message":    "未认证",
		})
		return
	}
	now := time.Now().Unix()
	if token.ExpiresAt > now {
		remainingDays := (token.ExpiresAt - now) / 86400
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":        true,
			"valid":             true,
			"has_refresh_token": token.RefreshToken != "",
			"expires_at":        token.ExpiresAt,
			"expires_str":       time.Unix(token.ExpiresAt, 0).Format("2006-01-02 15:04:05"),
			"remaining_days":    remainingDays,
			"message":           fmt.Sprintf("已认证，剩余 %d 天", remainingDays),
		})
		return
	}
	expiredMsg := "，需重新认证"
	if token.RefreshToken != "" {
		expiredMsg = "，可尝试自动刷新"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":        true,
		"valid":             false,
		"has_refresh_token": token.RefreshToken != "",
		"expires_at":        token.ExpiresAt,
		"expires_str":       time.Unix(token.ExpiresAt, 0).Format("2006-01-02 15:04:05"),
		"message":           "Token 已过期" + expiredMsg,
	})
}

// ---------- GET /api/system/usage ----------

func (s *Server) handleSystemUsage(w http.ResponseWriter, r *http.Request) {
	memMB := readMemMB()
	s.memMu.Lock()
	hist := append([][2]float64(nil), s.memHistory...)
	s.memMu.Unlock()
	if len(hist) > memHistoryMax {
		hist = hist[len(hist)-memHistoryMax:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"process": map[string]any{"memory_mb": memMB},
		"history": hist,
	})
}

// ---------- STRM ----------

func (s *Server) handleStrmStatus(w http.ResponseWriter, r *http.Request) {
	rt := strm.GetRuntime()
	if rt == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "STRM 功能未启用（ENV_STRM_ENABLED=1）",
		})
		return
	}
	writeJSON(w, http.StatusOK, rt.Status())
}

func (s *Server) handleStrmGetConfig(w http.ResponseWriter, r *http.Request) {
	// 从文件读取配置
	result := readEnvKeys(s.envPath, strmEnvKeys)
	// 运行时配置优先于文件配置（运行时持有热更新后的值）
	if rt := strm.GetRuntime(); rt != nil {
		runtimeCfg := rt.Config()
		for k, v := range runtimeCfg {
			result[k] = v
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStrmSaveConfig(w http.ResponseWriter, r *http.Request) {
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	updates := map[string]string{}
	for _, key := range strmEnvKeys {
		if v, ok := data[key]; ok {
			updates[key] = toStringValue(v)
		}
	}
	log.Printf("[STRM] handleStrmSaveConfig 收到数据: data=%v, updates=%v", data, updates)
	if len(updates) > 0 {
		if !writeEnvBlock(s.envPath, strmEnvKeys, "# STRM 功能配置", updates) {
			log.Printf("[STRM] writeEnvBlock 失败: path=%s", s.envPath)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入 user.env 失败"})
			return
		}
		log.Printf("[STRM] writeEnvBlock 成功: path=%s", s.envPath)
	}
	// 同步进程环境变量（确保 os.Getenv 能读到最新值）
	for k, v := range updates {
		os.Setenv(k, v)
	}
	// 热更新运行时，不重启进程
	if rt := strm.GetRuntime(); rt != nil {
		rt.ApplyConfig(updates)
		log.Printf("[STRM] 运行时热更新成功")
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "STRM 配置已保存（已热更新）"})
}

func (s *Server) handleStrmRun(w http.ResponseWriter, r *http.Request) {
	rt := strm.GetRuntime()
	if rt == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "STRM 功能未启用"})
		return
	}
	var data struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(r.Body).Decode(&data)
	switch data.Action {
	case "full_sync":
		if !rt.TriggerFullSync() {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "STRM 任务正在执行中，请稍后再试"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "message": "任务 full_sync 已触发"})
	case "catalog":
		if !rt.TriggerCatalog() {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "STRM 梳理入册正在执行中，请稍后再试"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "message": "任务 catalog 已触发"})
	case "rewrite":
		if !rt.TriggerRewrite() {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "STRM 改写正在执行中，请稍后再试"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "message": "本地 STRM 地址改写已触发"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知操作: " + data.Action})
	}
}

// handleStrmRedirect STRM 302 跳转（公开接口，无需登录）。
func (s *Server) handleStrmRedirect(w http.ResponseWriter, r *http.Request) {
	rt := strm.GetRuntime()
	if rt == nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "STRM 功能未启用"})
		return
	}
	q := r.URL.Query()
	apikey := q.Get("apikey")
	if !rt.CheckAPIKey(apikey) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "apikey 无效"})
		return
	}
	size := int64(0)
	if v := q.Get("size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			size = n
		}
	}
	url, err := rt.GetRedirect(context.Background(), q.Get("name"), size, q.Get("md5"), q.Get("s3_key_flag"), r.Header.Get("User-Agent"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if url == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "获取下载链接失败"})
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// ---------- MediaWarp ----------

func (s *Server) handleMwareStatus(w http.ResponseWriter, r *http.Request) {
	rt := mediawarp.GetRuntime()
	if rt == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "MediaWarp 反代未启用（请先在页面打开「启用反代」开关并保存配置）",
		})
		return
	}
	writeJSON(w, http.StatusOK, rt.Status())
}

func (s *Server) handleMwareGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, readEnvKeys(s.envPath, mwarpEnvKeys))
}

func (s *Server) handleMwareSaveConfig(w http.ResponseWriter, r *http.Request) {
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	updates := map[string]string{}
	for _, key := range mwarpEnvKeys {
		if v, ok := data[key]; ok {
			updates[key] = toStringValue(v)
		}
	}
	log.Printf("[MediaWarp] handleMwareSaveConfig 收到数据: data=%v, updates=%v", data, updates)
	if len(updates) > 0 {
		if !writeEnvBlock(s.envPath, mwarpEnvKeys, "# MediaWarp 反代配置", updates) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入 user.env 失败"})
			return
		}
		// 同步进程环境变量，确保后续 os.Getenv 能读到最新值
		for k, v := range updates {
			os.Setenv(k, v)
		}
	}
	// 按「启用反代」开关立即启停进程
	enabled := toStringValue(updates["ENV_MWARP_ENABLED"]) == "1"
	// 直接使用 updates 构建配置，避免经过 envGet 的间接路径
	if enabled {
		// 停止旧实例
		if rt := mediawarp.GetRuntime(); rt != nil {
			rt.Stop()
			mediawarp.SetRuntime(nil)
		}
		cfg := mediawarp.Config{
			Enabled:      true,
			Port:         updates["ENV_MWARP_PORT"],
			MServerType:  updates["ENV_MWARP_MEDIASERVER_TYPE"],
			MServerAddr:  updates["ENV_MWARP_MEDIASERVER_ADDR"],
			MServerAuth:  updates["ENV_MWARP_MEDIASERVER_AUTH"],
			StrmPrefixes: updates["ENV_MWARP_STRM_PREFIXES"],
			DataDir:      s.mwDataDir,
		}
		rt := mediawarp.NewRuntime(cfg)
		if rt.Start() {
			rt.StartWatchdog()
			mediawarp.SetRuntime(rt)
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "MediaWarp 反代已启动"})
		} else {
			mediawarp.SetRuntime(rt) // 保留实例，便于前端展示错误原因
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": rt.Error})
		}
	} else {
		// 关闭反代
		if rt := mediawarp.GetRuntime(); rt != nil {
			rt.Stop()
			mediawarp.SetRuntime(nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "MediaWarp 反代已停止"})
	}
}

// ---------- Emby 查重 ----------

func (s *Server) handleEmbyStatus(w http.ResponseWriter, r *http.Request) {
	rt := strm.EnsureScanRuntime(getEnvAdapter)
	if rt == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "busy": false})
		return
	}
	// 每次状态查询都从文件刷新结果 + 检查锁文件
	rt.ReloadScanResult()
	hasResult := rt.ScanResult != nil
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":  rt.Configured,
		"busy":        rt.Busy,
		"last_scan":   rt.LastScan,
		"scan_result": rt.ScanResult,
		"scan_error":  rt.ScanError,
	})
	// 查重结果只为本次响应服务；避免主进程长期持有整个重复文件明细。
	// ClearScanResult 之后这批明细才真正变成垃圾，此时才适合主动回收。
	// 回收按全进程统一节流（见 internal/gcguard）：本接口前端每 2 秒轮询一次，
	// 不节流等于每 2 秒强制一次 STW GC，RSS 被反复打到低位再涨回来。
	if hasResult {
		rt.ClearScanResult()
		gcguard.Reclaim()
	}
}

func (s *Server) handleEmbyScan(w http.ResponseWriter, r *http.Request) {
	// 获取当前可执行文件路径，用于启动子进程
	selfExe, err := os.Executable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "无法获取可执行文件路径"})
		return
	}
	errMsg := strm.RunScan(selfExe)
	if errMsg != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": errMsg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleEmbyDelete(w http.ResponseWriter, r *http.Request) {
	var data struct {
		IDs []any `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	if len(data.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未选择文件"})
		return
	}
	rt := strm.GetScanRuntime()
	if rt == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "媒体洗版运行时未初始化"})
		return
	}
	rt.ReloadScanResult()
	scanResult := rt.ScanResult
	if scanResult == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请先执行查重扫描"})
		return
	}

	// 从最近扫描结果中映射 id -> 文件信息
	idMap := map[string]strm.DupFile{}
	for _, v := range scanResult {
		groups, ok := v.([]strm.DupGroup)
		if !ok {
			continue
		}
		for _, g := range groups {
			for _, f := range g.Files {
				idMap[f.ID] = f
			}
		}
	}
	var items []strm.DeleteItem
	for _, id := range data.IDs {
		idStr := toIDString(id)
		f, ok := idMap[idStr]
		if !ok {
			continue
		}
		items = append(items, strm.DeleteItem{
			ID:   f.ID,
			Name: f.Name,
			Path: f.Path,
			Size: f.Size,
		})
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未找到对应的扫描记录（结果已过期，请重新扫描）"})
		return
	}

	client := transferClient()
	embyClient := strm.GetEmbyRuntime()
	var history *transfer.TransferHistory
	if ex := transfer.GetTransferExecutor(); ex != nil {
		history = ex.History
	}
	result := strm.DeleteItems(context.Background(), client, items, nil, embyClient, envGet("ENV_STRM_PATHS", ""), history)
	// 同步更新内存结果
	rt.RemoveItems()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": result.Success,
		"fail":    len(result.FailList),
	})
}

// transferClient 获取 123 云盘客户端（复用整理执行器实例）。
func transferClient() *pan123.Client {
	if ex := transfer.GetTransferExecutor(); ex != nil && ex.Client != nil {
		return ex.Client
	}
	return nil
}
