// STRM 功能运行时（对应 strm_engine/runtime.py）。
package strm

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/pan123"
	"mmbot/internal/transfer"
)

// 默认值
const (
	DefaultAPIKey   = "sscc123bot"
	DefaultMediaext = "mp4,mkv,ts,iso,rmvb,avi,mov,mpeg,mpg,wmv,3gp,asf,m4v,flv,m2ts,tp,f4v"
	DefaultDL_Ext   = "srt,ssa,ass"
)

// StrmRuntime STRM 功能运行时（全局单例）。
type StrmRuntime struct {
	client          *pan123.Client
	Enabled         bool
	ServerAddress   string
	APIKey          string
	Paths           string
	Mediaext        string
	DlExt           string
	Overwrite       string
	FullSyncEnabled bool
	FullSyncCron    string
	CatalogEnabled  bool
	CatalogCron     string
	TransferLinked  bool
	Concurrency     int

	mu              sync.Mutex
	busy            bool
	catalogBusy     bool
	rewriteBusy     bool
	stopCh          chan struct{}
	nextFullSync    time.Time
	nextCatalog     time.Time
	lastCatalogRes  *CatalogResult // 最近一次 catalog 结果（完成后非 nil）
	lastCatalogTime time.Time
}

// StrmConfig STRM 初始化配置。
type StrmConfig struct {
	Enabled         bool
	ServerAddress   string
	Paths           string
	Mediaext        string
	DlExt           string
	Overwrite       string
	FullSyncEnabled bool
	FullSyncCron    string
	CatalogEnabled  bool
	CatalogCron     string
	TransferLinked  bool
	Concurrency     int
}

// NewStrmRuntime 创建 STRM 运行时。
func NewStrmRuntime(client *pan123.Client, cfg StrmConfig) *StrmRuntime {
	r := &StrmRuntime{
		client:          client,
		Enabled:         cfg.Enabled,
		ServerAddress:   strings.TrimRight(cfg.ServerAddress, "/"),
		APIKey:          DefaultAPIKey,
		Paths:           cfg.Paths,
		Mediaext:        cfg.Mediaext,
		DlExt:           cfg.DlExt,
		Overwrite:       cfg.Overwrite,
		FullSyncEnabled: cfg.FullSyncEnabled,
		FullSyncCron:    cfg.FullSyncCron,
		CatalogEnabled:  cfg.CatalogEnabled,
		CatalogCron:     cfg.CatalogCron,
		TransferLinked:  cfg.TransferLinked,
		Concurrency:     cfg.Concurrency,
		stopCh:          make(chan struct{}),
	}
	if r.Mediaext == "" {
		r.Mediaext = DefaultMediaext
	}
	if r.DlExt == "" {
		r.DlExt = DefaultDL_Ext
	}
	if r.Overwrite == "" {
		r.Overwrite = "never"
	}
	if r.Concurrency < 1 {
		r.Concurrency = 1
	}
	return r
}

// ApplyConfig 热更新运行时配置（不重启进程）。
func (r *StrmRuntime) ApplyConfig(data map[string]string) {
	if r == nil {
		return
	}
	if v, ok := data["ENV_STRM_ENABLED"]; ok {
		r.Enabled = v == "1" || v == "true"
	}
	if v, ok := data["ENV_STRM_SERVER_ADDRESS"]; ok {
		r.ServerAddress = strings.TrimRight(v, "/")
	}
	if v, ok := data["ENV_STRM_PATHS"]; ok {
		r.Paths = v
	}
	if v, ok := data["ENV_STRM_MEDIAEXT"]; ok {
		if v == "" {
			v = DefaultMediaext
		}
		r.Mediaext = v
	}
	if v, ok := data["ENV_STRM_DL_EXT"]; ok {
		if v == "" {
			v = DefaultDL_Ext
		}
		r.DlExt = v
	}
	if v, ok := data["ENV_STRM_OVERWRITE"]; ok {
		if v == "" {
			v = "never"
		}
		r.Overwrite = v
	}
	recomputeNext := false
	if v, ok := data["ENV_STRM_FULL_SYNC"]; ok {
		enabled := v == "1" || v == "true"
		if enabled != r.FullSyncEnabled {
			r.FullSyncEnabled = enabled
			recomputeNext = true
		}
	}
	if v, ok := data["ENV_STRM_FULL_SYNC_CRON"]; ok {
		if v != r.FullSyncCron {
			r.FullSyncCron = v
			recomputeNext = true
		}
	}
	if v, ok := data["ENV_STRM_CATALOG"]; ok {
		enabled := v == "1" || v == "true"
		if enabled != r.CatalogEnabled {
			r.CatalogEnabled = enabled
			recomputeNext = true
		}
	}
	if v, ok := data["ENV_STRM_CATALOG_CRON"]; ok {
		if v != r.CatalogCron {
			r.CatalogCron = v
			recomputeNext = true
		}
	}
	if recomputeNext {
		r.rescheduleNow()
	}
	if v, ok := data["ENV_STRM_TRANSFER_LINKED"]; ok {
		r.TransferLinked = v == "1" || v == "true"
	}
	if v, ok := data["ENV_STRM_CONCURRENCY"]; ok {
		n, _ := strconv.Atoi(v)
		if n < 1 {
			n = 1
		}
		r.Concurrency = n
	}
}

// Status 返回运行时状态。
func (r *StrmRuntime) Status() map[string]any {
	mappingCount := 0
	for _, line := range strings.Split(r.Paths, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.Contains(line, "#") {
			mappingCount++
		}
	}
	r.mu.Lock()
	busy := r.busy
	catalogBusy := r.catalogBusy
	rewriteBusy := r.rewriteBusy
	nextFS := r.nextFullSync
	nextCat := r.nextCatalog
	lastRes := r.lastCatalogRes
	lastTime := r.lastCatalogTime
	r.mu.Unlock()
	var nextFullSync, nextCatalog any
	if !nextFS.IsZero() {
		nextFullSync = nextFS.Format(time.RFC3339)
	}
	if !nextCat.IsZero() {
		nextCatalog = nextCat.Format(time.RFC3339)
	}

	// 构造 last_catalog 结果（如果有）
	var lastCatalog any
	if lastRes != nil {
		lc := map[string]any{
			"mappings": lastRes.Mappings,
			"matched":  lastRes.Matched,
			"orphan":   lastRes.Orphan,
			"skipped":  lastRes.Skipped,
		}
		if lastRes.Error != "" {
			lc["error"] = lastRes.Error
		}
		if !lastTime.IsZero() {
			lc["time"] = lastTime.Format(time.RFC3339)
		}
		lastCatalog = lc
	}

	return map[string]any{
		"enabled":            r.Enabled,
		"server_address":     r.ServerAddress,
		"api_key_configured": r.APIKey != "",
		"mapping_count":      mappingCount,
		"full_sync_enabled":  r.FullSyncEnabled,
		"full_sync_cron":     r.FullSyncCron,
		"catalog_enabled":    r.CatalogEnabled,
		"catalog_cron":       r.CatalogCron,
		"busy":               busy,
		"catalog_busy":       catalogBusy,
		"rewrite_busy":       rewriteBusy,
		"next_full_sync":     nextFullSync,
		"next_catalog":       nextCatalog,
		"transfer_linked":    r.TransferLinked,
		"last_catalog":       lastCatalog,
	}
}

// Config 返回运行时配置。
func (r *StrmRuntime) Config() map[string]string {
	boolStr := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		"ENV_STRM_ENABLED":         boolStr(r.Enabled),
		"ENV_STRM_SERVER_ADDRESS":  r.ServerAddress,
		"ENV_STRM_PATHS":           r.Paths,
		"ENV_STRM_MEDIAEXT":        r.Mediaext,
		"ENV_STRM_DL_EXT":          r.DlExt,
		"ENV_STRM_OVERWRITE":       r.Overwrite,
		"ENV_STRM_FULL_SYNC":       boolStr(r.FullSyncEnabled),
		"ENV_STRM_FULL_SYNC_CRON":  r.FullSyncCron,
		"ENV_STRM_CATALOG":         boolStr(r.CatalogEnabled),
		"ENV_STRM_CATALOG_CRON":    r.CatalogCron,
		"ENV_STRM_TRANSFER_LINKED": boolStr(r.TransferLinked),
		"ENV_STRM_CONCURRENCY":     strconv.Itoa(r.Concurrency),
	}
}

// Start 启动定时线程。
func (r *StrmRuntime) Start() {
	r.stopCh = make(chan struct{})
	go r.timerLoop()
	log.Printf("✅ STRM 定时线程已启动")
}

// Stop 停止定时线程。
func (r *StrmRuntime) Stop() {
	close(r.stopCh)
}

// timerLoop 定时循环。
func (r *StrmRuntime) timerLoop() {
	r.rescheduleNow()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case now := <-ticker.C:
			r.mu.Lock()
			nextFS := r.nextFullSync
			nextCat := r.nextCatalog
			r.mu.Unlock()

			// 全量同步
			if r.Enabled && r.FullSyncEnabled && r.FullSyncCron != "" && !nextFS.IsZero() {
				if now.After(nextFS) || now.Equal(nextFS) {
					r.safeRun("full_sync")
					r.rescheduleNow()
				}
			}
			// 梳理入册
			if r.Enabled && r.CatalogEnabled && r.CatalogCron != "" && !nextCat.IsZero() {
				if now.After(nextCat) || now.Equal(nextCat) {
					r.TriggerCatalog()
					r.rescheduleNow()
				}
			}
		}
	}
}

// rescheduleNow 按当前配置重算下一次全量同步 + 梳理入册时间。
func (r *StrmRuntime) rescheduleNow() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 全量同步
	r.nextFullSync = time.Time{}
	if r.Enabled && r.FullSyncEnabled && r.FullSyncCron != "" {
		if t := computeNextCronTime(r.FullSyncCron, time.Now()); t != nil {
			r.nextFullSync = *t
		} else {
			log.Printf("【STRM定时】全量同步 cron 表达式无效: %s", r.FullSyncCron)
		}
	}

	// 梳理入册
	r.nextCatalog = time.Time{}
	if r.Enabled && r.CatalogEnabled && r.CatalogCron != "" {
		if t := computeNextCronTime(r.CatalogCron, time.Now()); t != nil {
			r.nextCatalog = *t
		} else {
			log.Printf("【STRM定时】梳理入册 cron 表达式无效: %s", r.CatalogCron)
		}
	}
}

// safeRun 带互斥锁的后台执行。
func (r *StrmRuntime) safeRun(action string) bool {
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		log.Printf("STRM 任务正在执行中，跳过本次 %s", action)
		return false
	}
	r.busy = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.busy = false
			r.mu.Unlock()
		}()
		if action == "full_sync" {
			r.runFullSync()
		}
	}()
	return true
}

// TriggerFullSync 手动触发全量同步（busy 时返回 false）。
func (r *StrmRuntime) TriggerFullSync() bool {
	if r == nil {
		return false
	}
	return r.safeRun("full_sync")
}

// runFullSync 执行全量同步。
func (r *StrmRuntime) runFullSync() {
	if !r.Enabled {
		return
	}
	if r.ServerAddress == "" {
		log.Printf("【全量STRM生成】未配置 302 服务地址（ENV_STRM_SERVER_ADDRESS），跳过")
		return
	}
	if r.Paths == "" {
		log.Printf("【全量STRM生成】未配置同步目录映射（ENV_STRM_PATHS），跳过")
		return
	}
	helper := NewFullSyncStrmHelper(
		r.client,
		r.Mediaext,
		r.DlExt,
		r.ServerAddress,
		r.APIKey,
		false, // autoDownloadMediaInfo
		r.Concurrency,
	)
	count, ok := helper.GenerateStrmFiles(context.Background(), r.Paths, r.Overwrite)
	if ok {
		transfer.Notify(fmt.Sprintf("📁 全量STRM同步完成：生成 %d 个 STRM 文件", count), "")
	}
}

// TriggerCatalog 手动触发 STRM 梳理入册（catalogBusy 时返回 false）。
func (r *StrmRuntime) TriggerCatalog() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.catalogBusy {
		r.mu.Unlock()
		return false
	}
	r.catalogBusy = true
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			r.catalogBusy = false
			r.mu.Unlock()
		}()
		r.runCatalog()
	}()
	return true
}

// runCatalog 执行 STRM 梳理入册。
func (r *StrmRuntime) runCatalog() {
	executor := transfer.GetTransferExecutor()
	if executor == nil || executor.History == nil {
		log.Printf("【STRM 梳理】transfer executor/history 未初始化")
		r.storeCatalogResult(CatalogResult{Error: "transfer executor/history 未初始化"})
		return
	}
	if r.Paths == "" {
		log.Printf("【STRM 梳理】未配置 STRM 映射（ENV_STRM_PATHS），跳过")
		r.storeCatalogResult(CatalogResult{Error: "未配置 STRM 映射（ENV_STRM_PATHS）"})
		return
	}
	if r.client == nil {
		log.Printf("【STRM 梳理】123 client 未初始化，跳过")
		r.storeCatalogResult(CatalogResult{Error: "123 client 未初始化"})
		return
	}

	result := CatalogStrmToHistory(context.Background(), r.client, r.Paths, executor.History, r.Concurrency)
	r.storeCatalogResult(result)
	if result.Error != "" {
		log.Printf("【STRM 梳理】失败: %s", result.Error)
		transfer.Notify("⚠️ STRM 梳理失败："+result.Error, "")
		return
	}
	transfer.Notify(fmt.Sprintf("📚 STRM 梳理完成：匹配 %d  orphan %d  跳过映射 %d",
		result.Matched, result.Orphan, result.Skipped), "")
}

// TriggerRewrite 手动改写本地 STRM URL。
func (r *StrmRuntime) TriggerRewrite() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.rewriteBusy {
		r.mu.Unlock()
		return false
	}
	r.rewriteBusy = true
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			r.rewriteBusy = false
			r.mu.Unlock()
		}()
		r.runRewrite()
	}()
	return true
}

// runRewrite 执行改写。
func (r *StrmRuntime) runRewrite() {
	if r.ServerAddress == "" {
		log.Printf("【改写STRM】未配置 302 服务地址，跳过")
		return
	}
	if r.Paths == "" {
		log.Printf("【改写STRM】未配置 STRM 映射，跳过")
		return
	}
	roots := LocalRoots(r.Paths)
	if len(roots) == 0 {
		log.Printf("【改写STRM】无本地目录映射")
		return
	}
	total, okCount, failCount := RewriteStrmURLs(roots, r.ServerAddress, r.APIKey, r.Concurrency)
	log.Printf("【改写STRM】完成：扫描 %d 成功 %d 失败 %d", total, okCount, failCount)
	transfer.Notify(fmt.Sprintf("📝 STRM 改写完成：共 %d 个，成功 %d，失败 %d", total, okCount, failCount), "")
}

func (r *StrmRuntime) storeCatalogResult(res CatalogResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastCatalogRes = &res
	r.lastCatalogTime = time.Now()
}

// GetLastCatalogResult 返回最近一次 catalog 结果和时间。
func (r *StrmRuntime) GetLastCatalogResult() (*CatalogResult, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastCatalogRes, r.lastCatalogTime
}

// CheckAPIKey 302 接口鉴权。
func (r *StrmRuntime) CheckAPIKey(apikey string) bool {
	return r.APIKey != "" && apikey == r.APIKey
}

// notifyEmbyNew 整理入库后通知 Emby 增量扫描新 STRM 路径（未配置/失败静默跳过）。
func notifyEmbyNew(paths []string) {
	emby := GetEmbyRuntime()
	if emby == nil || len(paths) == 0 {
		return
	}
	seen := map[string]bool{}
	var updates []map[string]string
	for _, p := range paths {
		p = strings.ReplaceAll(p, "\\", "/")
		if seen[p] {
			continue
		}
		seen[p] = true
		updates = append(updates, map[string]string{"Path": p, "UpdateType": "Created"})
	}
	_, _, err := emby.PostRaw(context.Background(), "/emby/Library/Media/Updated", map[string]any{"Updates": updates})
	if err != nil {
		return
	}
	log.Printf("已通知 Emby 扫描新增 STRM 路径（%d 个目录）", len(updates))
}

// GetRedirect 302 跳转。
func (r *StrmRuntime) GetRedirect(ctx context.Context, name string, size int64, md5, s3KeyFlag, userAgent string) (string, error) {
	return GetRedirectURL(ctx, r.client, name, size, md5, s3KeyFlag, userAgent)
}

// OnTransferCompleted 整理完成后联动生成 STRM。
func (r *StrmRuntime) OnTransferCompleted(stats *transfer.TransferStats) {
	if !r.Enabled || !r.TransferLinked {
		return
	}
	if r.ServerAddress == "" || r.Paths == "" {
		return
	}
	var targetPaths []string
	for _, group := range stats.Groups {
		targetPaths = append(targetPaths, group.TargetPaths...)
	}
	if len(targetPaths) == 0 {
		return
	}

	generator := NewTransferLinkedStrmGenerator(
		r.client,
		r.Paths,
		r.ServerAddress,
		r.APIKey,
		r.Mediaext,
		r.Concurrency,
	)

	go func() {
		retries := 5
		delay := 20 * time.Second
		var doGenerate func()
		doGenerate = func() {
			r.mu.Lock()
			if r.busy {
				r.mu.Unlock()
				if retries > 0 {
					retries--
					log.Printf("STRM 任务正在执行中，整理联动 %v 后重试（剩余 %d 次）", delay, retries)
					time.AfterFunc(delay, doGenerate)
				} else {
					log.Printf("STRM 任务持续繁忙，整理联动放弃；可手动触发全量同步补生成缺失 STRM")
				}
				return
			}
			r.busy = true
			r.mu.Unlock()

			defer func() {
				r.mu.Lock()
				r.busy = false
				r.mu.Unlock()
			}()

			result := generator.GenerateByPaths(context.Background(), targetPaths, r.Overwrite)
			msg := "📥 联动：STRM 成功 " + strconv.Itoa(result["success"]) + " 跳过 " + strconv.Itoa(result["skip"]) + " 失败 " + strconv.Itoa(result["fail"])
			log.Printf("%s", msg)
			transfer.Notify(msg, "")
			if result["success"] > 0 {
				notifyEmbyNew(targetPaths)
			}
		}
		doGenerate()
	}()
}

// SetupTransferDeleteCallback 注册整理去重覆盖时的旧版本删除回调（完整四件套：本地 STRM + 网盘回收站 + Emby + 整理历史）。
// 对应 Python transfer_executor.py _trash_old_version → delete_media_items。
func SetupTransferDeleteCallback() {
	transfer.SetDeleteOldVersionCallback(func(ctx context.Context, oldFileID, oldTargetPath string, history *transfer.TransferHistory) {
		rt := GetRuntime()
		if rt == nil {
			// STRM 未启用时退化为仅网盘回收站 + 整理历史（与未注册回调时的兜底行为一致）
			if ex := transfer.GetTransferExecutor(); ex != nil && ex.Client != nil {
				_, _ = ex.Client.TrashFile(ctx, oldFileID)
			}
			if history != nil {
				history.DeleteByFileID(oldFileID)
			}
			return
		}
		name := strings.TrimSuffix(oldTargetPath, "/")
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		if name == "" {
			name = oldFileID
		}
		fid, _ := strconv.ParseInt(oldFileID, 10, 64)
		items := []DeleteItem{{
			ID:        oldFileID,
			Name:      name,
			PanPath:   oldTargetPath,
			PanFileID: fid,
		}}
		emby := GetEmbyRuntime()
		DeleteItems(ctx, rt.client, items, nil, emby, rt.Paths, history)
	})
}

// ---------- 全局实例管理 ----------

var (
	strmRuntime   *StrmRuntime
	strmRuntimeMu sync.RWMutex
)

// SetRuntime 设置全局 STRM 运行时。
func SetRuntime(r *StrmRuntime) {
	strmRuntimeMu.Lock()
	strmRuntime = r
	strmRuntimeMu.Unlock()
}

// GetRuntime 获取全局 STRM 运行时。
func GetRuntime() *StrmRuntime {
	strmRuntimeMu.RLock()
	defer strmRuntimeMu.RUnlock()
	return strmRuntime
}

// OnTransferCompleted 全局整理完成回调（供 transfer runtime 调用）。
func OnTransferCompleted(stats *transfer.TransferStats) {
	if r := GetRuntime(); r != nil {
		r.OnTransferCompleted(stats)
	}
}

// InitFromEnv 从环境变量初始化 STRM 功能，返回是否启用。
func InitFromEnv(client *pan123.Client, getEnv func(string) string) bool {
	if getEnv == nil {
		getEnv = func(string) string { return "" }
	}
	enabled := getEnv("ENV_STRM_ENABLED") == "1"
	if !enabled {
		log.Printf("🚫 STRM 功能未启用（ENV_STRM_ENABLED=0）")
		SetRuntime(nil)
		return false
	}

	concurrency, _ := strconv.Atoi(getEnv("ENV_STRM_CONCURRENCY"))
	if concurrency < 1 {
		concurrency = 1
	}
	cfg := StrmConfig{
		Enabled:         true,
		ServerAddress:   getEnv("ENV_STRM_SERVER_ADDRESS"),
		Paths:           getEnv("ENV_STRM_PATHS"),
		Mediaext:        getEnv("ENV_STRM_MEDIAEXT"),
		DlExt:           getEnv("ENV_STRM_DL_EXT"),
		Overwrite:       getEnv("ENV_STRM_OVERWRITE"),
		FullSyncEnabled: getEnv("ENV_STRM_FULL_SYNC") == "1",
		FullSyncCron:    getEnv("ENV_STRM_FULL_SYNC_CRON"),
		CatalogEnabled:  getEnv("ENV_STRM_CATALOG") == "1",
		CatalogCron:     getEnv("ENV_STRM_CATALOG_CRON"),
		TransferLinked:  getEnv("ENV_STRM_TRANSFER_LINKED") == "1",
		Concurrency:     concurrency,
	}
	r := NewStrmRuntime(client, cfg)
	SetRuntime(r)
	r.Start()
	log.Printf("✅ STRM 功能已初始化并启动")
	return true
}

// ---------- cron 表达式解析 ----------

// parseCronField 解析 cron 单字段，返回允许值集合。
func parseCronField(field string, lo, hi int) map[int]bool {
	values := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			for i := lo; i <= hi; i++ {
				values[i] = true
			}
		} else if strings.Contains(part, "/") {
			sub := strings.SplitN(part, "/", 2)
			step := atoi(sub[1], 1)
			start, end := lo, hi
			if sub[0] != "" && sub[0] != "*" {
				if strings.Contains(sub[0], "-") {
					rng := strings.SplitN(sub[0], "-", 2)
					start = atoi(rng[0], lo)
					end = atoi(rng[1], hi)
				} else {
					start = atoi(sub[0], lo)
				}
			}
			for i := start; i <= end; i += step {
				values[i] = true
			}
		} else if strings.Contains(part, "-") {
			rng := strings.SplitN(part, "-", 2)
			for i := atoi(rng[0], lo); i <= atoi(rng[1], hi); i++ {
				values[i] = true
			}
		} else {
			values[atoi(part, lo)] = true
		}
	}
	return values
}

// computeNextCronTime 计算 cron 表达式的下一次执行时间。
// 5 段 cron：分 时 日 月 周（周 0=周日）。
func computeNextCronTime(expr string, base time.Time) *time.Time {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil
	}

	minutes := parseCronField(fields[0], 0, 59)
	hours := parseCronField(fields[1], 0, 23)
	days := parseCronField(fields[2], 1, 31)
	months := parseCronField(fields[3], 1, 12)
	weeks := parseCronField(fields[4], 0, 6)

	if len(minutes) == 0 || len(hours) == 0 || len(days) == 0 || len(months) == 0 || len(weeks) == 0 {
		return nil
	}

	dt := base.Add(time.Minute)
	deadline := base.AddDate(5, 0, 0) // 最多向后找 5 年

	for dt.Before(deadline) || dt.Equal(deadline) {
		// weekday: 0=Sunday, 1=Monday, ..., 6=Saturday -> Go time.Weekday 0=Sunday
		if months[int(dt.Month())] && days[dt.Day()] && weeks[int(dt.Weekday())] && hours[dt.Hour()] && minutes[dt.Minute()] {
			t := dt
			return &t
		}
		dt = dt.Add(time.Minute)
	}
	return nil
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			return def
		}
	}
	return n
}
