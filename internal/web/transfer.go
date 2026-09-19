package web

// 文件整理功能 API（对应 server.py 的整理相关路由）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"mmbot/internal/strm"
	"mmbot/internal/transfer"
)

// ---------- GET /api/transfer/status ----------

func (s *Server) handleTransferStatus(w http.ResponseWriter, r *http.Request) {
	executor := transfer.GetTransferExecutor()
	if executor == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "整理功能未启用或未初始化（请在配置中设置 ENV_TRANSFER_ENABLED=1）",
		})
		return
	}
	scheduler := transfer.GetTransferScheduler()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":           true,
		"scheduler_running": scheduler != nil && scheduler.IsRunning(),
		"is_scanning":       scheduler != nil && scheduler.IsScanning(),
		"scan_interval_min": schedulerInterval(scheduler),
		"tmdb_configured":   executor.TMDB != nil,
		"scrape_enabled":    executor.Scraper != nil,
		"history_count":     executor.History.Count(""),
	})
}

func schedulerInterval(s *transfer.TransferScheduler) int {
	if s == nil {
		return 0
	}
	return s.ScanInterval()
}

// ---------- GET/POST /api/transfer/dirs ----------

func dirsFilePath() string {
	p := envGet("ENV_TRANSFER_DIRS_FILE", "")
	if p == "" {
		return "config/directories.json"
	}
	return p
}

func (s *Server) handleTransferGetDirs(w http.ResponseWriter, r *http.Request) {
	helper := transfer.NewDirectoryHelper(dirsFilePath())
	dirs := helper.LoadDirs(true)
	var out []map[string]any
	for _, d := range dirs {
		out = append(out, map[string]any{
			"name":           d.Name,
			"source_pid":     d.SourcePID,
			"library_pid":    d.LibraryPID,
			"media_type":     d.MediaType,
			"media_category": d.MediaCategory,
			"priority":       d.Priority,
			"monitor":        d.Monitor,
			"transfer_type":  d.TransferType,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"dirs": out})
}

func (s *Server) handleTransferSaveDirs(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Dirs []map[string]any `json:"dirs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	helper := transfer.NewDirectoryHelper(dirsFilePath())
	if helper.SaveDirs(data.Dirs) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "保存失败"})
}

// ---------- POST /api/transfer/run ----------

func (s *Server) handleTransferRun(w http.ResponseWriter, r *http.Request) {
	executor := transfer.GetTransferExecutor()
	if executor == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "整理功能未启用"})
		return
	}
	if executor.Client == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "123 客户端未初始化，请检查 ENV_123_CLIENT_ID/SECRET 配置"})
		return
	}
	scheduler := transfer.GetTransferScheduler()

	var data struct {
		FileID    any    `json:"file_id"`
		SourcePID int    `json:"source_pid"`
		Force     bool   `json:"force"`
		FileName  string `json:"file_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	ctx := context.Background()

	if data.FileID != nil {
		fileIDStr := toIDString(data.FileID)
		if fileIDStr == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效 file_id"})
			return
		}
		var result transfer.TransferResult
		if data.FileName == "" {
			result = executor.TransferFileByID(ctx, fileIDStr, data.SourcePID, data.Force)
		} else {
			result = executor.TransferFile(ctx, fileIDStr, data.FileName, data.SourcePID, data.Force, nil, "", 0, nil, nil)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     result.Success,
			"file_id":     result.FileID,
			"file_name":   result.FileName,
			"target_path": result.TargetPath,
			"media_title": result.MediaTitle,
			"message":     result.Message,
			"skipped":     result.Skipped,
		})
		return
	}

	if data.SourcePID > 0 {
		stats := executor.TransferDirectory(ctx, data.SourcePID, true, data.Force, nil, "", false, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":       true,
			"success_count": stats.Success,
			"fail_count":    stats.Fail,
			"skip_count":    stats.Skip,
			"fail_list":     truncateFailList(stats.FailList, 20),
		})
		return
	}

	if scheduler != nil {
		result := scheduler.ScanNow(0)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":       true,
			"total_dirs":    result["total_dirs"],
			"success_count": result["success"],
			"fail_count":    result["fail"],
			"skip_count":    result["skip"],
		})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "调度器未初始化"})
}

func toIDString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func truncateFailList(list [][2]string, n int) []map[string]string {
	if len(list) > n {
		list = list[:n]
	}
	var out []map[string]string
	for _, item := range list {
		out = append(out, map[string]string{"name": item[0], "error": item[1]})
	}
	return out
}

// ---------- GET /api/transfer/history ----------

func (s *Server) handleTransferHistory(w http.ResponseWriter, r *http.Request) {
	page := 1
	size := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil && v > 0 {
		size = v
	}
	keyword := r.URL.Query().Get("keyword")
	status := r.URL.Query().Get("status")

	executor := transfer.GetTransferExecutor()
	if executor == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "page": page, "size": size, "total": 0})
		return
	}

	items := executor.History.List(size, (page-1)*size, status, keyword)
	total := executor.History.CountFilter(status, keyword)

	var out []map[string]any
	for _, it := range items {
		out = append(out, map[string]any{
			"id":            it.ID,
			"file_id":       it.FileID,
			"file_name":     it.FileName,
			"source_pid":    it.SourcePID,
			"target_pid":    it.TargetPID,
			"target_path":   it.TargetPath,
			"media_title":   it.MediaTitle,
			"media_year":    it.MediaYear,
			"media_type":    it.MediaType,
			"tmdb_id":       nullInt64(it.TMDBID),
			"season":        nullInt64(it.Season),
			"episode":       nullInt64(it.Episode),
			"file_size":     it.FileSize,
			"version":       it.Version,
			"status":        it.Status,
			"error_msg":     it.ErrorMsg,
			"transfer_type": it.TransferType,
			"transfer_time": it.TransferTime,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out, "page": page, "size": size, "total": total,
	})
}

func nullInt64(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

// ---------- DELETE /api/transfer/history/{id} ----------

func (s *Server) handleTransferDeleteHistory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效 id"})
		return
	}
	executor := transfer.GetTransferExecutor()
	if executor == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "整理功能未启用"})
		return
	}
	if executor.History.Delete(id) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "记录不存在"})
}

// ---------- GET /api/transfer/categories ----------

func (s *Server) handleTransferCategories(w http.ResponseWriter, r *http.Request) {
	mtype := r.URL.Query().Get("type")
	yamlPath := envGet("ENV_TRANSFER_CATEGORY_YAML", "")
	if yamlPath == "" {
		yamlPath = "config/category.yaml"
	}
	helper := transfer.NewCategoryHelper(yamlPath)
	if mtype != "" {
		writeJSON(w, http.StatusOK, map[string]any{"categories": helper.ListCategories(mtype)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"movie": helper.ListCategories("movie"),
		"tv":    helper.ListCategories("tv"),
	})
}

// ---------- GET/POST /api/transfer/config ----------

func (s *Server) handleTransferGetConfig(w http.ResponseWriter, r *http.Request) {
	envConfig := readEnvKeys(s.envPath, transferEnvKeys)
	helper := transfer.NewDirectoryHelper(dirsFilePath())
	dirs := helper.LoadDirs(true)
	sourcePID, libraryPID := 0, 0
	if len(dirs) > 0 {
		sourcePID = dirs[0].SourcePID
		libraryPID = dirs[0].LibraryPID
	}
	transferType := "move"
	if v, ok := envConfig["ENV_TRANSFER_TYPE"]; ok {
		transferType = v
	}
	result := map[string]any{}
	for k, v := range envConfig {
		result[k] = v
	}
	result["source_pid"] = sourcePID
	result["library_pid"] = libraryPID
	result["transfer_type"] = transferType
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTransferSaveConfig(w http.ResponseWriter, r *http.Request) {
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	// 1. 写入 user.env（仅整理相关变量）
	envUpdates := map[string]string{}
	for _, key := range transferEnvKeys {
		if v, ok := data[key]; ok {
			envUpdates[key] = toStringValue(v)
		}
	}
	if len(envUpdates) > 0 {
		if !writeEnvBlock(s.envPath, transferEnvKeys, "# 文件整理功能配置", envUpdates) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入 user.env 失败"})
			return
		}
		// 热更新运行时，不重启进程
		transfer.ApplyConfig(envUpdates)
	}
	// 2. 写入 directories.json
	sourcePID := intVal(data["source_pid"])
	libraryPID := intVal(data["library_pid"])
	transferType := toStringValue(data["transfer_type"])
	if transferType == "" {
		transferType = "move"
	}
	if sourcePID > 0 && libraryPID > 0 {
		dirs := []map[string]any{{
			"name":           "待整理目录",
			"source_pid":     sourcePID,
			"library_pid":    libraryPID,
			"media_type":     "",
			"media_category": "",
			"priority":       1,
			"monitor":        true,
			"transfer_type":  transferType,
		}}
		helper := transfer.NewDirectoryHelper(dirsFilePath())
		if !helper.SaveDirs(dirs) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入 directories.json 失败"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "整理配置已保存"})
}

func intVal(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	}
	return 0
}

func toStringValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		if t {
			return "1"
		}
		return "0"
	}
	return ""
}

// readEnvKeys 读取 user.env 中指定键集的值。
func readEnvKeys(path string, keys []string) map[string]string {
	all := readAllEnvValues(path)
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	result := map[string]string{}
	for k, v := range all {
		if keySet[k] {
			result[k] = v
		}
	}
	return result
}

// writeEnvBlock 更新 user.env 中指定注释标记的配置块（保留其他配置不变）。
// 文件不存在时自动创建，避免保存失败。
func writeEnvBlock(path string, keys []string, blockTitle string, updates map[string]string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		data = []byte{}
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	titleKey := strings.TrimPrefix(blockTitle, "# ")
	for i, line := range lines {
		if strings.Contains(line, blockTitle) {
			start = i
			break
		}
	}
	var newBlock []string
	newBlock = append(newBlock, blockTitle+"\n")
	for _, key := range keys {
		if v, ok := updates[key]; ok {
			newBlock = append(newBlock, key+"="+v+"\n")
		}
	}
	newBlock = append(newBlock, "\n")

	var out []string
	if start >= 0 {
		end := start + 1
		for end < len(lines) {
			l := strings.TrimSpace(lines[end])
			if strings.HasPrefix(l, "# ") && !strings.Contains(l, titleKey) {
				break
			}
			end++
		}
		out = append(out, lines[:start]...)
		out = append(out, newBlock...)
		out = append(out, lines[end:]...)
	} else {
		out = append(out, lines...)
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, newBlock...)
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return false
	}
	return true
}

// ---------- POST /api/transfer/history/batch-delete ----------

func (s *Server) handleTransferBatchDelete(w http.ResponseWriter, r *http.Request) {
	var data struct {
		IDs    []int `json:"ids"`
		Linked bool  `json:"linked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	if len(data.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未选择记录"})
		return
	}

	executor := transfer.GetTransferExecutor()
	if executor == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "整理功能未启用"})
		return
	}

	if data.Linked {
		// 联动删除：获取记录详情，调用 DeleteItems 执行四件套
		records := executor.History.GetByIDs(data.IDs)
		if len(records) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未找到对应的记录（可能已被删除）"})
			return
		}
		var items []strm.DeleteItem
		for _, rec := range records {
			fid, _ := strconv.ParseInt(rec.FileID, 10, 64)
			log.Printf("[transfer] 深度删除记录: id=%d, file_id=%s, target_path=%s, file_name=%s", rec.ID, rec.FileID, rec.TargetPath, rec.FileName)
			items = append(items, strm.DeleteItem{
				Name:      rec.FileName,
				PanPath:   rec.TargetPath,
				Size:      rec.FileSize,
				PanFileID: fid,
			})
		}
		client := transferClient()
		emby := strm.GetEmbyRuntime()
		ctx := context.Background()
		result := strm.DeleteItems(ctx, client, items, nil, emby, envGet("ENV_STRM_PATHS", ""), executor.History)
		// 兜底：防止 PanFileID 解析失败或网盘删除失败导致历史记录残留
		executor.History.DeleteBatch(data.IDs)
		// 构造失败详情列表
		var failDetails []map[string]string
		for _, f := range result.FailList {
			failDetails = append(failDetails, map[string]string{"name": f.Name, "error": f.Error})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":   result.Success,
			"fail":      len(result.FailList),
			"fail_list": failDetails,
			"linked":    true,
		})
		return
	}

	// 仅删记录
	n := executor.History.DeleteBatch(data.IDs)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"deleted": n,
		"linked":  false,
	})
}

// ---------- POST /api/transfer/history/retry ----------

func (s *Server) handleHistoryRetry(w http.ResponseWriter, r *http.Request) {
	var data struct {
		IDs          []int  `json:"ids"`
		Title        string `json:"title"`
		Year         int    `json:"year"`
		Type         string `json:"type"`
		TransferType string `json:"transfer_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	if len(data.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未选择记录"})
		return
	}
	if strings.TrimSpace(data.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请填写片名"})
		return
	}

	executor := transfer.GetTransferExecutor()
	if executor == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "整理功能未启用"})
		return
	}
	records := executor.History.GetByIDs(data.IDs)
	if len(records) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未找到对应的记录（可能已被删除）"})
		return
	}

	results := []map[string]any{}
	okCount := 0
	// 收集本次成功/失败结果，统一走 STRM 联动 + 正式模板通知（与批量整理收尾一致）
	aggregated := transfer.NewTransferStats()
	for _, rec := range records {
		if rec.Status != "fail" {
			results = append(results, map[string]any{
				"id": rec.ID, "file_name": rec.FileName, "success": false,
				"message": "仅识别失败记录可手动识别（当前状态: " + rec.Status + "）",
			})
			continue
		}
		res := executor.RetryWithTitle(r.Context(), *rec, data.Title, data.Year, data.Type, data.TransferType, aggregated)
		if res.Success {
			okCount++
		}
		msg := res.Message
		if msg == "" {
			if res.Skipped {
				msg = "跳过"
			} else {
				msg = "已整理"
			}
		}
		results = append(results, map[string]any{
			"id": rec.ID, "file_name": rec.FileName, "success": res.Success,
			"skipped": res.Skipped, "message": msg, "media_title": res.MediaTitle,
		})
	}
	if okCount > 0 {
		executor.FinalizeTransfer(aggregated)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   len(records),
		"ok":      okCount,
		"fail":    len(records) - okCount,
		"results": results,
	})
}
