package web

// 配置管理 API（对应 server.py 的 /api/env、/api/restart、user.env 读写工具）。

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"mmbot/internal/mediawarp"
	"mmbot/internal/strm"
	"mmbot/internal/transfer"
)

// 整理相关的环境变量名列表（用于读写 user.env）。
var transferEnvKeys = []string{
	"ENV_TRANSFER_ENABLED",
	"ENV_TRANSFER_TMDB_LANGUAGE",
	"ENV_TRANSFER_SCAN_INTERVAL",
	"ENV_TRANSFER_SCRAPE",
	"ENV_TRANSFER_MOVIE_FORMAT",
	"ENV_TRANSFER_TV_FORMAT",
	"ENV_TRANSFER_CATEGORY_YAML",
	"ENV_TRANSFER_DIRS_FILE",
	"ENV_TRANSFER_TYPE",
	"ENV_TRANSFER_SKIP_EXTS",
	"ENV_TRANSFER_MIN_FILESIZE",
	"ENV_TRANSFER_PRIORITY_VERSIONS",
	"ENV_TRANSFER_SIZE_OVERRIDE_RATIO",
	// AI 标题清洗兜底
	"ENV_AI_ENABLED",
	"ENV_AI_API_KEY",
	"ENV_AI_BASE_URL",
	"ENV_AI_MODEL",
	"ENV_AI_DAILY_QUOTA",
}

// STRM 相关环境变量名列表。
var strmEnvKeys = []string{
	"ENV_STRM_ENABLED",
	"ENV_STRM_SERVER_ADDRESS",
	"ENV_STRM_PATHS",
	"ENV_STRM_MEDIAEXT",
	"ENV_STRM_DL_EXT",
	"ENV_STRM_OVERWRITE",
	"ENV_STRM_FULL_SYNC",
	"ENV_STRM_FULL_SYNC_CRON",
	"ENV_STRM_TRANSFER_LINKED",
	"ENV_STRM_CONCURRENCY",
}

// MediaWarp 相关环境变量名列表。
var mwarpEnvKeys = []string{
	"ENV_MWARP_ENABLED",
	"ENV_MWARP_PORT",
	"ENV_MWARP_MEDIASERVER_TYPE",
	"ENV_MWARP_MEDIASERVER_ADDR",
	"ENV_MWARP_MEDIASERVER_AUTH",
	"ENV_MWARP_STRM_PREFIXES",
}

// 重启回调（由 main 注入；未注入时直接退出进程）。
var restartCallback func()

// SetRestartCallback 设置重启回调。
func SetRestartCallback(cb func()) { restartCallback = cb }

// envConfigItem 单个配置项。
type envConfigItem struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Comment string `json:"comment"`
}

// ---------- GET /api/env ----------

// handleGetEnv 读取模板文件结构和注释，填充 user.env 的实际值。
func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.tplEnvPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Template file not found"})
		return
	}

	templateStructure := map[string][]envConfigItem{}
	var templateOrder []string
	currentSection := ""
	currentComment := ""

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "## ") {
			currentSection = strings.TrimSpace(line[2:])
			if _, ok := templateStructure[currentSection]; !ok {
				templateStructure[currentSection] = []envConfigItem{}
				templateOrder = append(templateOrder, currentSection)
			}
			currentComment = ""
		} else if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "## ") {
				currentComment = strings.TrimSpace(line[3:])
			} else {
				currentComment = strings.TrimSpace(line[2:])
			}
		} else if strings.Contains(line, "=") {
			key := strings.TrimSpace(line[:strings.Index(line, "=")])
			if currentSection != "" {
				templateStructure[currentSection] = append(templateStructure[currentSection], envConfigItem{
					Key:     key,
					Value:   "",
					Comment: currentComment,
				})
			}
			currentComment = ""
		}
	}

	envValues := readAllEnvValues(s.envPath)
	for section := range templateStructure {
		for i := range templateStructure[section] {
			if v, ok := envValues[templateStructure[section][i].Key]; ok {
				templateStructure[section][i].Value = v
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sections": templateStructure,
		"order":    templateOrder,
	})
}

// readAllEnvValues 读取 user.env 全部键值（支持值内换行）。
func readAllEnvValues(path string) map[string]string {
	result := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		key := strings.TrimSpace(k)
		value := strings.TrimSpace(v)
		for j := i + 1; j < len(lines); j++ {
			ns := strings.TrimSpace(lines[j])
			if ns == "" || strings.HasPrefix(ns, "#") || strings.Contains(ns, "=") {
				break
			}
			value += "\n" + ns
		}
		result[key] = unquoteEnv(value)
	}
	return result
}

// ---------- POST /api/env ----------

// handleSaveEnv 保存 user.env（保留未在前端提交的变量），restart=true 时延迟退出触发容器重启。
// body 结构：{"restart": bool, "<章节名>": [{"key","value","comment"}], ...}
func (s *Server) handleSaveEnv(w http.ResponseWriter, r *http.Request) {
	raw := map[string]json.RawMessage{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		log.Printf("[Web] /api/env 保存失败: body 解析错误: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	restart := true
	if rv, ok := raw["restart"]; ok {
		var b bool
		if err := json.Unmarshal(rv, &b); err == nil {
			restart = b
		}
		delete(raw, "restart")
	}

	sections := map[string][]envConfigItem{}
	for name, itemsRaw := range raw {
		var items []envConfigItem
		if err := json.Unmarshal(itemsRaw, &items); err != nil {
			log.Printf("[Web] /api/env 保存失败: 章节 %s 不是配置项数组: %v", name, err)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "章节 " + name + " 格式错误"})
			return
		}
		sections[name] = items
	}

	submittedKeys := map[string]bool{}
	for _, items := range sections {
		for _, item := range items {
			submittedKeys[item.Key] = true
		}
	}
	preserved := map[string]string{}
	for k, v := range readAllEnvValues(s.envPath) {
		if !submittedKeys[k] {
			preserved[k] = v
		}
	}
	transferSet, strmSet, mwarpSet := setOf(transferEnvKeys), setOf(strmEnvKeys), setOf(mwarpEnvKeys)
	var transferPreserved, strmPreserved, mwarpPreserved, otherPreserved []string
	for k, v := range preserved {
		switch {
		case transferSet[k]:
			transferPreserved = append(transferPreserved, k+"="+v)
		case strmSet[k]:
			strmPreserved = append(strmPreserved, k+"="+v)
		case mwarpSet[k]:
			mwarpPreserved = append(mwarpPreserved, k+"="+v)
		default:
			otherPreserved = append(otherPreserved, k+"="+v)
		}
	}

	// 补全运行时中存在但文件中缺失的 key（如新增的 ENV_STRM_CONCURRENCY）
	if rt := strm.GetRuntime(); rt != nil {
		cfg := rt.Config()
		log.Printf("[Env] 运行时 STRM config: %v", cfg)
		for _, key := range strmEnvKeys {
			if _, ok := preserved[key]; !ok {
				if v, ok := cfg[key]; ok {
					preserved[key] = v
					strmPreserved = append(strmPreserved, key+"="+v)
					log.Printf("[Env] 从运行时补全缺失 key: %s=%s", key, v)
				}
			}
		}
	}
	log.Printf("[Env] preserved: %d keys, strmPreserved: %d keys", len(preserved), len(strmPreserved))
	if rt := mediawarp.GetRuntime(); rt != nil {
		cfg := rt.ToConfig()
		for _, key := range mwarpEnvKeys {
			if _, ok := preserved[key]; !ok {
				if v, ok := cfg[key]; ok {
					preserved[key] = v
					mwarpPreserved = append(mwarpPreserved, key+"="+v)
				}
			}
		}
	}

	if err := os.MkdirAll(dirOf(s.envPath), 0o755); err != nil {
		log.Printf("[Web] /api/env 保存失败: 创建目录 %s 失败: %v", dirOf(s.envPath), err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var b strings.Builder
	for section, items := range sections {
		writeEnvSection(&b, section, items)
	}
	writePreservedSection(&b, "文件整理功能配置", transferPreserved)
	writePreservedSection(&b, "保留的配置（未在前端显示）", otherPreserved)
	writePreservedSection(&b, "STRM 功能配置", strmPreserved)
	writePreservedSection(&b, "MediaWarp 反代配置", mwarpPreserved)
	if err := os.WriteFile(s.envPath, []byte(b.String()), 0o644); err != nil {
		log.Printf("[Web] /api/env 保存失败: 写入 %s 失败: %v", s.envPath, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if !restart {
		// 热更新环境变量，不重启进程
		changedKeys := map[string]string{}
		for _, items := range sections {
			for _, item := range items {
				os.Setenv(item.Key, item.Value)
				changedKeys[item.Key] = item.Value
			}
		}
		// 热更新整理引擎相关配置
		transfer.ApplyConfig(changedKeys)
		// 热更新 STRM 运行时配置
		if rt := strm.GetRuntime(); rt != nil {
			rt.ApplyConfig(changedKeys)
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "restart": false})
		return
	}
	log.Printf("配置已保存，程序将退出以触发容器重启...")
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
	scheduleRestart()
}

// handleRestart 手动重启服务（不保存配置）。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if restartCallback != nil {
		log.Printf("收到手动重启请求，执行优雅重启")
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "graceful": true})
	} else {
		log.Printf("收到手动重启请求（无回调），程序将退出以触发重启...")
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	}
	scheduleRestart()
}

// ---------- 工具 ----------

func writeEnvSection(b *strings.Builder, title string, items []envConfigItem) {
	b.WriteString("# " + title + "\n")
	for _, item := range items {
		if item.Comment != "" {
			b.WriteString("## " + item.Comment + "\n")
		}
		b.WriteString(item.Key + "=" + item.Value + "\n")
	}
	b.WriteString("\n")
}

func writePreservedSection(b *strings.Builder, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	b.WriteString("# " + title + "\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

func scheduleRestart() {
	go func() {
		time.Sleep(time.Second)
		if restartCallback != nil {
			restartCallback()
			return
		}
		os.Exit(0)
	}()
}

func setOf(keys []string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func dirOf(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "."
	}
	return p[:i]
}

// hotUpdateFilter 热更新运行中 Bot 的关键词正则。
func (s *Server) hotUpdateFilter(newValue string) bool {
	if s.bot == nil {
		return false
	}
	s.bot.SetFilter(newValue)
	return true
}

// addFilterKeyword 把关键词追加到 ENV_FILTER，返回 (成功, 消息, 新值)。
func (s *Server) addFilterKeyword(keyword string) (bool, string, string) {
	data, err := os.ReadFile(s.envPath)
	if err != nil {
		return false, "配置文件 " + s.envPath + " 不存在", ""
	}
	lines := strings.Split(string(data), "\n")
	found, existed := false, false
	var parts []string
	newLines := make([]string, 0, len(lines))
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "ENV_FILTER=") {
			found = true
			current := strings.TrimSpace(strings.TrimPrefix(stripped, "ENV_FILTER="))
			parts = splitFilter(current)
			for _, p := range parts {
				if strings.EqualFold(p, keyword) {
					existed = true
					break
				}
			}
			if !existed {
				parts = append(parts, keyword)
				newLines = append(newLines, "ENV_FILTER="+strings.Join(parts, "|"))
			} else {
				newLines = append(newLines, line)
			}
		} else {
			newLines = append(newLines, line)
		}
	}
	if !found {
		parts = []string{keyword}
		newLines = append(newLines, "ENV_FILTER="+keyword)
	}
	if err := os.WriteFile(s.envPath, []byte(strings.Join(newLines, "\n")), 0o644); err != nil {
		return false, "写入配置文件失败: " + err.Error(), ""
	}
	newValue := strings.Join(parts, "|")
	_ = os.Setenv("ENV_FILTER", newValue)
	runtimeUpdated := s.hotUpdateFilter(newValue)

	if existed {
		return true, "该关键词已在频道监控选择关键词中", newValue
	}
	if runtimeUpdated {
		return true, "已订阅：" + keyword + "，已加入频道监控选择关键词并即时生效", newValue
	}
	return true, "已订阅：" + keyword + "，已加入频道监控选择关键词（重启后完全生效）", newValue
}

// removeFilterKeyword 从 ENV_FILTER 中移除关键词，返回 (成功, 消息, 新值)。
func (s *Server) removeFilterKeyword(keyword string) (bool, string, string) {
	data, err := os.ReadFile(s.envPath)
	if err != nil {
		return false, "配置文件 " + s.envPath + " 不存在", ""
	}
	lines := strings.Split(string(data), "\n")
	found, existed := false, false
	var parts []string
	newLines := make([]string, 0, len(lines))
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "ENV_FILTER=") {
			found = true
			current := strings.TrimSpace(strings.TrimPrefix(stripped, "ENV_FILTER="))
			parts = splitFilter(current)
			for _, p := range parts {
				if strings.EqualFold(p, keyword) {
					existed = true
					break
				}
			}
			var newParts []string
			for _, p := range parts {
				if !strings.EqualFold(p, keyword) {
					newParts = append(newParts, p)
				}
			}
			parts = newParts
			newLines = append(newLines, "ENV_FILTER="+strings.Join(newParts, "|"))
		} else {
			newLines = append(newLines, line)
		}
	}
	if !found {
		return false, "未找到 ENV_FILTER 配置", ""
	}
	if err := os.WriteFile(s.envPath, []byte(strings.Join(newLines, "\n")), 0o644); err != nil {
		return false, "写入配置文件失败: " + err.Error(), ""
	}
	newValue := strings.Join(parts, "|")
	_ = os.Setenv("ENV_FILTER", newValue)
	runtimeUpdated := s.hotUpdateFilter(newValue)

	if !existed {
		return true, "该关键词不在订阅列表中", newValue
	}
	if runtimeUpdated {
		return true, "已取消订阅：" + keyword + "，已从频道监控选择关键词中移除并即时生效", newValue
	}
	return true, "已取消订阅：" + keyword + "，已从频道监控选择关键词中移除（重启后完全生效）", newValue
}

func splitFilter(v string) []string {
	var out []string
	for _, p := range strings.Split(v, "|") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
