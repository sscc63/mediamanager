package transfer

// 运行时全局实例管理（对应 runtime.py）。
// 全局实例由主程序初始化，其他模块通过 GetExecutor / GetScheduler 获取。

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/config"
	"mmbot/internal/pan123"
)

var (
	execMu    sync.RWMutex
	executor  *TransferExecutor
	scheduler *TransferScheduler
)

// SetExecutor 设置全局整理执行器。
func SetExecutor(e *TransferExecutor) {
	execMu.Lock()
	executor = e
	execMu.Unlock()
	log.Printf("全局整理执行器已设置")
}

// GetExecutor 获取全局整理执行器（未初始化返回 nil）。
func GetExecutor() *TransferExecutor {
	execMu.RLock()
	defer execMu.RUnlock()
	return executor
}

// SetScheduler 设置全局调度器。
func SetScheduler(s *TransferScheduler) {
	execMu.Lock()
	scheduler = s
	execMu.Unlock()
	log.Printf("全局整理调度器已设置")
}

// GetScheduler 获取全局调度器（未初始化返回 nil）。
func GetScheduler() *TransferScheduler {
	execMu.RLock()
	defer execMu.RUnlock()
	return scheduler
}

// GetTransferExecutor 获取全局整理执行器（Bot 模块别名，对应 Python 的 get_transfer_runtime）。
func GetTransferExecutor() *TransferExecutor { return GetExecutor() }

// GetTransferScheduler 获取全局整理调度器。
func GetTransferScheduler() *TransferScheduler { return GetScheduler() }

// InitFromEnv 从配置初始化整理引擎，返回是否初始化成功。
func InitFromEnv(client *pan123.Client, cfg *config.Config) bool {
	if !cfg.GetBool("ENV_TRANSFER_ENABLED", false) {
		log.Printf("🚫 文件整理功能未启用（ENV_TRANSFER_ENABLED=0）")
		return false
	}

	tmdbAPIKey := cfg.Get("ENV_TMDB_API_KEY", "")
	tmdbLanguage := cfg.Get("ENV_TRANSFER_TMDB_LANGUAGE", "zh-CN")
	if tmdbLanguage == "" {
		tmdbLanguage = "zh-CN"
	}
	categoryYAML := cfg.Get("ENV_TRANSFER_CATEGORY_YAML", "config/category.yaml")
	if categoryYAML == "" {
		categoryYAML = "config/category.yaml"
	}
	dirsFile := cfg.Get("ENV_TRANSFER_DIRS_FILE", "config/directories.json")
	if dirsFile == "" {
		dirsFile = "config/directories.json"
	}
	movieFormat := cfg.Get("ENV_TRANSFER_MOVIE_FORMAT", "")
	tvFormat := cfg.Get("ENV_TRANSFER_TV_FORMAT", "")
	defaultType := cfg.Get("ENV_TRANSFER_TYPE", "move")
	if defaultType == "" {
		defaultType = "move"
	}
	scanInterval := cfg.GetInt("ENV_TRANSFER_SCAN_INTERVAL", 30)
	enableScrape := cfg.GetBool("ENV_TRANSFER_SCRAPE", true)
	minFilesize := cfg.GetInt("ENV_TRANSFER_MIN_FILESIZE", 10)

	// 跳过扩展名
	skipExts := map[string]bool{}
	for _, e := range strings.Split(cfg.Get("ENV_TRANSFER_SKIP_EXTS", ".part,!qB,.temp"), ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		skipExts[e] = true
	}

	// 版本优先替换
	priorityVersions := map[string]bool{}
	for _, v := range strings.Split(cfg.Get("ENV_TRANSFER_PRIORITY_VERSIONS", ""), ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			priorityVersions[v] = true
		}
	}

	// 体积豁免倍数
	sizeOverrideRatio := 0.0
	if raw := strings.TrimSpace(cfg.Get("ENV_TRANSFER_SIZE_OVERRIDE_RATIO", "")); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 1 {
			sizeOverrideRatio = f
		}
	}

	executor, err := NewTransferExecutor(ExecutorConfig{
		Client:              client,
		TMDBAPIKey:          tmdbAPIKey,
		TMDBLanguage:        tmdbLanguage,
		CategoryYAML:        categoryYAML,
		DirsFile:            dirsFile,
		DBPath:              "data/transfer.db",
		MovieFormat:         movieFormat,
		TVFormat:            tvFormat,
		DefaultTransferType: defaultType,
		SkipExts:            skipExts,
		MinFilesizeMB:       minFilesize,
		EnableScrape:        enableScrape,
		PriorityVersions:    priorityVersions,
		SizeOverrideRatio:   sizeOverrideRatio,
		AIConfig: AIConfig{
			Enabled:    cfg.GetBool("ENV_AI_ENABLED", false),
			APIKey:     cfg.Get("ENV_AI_API_KEY", ""),
			BaseURL:    cfg.Get("ENV_AI_BASE_URL", ""),
			Model:      cfg.Get("ENV_AI_MODEL", ""),
			DailyQuota: cfg.GetInt("ENV_AI_DAILY_QUOTA", aiDefaultQuota),
		},
	})
	if err != nil {
		log.Printf("整理执行器初始化失败: %v", err)
		return false
	}
	SetExecutor(executor)

	scheduler := NewTransferScheduler(executor, scanInterval)
	SetScheduler(scheduler)
	if scanInterval > 0 {
		scheduler.Start()
	}
	log.Printf("✅ 文件整理引擎已启动（扫描间隔 %d 分钟）", scanInterval)
	return true
}

// ApplyConfig 热更新整理引擎配置（不重启进程）。
// 支持所有 ENV_TRANSFER_* 及 ENV_TMDB_API_KEY 的热更新。
func ApplyConfig(data map[string]string) {
	e := GetExecutor()
	s := GetScheduler()
	if e == nil {
		return
	}

	if v, ok := data["ENV_TRANSFER_ENABLED"]; ok {
		enabled := v == "1" || v == "true"
		if s != nil {
			if enabled {
				if !s.IsRunning() && s.ScanInterval() > 0 {
					s.Start()
				}
			} else {
				s.Stop()
			}
		}
	}
	if v, ok := data["ENV_TRANSFER_SCAN_INTERVAL"]; ok {
		interval, _ := strconv.Atoi(v)
		if s != nil {
			s.Stop()
			s.scanInterval = time.Duration(interval) * time.Minute
			if interval > 0 {
				s.Start()
			}
		}
	}
	if v, ok := data["ENV_TRANSFER_MOVIE_FORMAT"]; ok {
		e.MovieFormat = v
	}
	if v, ok := data["ENV_TRANSFER_TV_FORMAT"]; ok {
		e.TVFormat = v
	}
	if v, ok := data["ENV_TRANSFER_TYPE"]; ok {
		if v == "" {
			v = "move"
		}
		e.DefaultTransferType = v
	}
	if v, ok := data["ENV_TRANSFER_SCRAPE"]; ok {
		e.EnableScrape = v == "1" || v == "true"
		if e.EnableScrape && e.TMDB != nil && e.Scraper == nil {
			e.Scraper = NewScraper(e.TMDB, e.Client, e.TMDB.Language)
		}
	}

	// 更新 TMDB 客户端
	if v, ok := data["ENV_TMDB_API_KEY"]; ok {
		if v != "" {
			if e.TMDB == nil {
				tmdb, err := NewTmdbClient(v, "zh-CN")
				if err == nil {
					e.TMDB = tmdb
				}
			} else {
				e.TMDB.APIKey = v
			}
		}
	}
	if v, ok := data["ENV_TRANSFER_TMDB_LANGUAGE"]; ok {
		if v == "" {
			v = "zh-CN"
		}
		if e.TMDB != nil {
			e.TMDB.Language = v
		}
		// 更新 Scraper 语言
		if e.Scraper != nil {
			e.Scraper = NewScraper(e.TMDB, e.Client, v)
		}
	}

	if v, ok := data["ENV_TRANSFER_SKIP_EXTS"]; ok {
		exts := map[string]bool{}
		for _, ext := range strings.Split(v, ",") {
			ext = strings.TrimSpace(ext)
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			exts[ext] = true
		}
		e.SkipExts = exts
	}
	if v, ok := data["ENV_TRANSFER_MIN_FILESIZE"]; ok {
		minMB, _ := strconv.Atoi(v)
		if minMB <= 0 {
			minMB = DefaultMinFilesizeMB
		}
		e.MinFilesize = int64(minMB) * 1024 * 1024
	}
	if v, ok := data["ENV_TRANSFER_PRIORITY_VERSIONS"]; ok {
		pv := map[string]bool{}
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				pv[s] = true
			}
		}
		e.PriorityVersions = pv
	}
	if v, ok := data["ENV_TRANSFER_SIZE_OVERRIDE_RATIO"]; ok {
		ratio, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if ratio > 1 {
			e.SizeOverrideRatio = ratio
		} else {
			e.SizeOverrideRatio = 0
		}
	}

	// AI 兜底配置热更新：涉及 ENV_AI_* 就整体重建客户端。
	// 重建会重置缓存与配额 —— 换 key/模型后旧缓存已失效，重建比逐字段打补丁更安全。
	if hasAnyKey(data, "ENV_AI_ENABLED", "ENV_AI_API_KEY", "ENV_AI_BASE_URL", "ENV_AI_MODEL", "ENV_AI_DAILY_QUOTA") {
		enabled := data["ENV_AI_ENABLED"] == "1" || data["ENV_AI_ENABLED"] == "true"
		quota := aiDefaultQuota
		if n, err := strconv.Atoi(strings.TrimSpace(data["ENV_AI_DAILY_QUOTA"])); err == nil && n > 0 {
			quota = n
		}
		e.AI = NewAIClient(AIConfig{
			Enabled:    enabled,
			APIKey:     data["ENV_AI_API_KEY"],
			BaseURL:    data["ENV_AI_BASE_URL"],
			Model:      data["ENV_AI_MODEL"],
			DailyQuota: quota,
		})
		if e.AI != nil {
			log.Printf("✅ AI 标题清洗配置已更新（模型 %s，每日上限 %d 次）", e.AI.Model, e.AI.Quota)
		} else {
			log.Printf("🚫 AI 标题清洗已关闭")
		}
	}
}

func hasAnyKey(data map[string]string, keys ...string) bool {
	for _, k := range keys {
		if _, ok := data[k]; ok {
			return true
		}
	}
	return false
}

// ---------- 转存后自动整理的并发控制 ----------
// 同一时刻只允许一个整理任务在跑，期间再触发只标记 pending，
// 当前任务结束后自动补跑一轮，保证新转存的文件不漏整理。
var (
	organizeMu      sync.Mutex
	organizeBusy    bool
	organizePending bool
)

// TriggerTransferAfterSave 转存成功后触发自动整理（事件驱动模式，异步执行）。
// 仅当 sourcePID 在 directories.json 中配置时才触发。
func TriggerTransferAfterSave(sourcePID int) bool {
	e := GetExecutor()
	if e == nil {
		return false // 整理功能未启用
	}

	// 检查 sourcePID 是否在配置的目录映射中
	dirConf := e.DirHelper.GetDirBySource(sourcePID)
	if dirConf == nil {
		return false // 该 PID 未配置整理
	}

	// 先在当前调用中合并触发请求，避免整理繁忙时为每次转存创建阻塞 goroutine。
	organizeMu.Lock()
	if organizeBusy {
		organizePending = true
		organizeMu.Unlock()
		log.Printf("🔄 整理任务执行中，标记待整理: source_pid=%d (%s)", sourcePID, dirConf.Name)
		return true
	}
	organizeBusy = true
	organizeMu.Unlock()

	// 异步触发整理（不阻塞转存流程）
	go func() {
		defer func() {
			organizeMu.Lock()
			organizeBusy = false
			organizeMu.Unlock()
		}()

		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("❌ 转存后自动整理异常: %v", r)
					}
				}()
				log.Printf("🔄 转存后触发自动整理: source_pid=%d (%s)", sourcePID, dirConf.Name)
				stats := e.TransferDirectory(context.Background(), sourcePID, true, false, nil, dirConf.TransferType, true, nil)
				log.Printf("✅ 转存后整理完成: source_pid=%d, success=%d, fail=%d, skip=%d",
					sourcePID, stats.Success, stats.Fail, stats.Skip)
				// 整理完成后联动生成 STRM（若 STRM 功能启用）
				if s := GetScheduler(); s != nil && s.OnTransferCompleted != nil {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("整理联动 STRM 生成失败: %v", r)
							}
						}()
						s.OnTransferCompleted(stats)
					}()
				}
			}()
			// 整理期间又有新的转存触发时，补跑一轮
			organizeMu.Lock()
			if !organizePending {
				organizeMu.Unlock()
				break
			}
			organizePending = false
			organizeMu.Unlock()
		}
	}()
	return true
}
