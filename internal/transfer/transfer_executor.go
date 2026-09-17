package transfer

// 整理执行器（对应 transfer_executor.py，核心）。
// 识别 → 分类 → 目录匹配 → 改名 → 移动 → 刮削 → 历史 → 通知

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"mmbot/internal/gcguard"
	"mmbot/internal/pan123"
)

// 默认跳过的文件扩展名。
var DefaultSkipExts = map[string]bool{".part": true, "!qb": true, ".temp": true, ".downloading": true, ".aria2": true}

// 默认最小文件大小（MB）。
const DefaultMinFilesizeMB = 10

// TransferResult 单个文件整理结果。
type TransferResult struct {
	Success    bool
	FileID     string
	FileName   string
	TargetPath string
	MediaTitle string
	Message    string
	Skipped    bool // 已整理过/被跳过
	Media      *MediaInfo
	Meta       *MetaInfo
	FileSize   int64 // 字节
}

// GroupSummary 同 media_id + season 的整理结果聚合（用于批量通知）。
type GroupSummary struct {
	Title        string
	Year         int
	MediaType    string // movie / tv
	Category     string
	TMDBID       int
	Season       int
	VoteAverage  float64
	FileCount    int   // 成功文件数
	TotalSize    int64 // 成功文件总大小（字节）
	Episodes     []int // 已入库集数列表
	EpisodeSizes map[int]int64
	ResourceTerm string // 资源质量描述
	TargetPaths  []string
	FailNames    []string
	PosterURL    string
}

// TransferStats 批量整理统计。
type TransferStats struct {
	Success  int
	Fail     int
	Skip     int
	FailList [][2]string // [(file_name, reason)]
	SkipList [][2]string // [(title, reason)]
	Groups   map[groupKey]*GroupSummary
	Moved    map[string]bool // 本次整理中已成功移入目标库的文件 id（源 id + 移动后 id），覆盖整理撤销计数用
}

// groupKey 分组键。
type groupKey struct {
	MediaKey string
	Season   int
}

// TransferExecutor 整理执行器。
type TransferExecutor struct {
	Client    *pan123.Client
	TMDB      *TmdbClient
	DirHelper *DirectoryHelper
	CatHelper *CategoryHelper
	History   *TransferHistory
	Scraper   *Scraper

	MovieFormat         string
	TVFormat            string
	DefaultTransferType string
	SkipExts            map[string]bool
	MinFilesize         int64
	EnableScrape        bool
	PriorityVersions    map[string]bool
	SizeOverrideRatio   float64

	// 目录 PID 缓存：(parentPID, dirName) -> childPID
	dirPIDCache    map[string]int
	dirPIDCacheMax int
	// 目标目录文件列表缓存（任务级，有上限防止内存峰值）
	targetListingCache    map[int64][]pan123.FileInfo
	targetListingCacheMax int

	// cacheMu 保护上面两个缓存。
	// 它们挂在共享的 executor 上，而访问来自多个 goroutine：后台整理任务
	// （转存后自动整理 / 调度器 / 频道监控触发的批量整理）与 Web 的单文件整理
	// （web/transfer.go 每个 HTTP 请求一个 goroutine）会并发访问。
	// 无锁时并发写 map 是 fatal error: concurrent map writes —— 不可 recover，进程直接退出。
	cacheMu sync.Mutex
}

// ExecutorConfig 执行器配置。
type ExecutorConfig struct {
	Client              *pan123.Client
	TMDBAPIKey          string
	TMDBLanguage        string
	CategoryYAML        string
	DirsFile            string
	DBPath              string
	MovieFormat         string
	TVFormat            string
	DefaultTransferType string
	SkipExts            map[string]bool
	MinFilesizeMB       int
	EnableScrape        bool
	PriorityVersions    map[string]bool
	SizeOverrideRatio   float64
}

// NewTransferExecutor 创建整理执行器。
func NewTransferExecutor(cfg ExecutorConfig) (*TransferExecutor, error) {
	e := &TransferExecutor{
		Client:              cfg.Client,
		MovieFormat:         cfg.MovieFormat,
		TVFormat:            cfg.TVFormat,
		DefaultTransferType: cfg.DefaultTransferType,
		SkipExts:            DefaultSkipExts,
		EnableScrape:        cfg.EnableScrape,
		dirPIDCache:         map[string]int{},
		// 目录 ID 只需覆盖近期整理任务，避免长期运行后缓存无限接近大上限。
		dirPIDCacheMax:        4096,
		targetListingCache:    map[int64][]pan123.FileInfo{},
		targetListingCacheMax: 500,
	}
	if cfg.SkipExts != nil {
		e.SkipExts = cfg.SkipExts
	}
	minMB := cfg.MinFilesizeMB
	if minMB <= 0 {
		minMB = DefaultMinFilesizeMB
	}
	e.MinFilesize = int64(minMB) * 1024 * 1024
	if cfg.DefaultTransferType == "" {
		e.DefaultTransferType = "move"
	}
	e.PriorityVersions = cfg.PriorityVersions
	if e.PriorityVersions == nil {
		e.PriorityVersions = map[string]bool{}
	}
	e.SizeOverrideRatio = cfg.SizeOverrideRatio
	if e.SizeOverrideRatio <= 1 {
		e.SizeOverrideRatio = 0
	}

	if cfg.TMDBAPIKey != "" {
		tmdb, err := NewTmdbClient(cfg.TMDBAPIKey, cfg.TMDBLanguage)
		if err != nil {
			log.Printf("TMDB 客户端初始化失败（将不调 TMDB）: %v", err)
			e.TMDB = nil
		} else {
			e.TMDB = tmdb
			log.Printf("TMDB 客户端已初始化")
		}
	} else {
		log.Printf("未配置 TMDB API Key，将只用文件名识别")
	}

	e.DirHelper = NewDirectoryHelper(cfg.DirsFile)
	e.CatHelper = NewCategoryHelper(cfg.CategoryYAML)
	if cfg.DBPath == "" {
		cfg.DBPath = "data/transfer.db"
	}
	hist, err := NewTransferHistory(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("初始化历史数据库失败: %w", err)
	}
	e.History = hist
	if e.EnableScrape && e.TMDB != nil {
		e.Scraper = NewScraper(e.TMDB, e.Client, cfg.TMDBLanguage)
	}
	return e, nil
}

// 版本优先级分值表。
var versionScoreMap = map[string]int{
	"DV": 4, "BluRay": 3, "HDR10+": 2, "HDR": 1, "HLG": 1,
}

// CalcVersionScore 根据文件名计算版本优先级分（公开函数，供 catalog 等其他模块使用）。
// 不依赖 PriorityVersions 配置，始终按 versionScoreMap 全量计算。
func CalcVersionScore(filename string) int {
	if filename == "" {
		return 0
	}
	score := 0
	// HDR10+ 特判
	if re := regexp.MustCompile(`(?i)HDR\s?10\+`); re.MatchString(filename) {
		score = max(score, versionScoreMap["HDR10+"])
	}
	for _, token := range strings.Fields(ExtractEffects(filename)) {
		key := token
		if token == "HDR10" {
			key = "HDR"
		}
		if v, ok := versionScoreMap[key]; ok {
			score = max(score, v)
		}
	}
	// DV / DoVi / Dolby Vision 特判
	if re := regexp.MustCompile(`(?i)\b(DV|DoVi|Dolby[\s.-]?Vision)\b`); re.MatchString(filename) {
		score = max(score, versionScoreMap["DV"])
	}
	// BluRay / REMUX 特判
	if re := regexp.MustCompile(`(?i)(REMUX|BLURAY|Blu-Ray)`); re.MatchString(filename) {
		score = max(score, versionScoreMap["BluRay"])
	}
	return score
}

// versionScore 计算新文件的版本优先级分（受 PriorityVersions 配置控制）。
func (e *TransferExecutor) versionScore(meta *MetaInfo) int {
	if len(e.PriorityVersions) == 0 || meta == nil {
		return 0
	}
	raw := meta.RawName
	score := 0
	// HDR10+ 特判
	if e.PriorityVersions["HDR10+"] {
		if re, _ := regexp.Compile(`(?i)HDR\s?10\+`); re.MatchString(raw) {
			score = max(score, versionScoreMap["HDR10+"])
		}
	}
	for _, token := range strings.Fields(ExtractEffects(raw)) {
		key := token
		if token == "HDR10" {
			key = "HDR"
		}
		if token == "DoVi" || token == "Dovi" {
			key = "DV"
		}
		if e.PriorityVersions[key] {
			score = max(score, versionScoreMap[key])
		}
	}
	// DV / DoVi / Dolby Vision 特判（受配置控制）
	if e.PriorityVersions["DV"] {
		if re, _ := regexp.Compile(`(?i)\b(DV|DoVi|Dolby[\s.-]?Vision)\b`); re.MatchString(raw) {
			score = max(score, versionScoreMap["DV"])
		}
	}
	if e.PriorityVersions["BluRay"] {
		src := strings.ToUpper(meta.Source)
		if strings.Contains(src, "REMUX") || strings.Contains(src, "BLURAY") {
			score = max(score, versionScoreMap["BluRay"])
		}
	}
	return score
}

// dedupDecision 版本优先去重决策，返回 (是否覆盖整理, 原因描述)。
func (e *TransferExecutor) dedupDecision(newVersion, oldVersion int, newSize, oldSize int64) (bool, string) {
	r := e.SizeOverrideRatio
	// 版本优先关闭时旧记录残留历史分数，统一归零，退化为纯大小去重
	if len(e.PriorityVersions) == 0 {
		newVersion, oldVersion = 0, 0
	}
	if r > 1 && oldSize > 0 {
		if newSize >= int64(float64(oldSize)*r) {
			return true, fmt.Sprintf("大小碾压（新 %dB ≥ 旧 %dB×%.1f）", newSize, oldSize, r)
		}
		if oldSize >= int64(float64(newSize)*r) {
			return false, fmt.Sprintf("旧文件大小碾压（旧 %dB ≥ 新 %dB×%.1f）", oldSize, newSize, r)
		}
	}
	if newVersion != oldVersion {
		if newVersion > oldVersion {
			return true, fmt.Sprintf("版本升级（%d分 > %d分）", newVersion, oldVersion)
		}
		return false, fmt.Sprintf("版本更低（%d分 < %d分）", newVersion, oldVersion)
	}
	if newSize > oldSize {
		return true, fmt.Sprintf("同分更大（%dB > %dB）", newSize, oldSize)
	}
	return false, fmt.Sprintf("同分更小或相等（%dB ≤ %dB）", newSize, oldSize)
}

// DeleteOldVersionFn 旧版本删除回调（完整四件套：本地 STRM + 网盘回收站 + Emby 通知 + 整理历史）。
// 由 strm 包注册，避免循环导入。
type DeleteOldVersionFn func(ctx context.Context, oldFileID, oldTargetPath string, history *TransferHistory)

var deleteOldVersion DeleteOldVersionFn

// SetDeleteOldVersionCallback 注册旧版本删除回调（由 strm 包在初始化时调用）。
func SetDeleteOldVersionCallback(fn DeleteOldVersionFn) {
	deleteOldVersion = fn
}

// trashOldVersion 删除旧版本文件（优先使用回调完整四件套，兜底仅网盘回收站 + 整理历史）。
func (e *TransferExecutor) trashOldVersion(ctx context.Context, oldFileID, oldTargetPath string) {
	// 旧版本被删掉后目标目录列表就过期了，这里沿用原有的「整批清空」策略（不动语义），
	// 只是改成加锁访问。注意本函数是在 TransferFile 的批量循环中途按文件调用的，
	// 整批任务中途会反复丢缓存 —— 这是既有的性能问题，另议。
	e.cacheResetListing()
	if deleteOldVersion != nil {
		deleteOldVersion(ctx, oldFileID, oldTargetPath, e.History)
		return
	}
	if ok, _ := e.Client.TrashFile(ctx, oldFileID); ok {
		log.Printf("已删除旧版本文件: file_id=%s", oldFileID)
	}
	e.History.DeleteByFileID(oldFileID)
}

// voidOldVersion 同批次内被覆盖整理删除的旧文件：撤销其 success 计数与分组统计。
// 场景：一个分享含同一部片子的多个版本，后处理的版本把先处理、刚入库的版本当"旧版本"删掉，
// 若不撤销，success 会虚高（本次实测 3 个版本入库后只剩 1 个，success 却报 3）。
func (e *TransferExecutor) voidOldVersion(stats *TransferStats, old *HistoryRecord, media *MediaInfo, meta *MetaInfo) {
	stats.Success--
	key := getGroupKey(media, meta)
	g := stats.Groups[key]
	if g == nil {
		return
	}
	g.FileCount--
	g.TotalSize -= old.FileSize
	if g.MediaType == "tv" && old.Episode.Valid && old.Episode.Int64 > 0 {
		ep := int(old.Episode.Int64)
		delete(g.EpisodeSizes, ep)
		for i, v := range g.Episodes {
			if v == ep {
				g.Episodes = append(g.Episodes[:i], g.Episodes[i+1:]...)
				break
			}
		}
	}
}

// TransferFile 整理单个文件的主流程。
func (e *TransferExecutor) TransferFile(ctx context.Context, fileID, fileName string, sourcePID int, force bool, scrape *bool, transferType string, fileSize int64, parentDirs []string, stats *TransferStats) TransferResult {
	// 0. 参数兜底
	if scrape == nil {
		s := e.EnableScrape
		scrape = &s
	}
	if transferType == "" {
		transferType = e.DefaultTransferType
	}

	// 1. 跳过非视频/字幕文件
	if !(IsVideoFile(fileName) || IsSubtitleFile(fileName)) {
		return TransferResult{Message: "非视频/字幕文件，跳过", Skipped: true, FileID: fileID, FileName: fileName}
	}

	// 2. 跳过指定扩展名
	ext := extOf(fileName)
	if e.SkipExts[ext] {
		return TransferResult{Message: fmt.Sprintf("扩展名 %s 在跳过列表", ext), Skipped: true, FileID: fileID, FileName: fileName}
	}

	// 3. 历史检查
	if !force && e.History.Exists(fileID) {
		return TransferResult{Message: "已整理过，跳过", Skipped: true, FileID: fileID, FileName: fileName}
	}

	// 4. 文件名识别
	meta := Recognize(fileName, "", parentDirs)
	if meta.Name == "" {
		e.recordHistory(ctx, fileID, fileName, sourcePID, 0, "", nil, &meta, "fail",
			fmt.Sprintf("文件名识别失败: %s", fileName), transferType, 0)
		return TransferResult{Message: fmt.Sprintf("文件名识别失败: %s", fileName), FileID: fileID, FileName: fileName}
	}

	// 5. TMDB 识别
	var media *MediaInfo
	if e.TMDB != nil {
		mtype := meta.Type
		if mtype == "" {
			mtype = "movie"
		}
		search := func(name string, year, id int) (m *MediaInfo) {
			if id > 0 {
				m = e.TMDB.GetDetail(id, mtype)
				if m != nil {
					log.Printf("TMDB 按 ID 匹配: %s (id=%d)", m.Title, id)
				}
			}
			if m == nil {
				m = e.TMDB.Search(name, year, mtype)
			}
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("TMDB 搜索失败（降级用文件名）: %v", r)
					media = nil
				}
			}()
			media = search(meta.Name, meta.Year, meta.TMDBID)
			// 父目录名重试：文件名 TMDB 搜索失败时，复用 Recognize 的父目录识别再搜（电影/剧集通用）
			if media == nil && len(parentDirs) > 0 {
				parent := Recognize("", mtype, parentDirs)
				if parent.Name != "" {
					// 优先用文件自身的年份搜索，避免同类不同年份撞名错配（如 1978 老版 vs 2020 重制）
					year := parent.Year
					if meta.Year > 0 {
						year = meta.Year
					}
					if m := search(parent.Name, year, parent.TMDBID); m != nil {
						media = m
						meta.Name = parent.Name
						if meta.Year == 0 {
							meta.Year = parent.Year
						}
						if meta.Type == "" {
							meta.Type = m.Type
						}
						log.Printf("父目录TMDB重试命中: '%s' -> '%s' (id=%d)", fileName, parent.Name, m.TMDBID)
					}
				}
			}
		}()
		if media != nil {
			if meta.Type == "" {
				meta.Type = media.Type
			}
			media.Category = e.CatHelper.Match(media)
		}
	}

	// 6. 无 TMDB 信息时降级构造
	if media == nil {
		media = &MediaInfo{
			Title:    meta.Name,
			Year:     meta.Year,
			Type:     meta.Type,
			Category: "未分类",
		}
		if media.Type == "" {
			media.Type = "movie"
		}
	}

	// 6.5 同集去重检查
	mtype := media.Type
	if mtype == "" {
		mtype = meta.Type
	}
	if mtype == "" {
		mtype = "movie"
	}
	if !force && mtype == "tv" && meta.Season > 0 && meta.Episode > 0 && fileSize > 0 {
		existing := e.History.FindSameEpisode(media.TMDBID, meta.Season, meta.Episode, media.Title)
		if len(existing) > 0 {
			old := existing[0]
			oldSize := old.FileSize
			oldFileID := old.FileID
			oldVersion := old.Version
			newVersion := e.versionScore(&meta)
			replace, reason := e.dedupDecision(newVersion, oldVersion, fileSize, oldSize)
			if replace {
				log.Printf("同集去重: S%02dE%02d 覆盖整理，%s", meta.Season, meta.Episode, reason)
				if stats != nil && stats.Moved[oldFileID] {
					delete(stats.Moved, oldFileID)
					e.voidOldVersion(stats, old, media, &meta)
				}
				e.trashOldVersion(ctx, oldFileID, old.TargetPath)
			} else {
				log.Printf("同集去重: S%02dE%02d 保留旧版，%s", meta.Season, meta.Episode, reason)
				if ok, _ := e.Client.TrashFile(ctx, fileID); ok {
					log.Printf("已删除跳过的新文件（移入回收站）: file_id=%s", fileID)
				}
				return TransferResult{Message: fmt.Sprintf("同集已有更优版本 (%dB)，跳过", oldSize),
					Skipped: true, FileID: fileID, FileName: fileName, Media: media, Meta: &meta}
			}
		}
	}

	// 6.6 电影版本去重检查
	if !force && mtype == "movie" && fileSize > 0 {
		yearStr := ""
		if media.Year > 0 {
			yearStr = strconv.Itoa(media.Year)
		}
		existing := e.History.FindSameMovie(media.TMDBID, media.Title, yearStr)
		if len(existing) > 0 {
			old := existing[0]
			oldSize := old.FileSize
			oldFileID := old.FileID
			oldVersion := old.Version
			newVersion := e.versionScore(&meta)
			replace, reason := e.dedupDecision(newVersion, oldVersion, fileSize, oldSize)
			if replace {
				log.Printf("电影版本去重: %s 覆盖整理，%s", media.Title, reason)
				if stats != nil && stats.Moved[oldFileID] {
					delete(stats.Moved, oldFileID)
					e.voidOldVersion(stats, old, media, &meta)
				}
				e.trashOldVersion(ctx, oldFileID, old.TargetPath)
			} else {
				log.Printf("电影版本去重: %s 保留旧版，%s", media.Title, reason)
				if ok, _ := e.Client.TrashFile(ctx, fileID); ok {
					log.Printf("已删除跳过的新文件（移入回收站）: file_id=%s", fileID)
				}
				return TransferResult{Message: fmt.Sprintf("已有更优版本 (%dB)，跳过", oldSize),
					Skipped: true, FileID: fileID, FileName: fileName, Media: media, Meta: &meta}
			}
		}
	}

	// 7. 目录匹配
	dirConf := e.DirHelper.Match(mtype, media.Category)
	if dirConf == nil {
		return TransferResult{Message: fmt.Sprintf("未匹配到目标目录 (type=%s, category=%s)", mtype, media.Category),
			FileID: fileID, FileName: fileName}
	}
	effectiveTransferType := transferType
	if effectiveTransferType == "" {
		effectiveTransferType = dirConf.TransferType
	}
	if effectiveTransferType == "" {
		effectiveTransferType = e.DefaultTransferType
	}

	// 8. 计算目标相对路径
	targetRelPath := BuildTargetPath(meta, media, e.MovieFormat, e.TVFormat, 0)
	if targetRelPath == "" {
		return TransferResult{Message: "目标路径计算失败", FileID: fileID, FileName: fileName}
	}

	// 8.1 自动在路径前面加上分类目录
	typeDir := "电影"
	if mtype == "tv" {
		typeDir = "电视剧"
	}
	category := media.Category
	firstPart := ""
	if idx := strings.IndexByte(targetRelPath, '/'); idx >= 0 {
		firstPart = targetRelPath[:idx]
	}
	if firstPart != typeDir && firstPart != category {
		prefix := typeDir
		if category != "" {
			prefix += "/" + category
		}
		targetRelPath = prefix + "/" + targetRelPath
	}

	// 拆分为目录链 + 文件名
	parts := splitPath(targetRelPath)
	if len(parts) == 0 {
		return TransferResult{Message: "目标路径为空", FileID: fileID, FileName: fileName}
	}

	newBasename := pan123.EscapeFileName(parts[len(parts)-1])
	oldExt := extOf(fileName)
	if oldExt != "" && !strings.HasSuffix(strings.ToLower(newBasename), strings.ToLower(oldExt)) {
		newBasename = newBasename + oldExt
	}
	dirParts := parts[:len(parts)-1]
	// 重命名目标统一转义非法字符（123 云盘禁止半角冒号等，实测含 : 的 rename 会静默失败），
	// 并同步重建目标相对路径：保证 rename / findTargetFileID / 整理历史 / 联动 STRM 匹配
	// 使用的文件名与网盘实际文件名完全一致
	targetRelPath = strings.Join(append(append([]string{}, dirParts...), newBasename), "/")

	// 9. 递归创建目录
	targetPID, err := e.ensureDirs(ctx, dirConf.LibraryPID, dirParts)
	if err != nil {
		msg := fmt.Sprintf("创建目标目录失败: %v", err)
		log.Printf("%s", msg)
		e.recordHistory(ctx, fileID, fileName, sourcePID, dirConf.LibraryPID, targetRelPath, media, &meta, "fail",
			msg, effectiveTransferType, 0)
		return TransferResult{Message: msg, FileID: fileID, FileName: fileName}
	}

	// 10. 重命名 + 移动
	moveErr := e.moveFile(ctx, fileID, fileName, newBasename, targetPID, effectiveTransferType)
	if moveErr != nil {
		// 123 云盘 code -3: 文件已在目标文件夹，视为跳过而非失败
		if apiErr, ok := moveErr.(*pan123.APIError); ok && apiErr.Code == -3 {
			msg := fmt.Sprintf("文件已在目标目录，跳过: %s", newBasename)
			log.Printf("%s", msg)
			realFID := e.findTargetFileID(ctx, targetPID, newBasename, fileSize)
			if realFID != "" {
				e.recordHistory(ctx, realFID, fileName, sourcePID, targetPID, targetRelPath, media, &meta,
					"success", "", effectiveTransferType, fileSize)
			}
			return TransferResult{Message: msg, Skipped: true, FileID: fileID, FileName: fileName, Media: media, Meta: &meta}
		}
		msg := fmt.Sprintf("移动文件失败: %v", moveErr)
		log.Printf("%s", msg)
		e.recordHistory(ctx, fileID, fileName, sourcePID, targetPID, targetRelPath, media, &meta, "fail",
			msg, effectiveTransferType, 0)
		return TransferResult{Message: msg, FileID: fileID, FileName: fileName, Media: media, Meta: &meta}
	}
	log.Printf("移动完成: %s -> pid=%d", newBasename, targetPID)

	// 11. 刮削
	if *scrape && e.Scraper != nil && IsVideoFile(fileName) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("刮削失败（不影响整理结果）: %v", r)
				}
			}()
			stem := newBasename
			if i := strings.LastIndexByte(newBasename, '.'); i >= 0 {
				stem = newBasename[:i]
			}
			e.Scraper.Scrape(ctx, media, meta, targetPID, stem)
		}()
	}

	// 12. 写历史记录
	realFileID := e.findTargetFileID(ctx, targetPID, newBasename, fileSize)
	if realFileID == "" {
		realFileID = fileID
	}
	e.recordHistory(ctx, realFileID, fileName, sourcePID, targetPID, targetRelPath, media, &meta,
		"success", "", effectiveTransferType, fileSize)

	// 13. 文件大小
	if fileSize == 0 {
		if detail, err := e.Client.FSDetail(ctx, fileID); err == nil && detail.Size > 0 {
			fileSize = detail.Size
		}
	}

	// 记录本次已移入目标库的文件 id（源 id + 移动后 id），供同批次覆盖整理撤销计数
	if stats != nil {
		stats.Moved[fileID] = true
		if realFileID != "" {
			stats.Moved[realFileID] = true
		}
	}

	return TransferResult{
		Success:    true,
		FileID:     fileID,
		FileName:   fileName,
		TargetPath: targetRelPath,
		MediaTitle: media.Title,
		Media:      media,
		Meta:       &meta,
		FileSize:   fileSize,
	}
}

// moveFile 重命名 + 移动。
func (e *TransferExecutor) moveFile(ctx context.Context, fileID, oldName, newName string, targetPID int, transferType string) error {
	if newName != oldName {
		if _, err := e.Client.RenameFile(ctx, fileID, newName); err != nil {
			log.Printf("重命名失败（继续移动）: %v", err)
		} else {
			log.Printf("重命名: %s -> %s", oldName, newName)
		}
	}
	if transferType == "copy" {
		_, err := e.Client.FSCopy(ctx, fileID, targetPID)
		return err
	}
	_, err := e.Client.FSMove(ctx, []int64{toI64(fileID)}, targetPID)
	return err
}

// TransferDirectory 整理整个目录（遍历 + 批量整理）。
//
// 这是整理任务唯一的对外入口，也负责在顶层任务结束时主动回收内存。
// 内部实现里 targetListingCache 是任务级缓存（最多 500 个目录的完整文件列表，是整批任务的
// 内存大头），由 transferDirectory 的 defer 置 nil —— 必须等它返回之后再回收，否则缓存还挂在
// e 上，等于白回收。递归子目录（dirNames 非空）不回收，避免每层目录都触发一次 STW GC。
func (e *TransferExecutor) TransferDirectory(ctx context.Context, sourcePID int, recursive, force bool, scrape *bool, transferType string, sendNotify bool, dirNames []string) *TransferStats {
	stats := e.transferDirectory(ctx, sourcePID, recursive, force, scrape, transferType, sendNotify, dirNames)
	if len(dirNames) == 0 {
		gcguard.Reclaim()
	}
	return stats
}

// transferDirectory 整理目录的实际实现；递归调用自身，不触发内存回收。
func (e *TransferExecutor) transferDirectory(ctx context.Context, sourcePID int, recursive, force bool, scrape *bool, transferType string, sendNotify bool, dirNames []string) *TransferStats {
	stats := &TransferStats{Groups: map[groupKey]*GroupSummary{}, Moved: map[string]bool{}}
	if e.Client == nil {
		log.Printf("整理目录失败: 123 客户端未初始化 (source_pid=%d)", sourcePID)
		stats.FailList = append(stats.FailList, [2]string{strconv.Itoa(sourcePID), "123 客户端未初始化，请检查 ENV_123_CLIENT_ID/SECRET 配置"})
		return stats
	}
	// 任务级缓存只在顶层任务创建和释放，避免长时间持有目录列表。
	if len(dirNames) == 0 {
		e.cacheResetListing()
		defer e.cacheReleaseListing()
	}
	log.Printf("开始整理目录: source_pid=%d, recursive=%v", sourcePID, recursive)

	items, err := e.Client.FSList(ctx, sourcePID)
	if err != nil {
		log.Printf("列举目录失败 (pid=%d): %v", sourcePID, err)
		stats.FailList = append(stats.FailList, [2]string{strconv.Itoa(sourcePID), fmt.Sprintf("列举目录失败: %v", err)})
		return stats
	}

	for _, item := range items {
		itemID := strconv.FormatInt(item.FileID, 10)
		itemName := item.FileName
		itemSize := item.Size

		// 目录：递归
		if item.Type == 1 && recursive {
			subNames := append(append([]string{}, dirNames...), itemName)
			subStats := e.transferDirectory(ctx, int(item.FileID), recursive, force, scrape, transferType, false, subNames)
			stats.Success += subStats.Success
			stats.Fail += subStats.Fail
			stats.Skip += subStats.Skip
			stats.FailList = append(stats.FailList, subStats.FailList...)
			stats.SkipList = append(stats.SkipList, subStats.SkipList...)
			mergeGroups(stats.Groups, subStats.Groups)
			for mid := range subStats.Moved {
				stats.Moved[mid] = true
			}
			// 递归完成后，若子目录已空则删除空目录
			if (subStats.Success + subStats.Skip) > 0 {
				remaining, err := e.Client.FSList(ctx, item.FileID)
				if err == nil && len(remaining) == 0 {
					if ok, _ := e.Client.TrashFile(ctx, item.FileID); ok {
						log.Printf("已删除空目录: %s (pid=%s)", itemName, itemID)
					}
				}
			}
			continue
		}

		// 文件：跳过过小文件
		if item.Type != 1 && itemSize > 0 && itemSize < e.MinFilesize {
			log.Printf("跳过过小文件: %s (%d bytes)", itemName, itemSize)
			stats.Skip++
			continue
		}

		// 文件：整理
		if item.Type != 1 {
			result := e.TransferFile(ctx, itemID, itemName, sourcePID, force, scrape, transferType, itemSize, dirNames, stats)
			if result.Success {
				stats.Success++
				accumulateToGroups(stats.Groups, result)
			} else if result.Skipped {
				stats.Skip++
				skipTitle := result.FileName
				if result.Media != nil && result.Media.Title != "" {
					skipTitle = result.Media.Title
				} else if result.Meta != nil && result.Meta.Name != "" {
					skipTitle = result.Meta.Name
				}
				msg := result.Message
				if msg == "" {
					msg = "已存在"
				}
				stats.SkipList = append(stats.SkipList, [2]string{skipTitle, msg})
			} else {
				stats.Fail++
				stats.FailList = append(stats.FailList, [2]string{itemName, result.Message})
				accumulateFailToGroups(stats.Groups, result)
			}
		}
	}

	log.Printf("目录整理完成: source_pid=%d, success=%d, fail=%d, skip=%d", sourcePID, stats.Success, stats.Fail, stats.Skip)

	// 仅在顶层调用且 sendNotify 时发送批量通知
	if sendNotify {
		if len(stats.Groups) > 0 {
			e.sendBatchNotifications(stats.Groups)
		}
		if stats.Skip > 0 {
			e.sendSkipNotifications(stats)
		}
	}
	return stats
}

// TransferFileByID 根据 file_id 整理（自动获取文件名）。
func (e *TransferExecutor) TransferFileByID(ctx context.Context, fileID string, sourcePID int, force bool) TransferResult {
	// 单文件整理自带一份干净的目录列表缓存；加锁是因为后台批量整理可能同时在跑。
	e.cacheResetListing()
	detail, err := e.Client.FSDetail(ctx, fileID)
	if err != nil {
		return TransferResult{Message: fmt.Sprintf("获取文件详情失败: %v", err), FileID: fileID}
	}
	fileName := detail.FileName
	if fileName == "" {
		return TransferResult{Message: "无法获取文件名", FileID: fileID}
	}
	return e.TransferFile(ctx, fileID, fileName, sourcePID, force, nil, "", detail.Size, nil, nil)
}

// ============ 分组聚合与批量通知 ============

// getGroupKey 生成分组键。
func getGroupKey(media *MediaInfo, meta *MetaInfo) groupKey {
	mtype := "movie"
	if media != nil && media.Type != "" {
		mtype = media.Type
	} else if meta != nil && meta.Type != "" {
		mtype = meta.Type
	}
	var mediaKey string
	if media != nil && media.TMDBID > 0 {
		mediaKey = fmt.Sprintf("tmdb_%d", media.TMDBID)
	} else {
		title := ""
		if media != nil && media.Title != "" {
			title = media.Title
		} else if meta != nil {
			title = meta.Name
		}
		mediaKey = "title_" + title
	}
	season := 0
	if meta != nil && meta.Season > 0 {
		season = meta.Season
	}
	if mtype != "tv" {
		season = 0
	}
	return groupKey{MediaKey: mediaKey, Season: season}
}

func newGroup(media *MediaInfo, meta *MetaInfo) *GroupSummary {
	g := &GroupSummary{
		EpisodeSizes: map[int]int64{},
	}
	if media != nil {
		g.Title = media.Title
		g.Year = media.Year
		g.MediaType = media.Type
		g.Category = media.Category
		g.TMDBID = media.TMDBID
		g.VoteAverage = media.VoteAverage
		g.PosterURL = buildPosterURL(media)
	}
	if g.Title == "" && meta != nil {
		g.Title = meta.Name
	}
	if g.Title == "" {
		g.Title = "未知标题"
	}
	if g.MediaType == "" && meta != nil {
		g.MediaType = meta.Type
	}
	if g.MediaType == "" {
		g.MediaType = "movie"
	}
	if meta != nil {
		g.Season = meta.Season
	}
	return g
}

func accumulateToGroups(groups map[groupKey]*GroupSummary, result TransferResult) {
	media, meta := result.Media, result.Meta
	if media == nil && meta == nil {
		return
	}
	key := getGroupKey(media, meta)
	g, ok := groups[key]
	if !ok {
		g = newGroup(media, meta)
		groups[key] = g
	}
	// 电视剧覆盖整理：同一集已有记录时，替换旧记录
	isOverwrite := false
	if g.MediaType == "tv" && meta != nil && meta.Episode > 0 {
		ep := meta.Episode
		if _, exists := g.EpisodeSizes[ep]; exists {
			g.TotalSize -= g.EpisodeSizes[ep]
			isOverwrite = true
		}
		g.EpisodeSizes[ep] = result.FileSize
	}
	if !isOverwrite {
		g.FileCount++
	}
	g.TotalSize += result.FileSize
	if g.MediaType == "tv" && meta != nil && meta.Episode > 0 {
		if !containsInt(g.Episodes, meta.Episode) {
			g.Episodes = append(g.Episodes, meta.Episode)
		}
	}
	if g.ResourceTerm == "" && meta != nil {
		parts := []string{}
		if meta.Resolution != "" {
			parts = append(parts, meta.Resolution)
		}
		if meta.Source != "" {
			parts = append(parts, meta.Source)
		}
		if meta.RawName != "" {
			effect := ExtractEffects(meta.RawName)
			if effect != "" {
				parts = append(parts, effect)
			}
		}
		if len(parts) > 0 {
			g.ResourceTerm = strings.Join(parts, " ")
		}
	}
	if result.TargetPath != "" {
		g.TargetPaths = append(g.TargetPaths, result.TargetPath)
	}
}

// buildPosterURL 构建 TMDB 海报图片 URL。
func buildPosterURL(media *MediaInfo) string {
	if media == nil {
		return ""
	}
	if media.BackdropPath != "" {
		return "https://image.tmdb.org/t/p/w1280" + media.BackdropPath
	}
	if media.PosterPath != "" {
		return "https://image.tmdb.org/t/p/w500" + media.PosterPath
	}
	return ""
}

func accumulateFailToGroups(groups map[groupKey]*GroupSummary, result TransferResult) {
	media, meta := result.Media, result.Meta
	if media == nil && meta == nil {
		key := groupKey{MediaKey: "_unrecognized"}
		g, ok := groups[key]
		if !ok {
			g = &GroupSummary{Title: "未识别文件", MediaType: "unknown", EpisodeSizes: map[int]int64{}}
			groups[key] = g
		}
		g.FailNames = append(g.FailNames, result.FileName)
		return
	}
	key := getGroupKey(media, meta)
	g, ok := groups[key]
	if !ok {
		g = newGroup(media, meta)
		groups[key] = g
	}
	g.FailNames = append(g.FailNames, result.FileName)
}

// mergeGroups 合并子目录的分组聚合结果到父目录。
func mergeGroups(dest, src map[groupKey]*GroupSummary) {
	for key, g := range src {
		d, ok := dest[key]
		if !ok {
			d = &GroupSummary{
				Title: g.Title, Year: g.Year, MediaType: g.MediaType, Category: g.Category,
				TMDBID: g.TMDBID, Season: g.Season, VoteAverage: g.VoteAverage,
				PosterURL: g.PosterURL, EpisodeSizes: map[int]int64{},
			}
			dest[key] = d
		}
		if g.MediaType == "tv" && len(g.EpisodeSizes) > 0 {
			for ep, epSize := range g.EpisodeSizes {
				if oldSize, exists := d.EpisodeSizes[ep]; exists {
					if epSize > oldSize {
						d.TotalSize += epSize - oldSize
						d.EpisodeSizes[ep] = epSize
					}
				} else {
					d.EpisodeSizes[ep] = epSize
					d.FileCount++
					d.TotalSize += epSize
					if !containsInt(d.Episodes, ep) {
						d.Episodes = append(d.Episodes, ep)
					}
				}
			}
		} else {
			d.FileCount += g.FileCount
			d.TotalSize += g.TotalSize
			for _, ep := range g.Episodes {
				if !containsInt(d.Episodes, ep) {
					d.Episodes = append(d.Episodes, ep)
				}
			}
		}
		for _, p := range g.TargetPaths {
			if !containsStr(d.TargetPaths, p) {
				d.TargetPaths = append(d.TargetPaths, p)
			}
		}
		d.FailNames = append(d.FailNames, g.FailNames...)
		if d.ResourceTerm == "" && g.ResourceTerm != "" {
			d.ResourceTerm = g.ResourceTerm
		}
		if d.PosterURL == "" && g.PosterURL != "" {
			d.PosterURL = g.PosterURL
		}
	}
}

// formatTotalSize 字节转可读字符串。
func formatTotalSize(sizeBytes int64) string {
	if sizeBytes <= 0 {
		return "0B"
	}
	size := float64(sizeBytes)
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.2fGB", size/(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1fMB", size/(1<<20))
	case size >= 1024:
		return fmt.Sprintf("%.1fKB", size/1024)
	}
	return fmt.Sprintf("%dB", int(size))
}

// formatSeasonEpisodes 格式化季集信息。
func formatSeasonEpisodes(season int, episodes []int) string {
	if season <= 0 {
		return ""
	}
	sFmt := fmt.Sprintf("S%02d", season)
	if len(episodes) == 0 {
		return sFmt
	}
	eps := append([]int{}, episodes...)
	sort.Ints(eps)
	eps = uniqueSorted(eps)
	if len(eps) == 1 {
		return fmt.Sprintf("%s E%02d", sFmt, eps[0])
	}
	// 连续区间合并
	var ranges [][2]int
	start := eps[0]
	prev := eps[0]
	for _, e := range eps[1:] {
		if e == prev+1 {
			prev = e
			continue
		}
		ranges = append(ranges, [2]int{start, prev})
		start = e
		prev = e
	}
	ranges = append(ranges, [2]int{start, prev})
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r[0] == r[1] {
			parts = append(parts, fmt.Sprintf("E%02d", r[0]))
		} else {
			parts = append(parts, fmt.Sprintf("E%02d-E%02d", r[0], r[1]))
		}
	}
	return fmt.Sprintf("%s %s", sFmt, strings.Join(parts, " "))
}

// sendBatchNotifications 遍历分组发送批量通知。
func (e *TransferExecutor) sendBatchNotifications(groups map[groupKey]*GroupSummary) {
	keys := sortedKeys(groups)
	for _, key := range keys {
		g := groups[key]
		// 全失败的分组：发送失败通知
		if g.FileCount == 0 && len(g.FailNames) > 0 {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("构建失败通知失败 (key=%v): %v", key, r)
					}
				}()
				failCount := len(g.FailNames)
				failDisplay := strings.Join(g.FailNames[:min(5, failCount)], ", ")
				more := ""
				if failCount > 5 {
					more = fmt.Sprintf(" 等%d个", failCount)
				}
				var title, text string
				if g.MediaType == "unknown" {
					title = fmt.Sprintf("❌ %d 个文件整理失败", failCount)
					text = fmt.Sprintf("以下文件无法识别媒体信息：\n%s%s", failDisplay, more)
				} else {
					titleYear := g.Title
					if g.Year > 0 {
						titleYear += fmt.Sprintf(" (%d)", g.Year)
					}
					seasonEpisode := ""
					if g.MediaType == "tv" {
						seasonEpisode = formatSeasonEpisodes(g.Season, g.Episodes)
					}
					title = fmt.Sprintf("❌ %s%s 整理失败", titleYear, withSpace(seasonEpisode))
					text = fmt.Sprintf("共 %d 个文件处理失败：\n%s%s", failCount, failDisplay, more)
				}
				Notify(title+"\n"+text, "")
			}()
			continue
		}
		// 跳过既没成功也没失败的空分组
		if g.FileCount == 0 {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("构建分组通知失败 (key=%v): %v", key, r)
					Notify(fmt.Sprintf("✅ 整理完成: %s 共 %d 个文件", g.Title, g.FileCount), "")
				}
			}()
			titleYear := g.Title
			if g.Year > 0 {
				titleYear += fmt.Sprintf(" (%d)", g.Year)
			}
			seasonEpisode := ""
			if g.MediaType == "tv" {
				seasonEpisode = formatSeasonEpisodes(g.Season, g.Episodes)
			}
			typeDisplay := g.Category
			if typeDisplay == "" {
				if g.MediaType == "movie" {
					typeDisplay = "电影"
				} else {
					typeDisplay = "电视剧"
				}
			}
			totalSizeStr := formatTotalSize(g.TotalSize)
			failLine := ""
			if len(g.FailNames) > 0 {
				failDisplay := strings.Join(g.FailNames[:min(5, len(g.FailNames))], ", ")
				more := ""
				if len(g.FailNames) > 5 {
					more = fmt.Sprintf(" 等%d个", len(g.FailNames))
				}
				failLine = fmt.Sprintf("\n❌ 以下文件处理失败：%s%s", failDisplay, more)
			}
			title := fmt.Sprintf("%s%s 已入库", titleYear, withSpace(seasonEpisode))
			lines := []string{}
			if g.VoteAverage > 0 {
				lines = append(lines, fmt.Sprintf("⭐ TMDB评分：%.1f", g.VoteAverage))
			}
			if typeDisplay != "" {
				lines = append(lines, fmt.Sprintf("🎬 类型：%s", typeDisplay))
			}
			if g.ResourceTerm != "" {
				lines = append(lines, fmt.Sprintf("💠 质量：%s", g.ResourceTerm))
			}
			lines = append(lines, fmt.Sprintf("📦 体积：%d 个文件，共 %s", g.FileCount, totalSizeStr))
			if failLine != "" {
				lines = append(lines, strings.TrimPrefix(failLine, "\n"))
			}
			Notify(title+"\n"+strings.Join(lines, "\n"), g.PosterURL)
		}()
	}
}

// sendSkipNotifications 发送跳过整理通知。
func (e *TransferExecutor) sendSkipNotifications(stats *TransferStats) {
	successTitles := map[string]bool{}
	for _, g := range stats.Groups {
		if g.FileCount > 0 && g.Title != "" {
			successTitles[g.Title] = true
		}
	}
	var titles []string
	for _, sl := range stats.SkipList {
		title := sl[0]
		if title == "" {
			title = "未知"
		}
		if successTitles[title] {
			continue
		}
		if !containsStr(titles, title) {
			titles = append(titles, title)
		}
	}
	var lines []string
	for i, title := range titles {
		if i >= 10 {
			break
		}
		lines = append(lines, fmt.Sprintf("🚫『%s』未入库|已存在", title))
	}
	if len(titles) > 10 {
		lines = append(lines, fmt.Sprintf("... 还有 %d 个资源未显示", len(titles)-10))
	}
	if len(lines) > 0 {
		Notify(strings.Join(lines, "\n"), "")
	}
}

// ============ 内部辅助方法 ============

// ensureDirs 递归创建目录，返回最深层目录 ID。
func (e *TransferExecutor) ensureDirs(ctx context.Context, rootPID int, dirParts []string) (int, error) {
	currentPID := rootPID
	e.cacheResetDirPIDIfFull()
	for _, part := range dirParts {
		cacheKey := fmt.Sprintf("%d|%s", currentPID, part)
		if cached, ok := e.cacheGetDirPID(cacheKey); ok {
			currentPID = cached
			continue
		}
		// 注意：下面 findSubdir / FSMkdir 都是网络调用，缓存锁必须在进循环前就放开，
		// 不能把锁持到整个循环外面。
		existing, err := e.findSubdir(ctx, currentPID, part)
		if err == nil && existing > 0 {
			currentPID = existing
			e.cachePutDirPID(cacheKey, existing)
		} else {
			resp, err := e.Client.FSMkdir(ctx, part, currentPID, 1)
			if err != nil {
				return 0, err
			}
			if !resp.IsSuccess() {
				return 0, &pan123.APIError{Code: resp.Code, Message: resp.MessageString()}
			}
			newID, err := parseFileIDFromResp(resp)
			if err != nil {
				return 0, err
			}
			currentPID = newID
			e.cachePutDirPID(cacheKey, newID)
			log.Printf("创建目录: %s -> ID: %d", part, newID)
		}
	}
	return currentPID, nil
}

// findSubdir 在 parentPID 下查找名为 name 的子目录。
func (e *TransferExecutor) findSubdir(ctx context.Context, parentPID int, name string) (int, error) {
	items, err := e.Client.FSList(ctx, parentPID)
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		if item.Type == 1 && item.FileName == name {
			return int(item.FileID), nil
		}
	}
	return 0, nil
}

// ============ 任务级缓存的加锁访问 ============
//
// 下面这组方法把 targetListingCache / dirPIDCache 的所有读写收敛到一处并统一加锁。
// 缓存挂在共享的 executor 上，后台整理任务与 Web 单文件整理会并发访问：
// 无锁时并发写 map 是 fatal error: concurrent map writes（不可 recover，进程直接退出）。
// 每个方法内部只做 map 操作，绝不在持锁时调用网络接口。

// cacheResetListing 换一份空的目录列表缓存（顶层任务开始 / 单文件整理开始 / 旧版本删除后）。
func (e *TransferExecutor) cacheResetListing() {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	e.targetListingCache = map[int64][]pan123.FileInfo{}
}

// cacheReleaseListing 顶层任务结束时释放目录列表缓存，让这批大对象能被随后的回收带走。
func (e *TransferExecutor) cacheReleaseListing() {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	e.targetListingCache = nil
}

// cacheGetListing 读目录列表缓存。
func (e *TransferExecutor) cacheGetListing(pid int64) ([]pan123.FileInfo, bool) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	items, ok := e.targetListingCache[pid]
	return items, ok
}

// cachePutListing 写目录列表缓存；超容量只停止缓存、不淘汰（沿用原策略）。
// 必须做 nil 兜底：顶层任务结束时缓存会被置 nil，此时 Web 侧并发发起单文件整理，
// 直接赋值会 panic: assignment to entry in nil map。
func (e *TransferExecutor) cachePutListing(pid int64, items []pan123.FileInfo) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if e.targetListingCache == nil {
		e.targetListingCache = map[int64][]pan123.FileInfo{}
	}
	if len(e.targetListingCache) < e.targetListingCacheMax {
		e.targetListingCache[pid] = items
	}
}

// cacheResetDirPIDIfFull 目录 PID 缓存超出上限时整体清空（沿用原策略，不做淘汰）。
func (e *TransferExecutor) cacheResetDirPIDIfFull() {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if len(e.dirPIDCache) > e.dirPIDCacheMax {
		e.dirPIDCache = map[string]int{}
	}
}

// cacheGetDirPID 读目录 PID 缓存。
func (e *TransferExecutor) cacheGetDirPID(key string) (int, bool) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	pid, ok := e.dirPIDCache[key]
	return pid, ok
}

// cachePutDirPID 写目录 PID 缓存。
func (e *TransferExecutor) cachePutDirPID(key string, pid int) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if e.dirPIDCache == nil {
		e.dirPIDCache = map[string]int{}
	}
	e.dirPIDCache[key] = pid
}

// listTargetDir 目标目录文件列表（任务级缓存）。
func (e *TransferExecutor) listTargetDir(ctx context.Context, pid int64) []pan123.FileInfo {
	if items, ok := e.cacheGetListing(pid); ok {
		return items
	}
	items, err := e.Client.FSList(ctx, pid)
	if err != nil {
		return nil
	}
	// 容量上限保护：超出时不再缓存，避免任务级内存峰值
	e.cachePutListing(pid, items)
	return items
}

// findTargetFileID 在目标目录按 文件名 + 大小 反查真实 file_id。
func (e *TransferExecutor) findTargetFileID(ctx context.Context, pid int, filename string, size int64) string {
	stem := filename
	if i := strings.LastIndexByte(filename, '.'); i >= 0 {
		stem = filename[:i]
	}
	var candidates [][2]any
	for _, item := range e.listTargetDir(ctx, int64(pid)) {
		if item.Type != 0 {
			continue
		}
		name := item.FileName
		if name == "" {
			continue
		}
		nameStem := name
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			nameStem = name[:i]
		}
		if nameStem == stem || strings.HasPrefix(nameStem, stem+"(") {
			if size > 0 && item.Size == size {
				return strconv.FormatInt(item.FileID, 10)
			}
			candidates = append(candidates, [2]any{item.Size, strconv.FormatInt(item.FileID, 10)})
		}
	}
	if len(candidates) > 0 {
		return candidates[0][1].(string)
	}
	return ""
}

// recordHistory 记录历史（异常不阻塞主流程）。
func (e *TransferExecutor) recordHistory(ctx context.Context, fileID, fileName string, sourcePID, targetPID int, targetPath string, media *MediaInfo, meta *MetaInfo, status, errorMsg, transferType string, fileSize int64) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("写入历史记录失败: %v", r)
			}
		}()
		mediaTitle, mediaYear, mediaType := "", "", ""
		var tmdbID, season, episode sql.NullInt64
		if media != nil {
			mediaTitle = media.Title
			mediaType = media.Type
			if media.Year > 0 {
				mediaYear = strconv.Itoa(media.Year)
			}
			if media.TMDBID > 0 {
				tmdbID = sql.NullInt64{Valid: true, Int64: int64(media.TMDBID)}
			}
		}
		if meta != nil {
			if mediaTitle == "" {
				mediaTitle = meta.Name
			}
			if mediaYear == "" && meta.Year > 0 {
				mediaYear = strconv.Itoa(meta.Year)
			}
			if mediaType == "" {
				mediaType = meta.Type
			}
			if meta.Season > 0 {
				season = sql.NullInt64{Valid: true, Int64: int64(meta.Season)}
			}
			if meta.Episode > 0 {
				episode = sql.NullInt64{Valid: true, Int64: int64(meta.Episode)}
			}
		}
		if transferType == "" {
			transferType = "move"
		}
		e.History.Add(HistoryRecord{
			FileID:       fileID,
			FileName:     fileName,
			SourcePID:    sourcePID,
			TargetPID:    targetPID,
			TargetPath:   targetPath,
			MediaTitle:   mediaTitle,
			MediaYear:    mediaYear,
			MediaType:    mediaType,
			TMDBID:       tmdbID,
			Season:       season,
			Episode:      episode,
			Status:       status,
			ErrorMsg:     errorMsg,
			TransferType: transferType,
			FileSize:     fileSize,
			Version:      e.versionScore(meta),
		})
	}()
}

// ---------- 工具 ----------

func splitPath(p string) []string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func toI64(v any) int64 {
	switch t := v.(type) {
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return 0
}

func parseFileIDFromResp(resp *pan123.Response) (int, error) {
	var data struct {
		Info struct {
			FileID any `json:"FileId"`
		} `json:"Info"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return 0, fmt.Errorf("无法解析响应: %v", err)
	}
	if n := toI64(data.Info.FileID); n > 0 {
		return int(n), nil
	}
	return 0, fmt.Errorf("无法从响应中获取目录 ID")
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func uniqueSorted(list []int) []int {
	var out []int
	for _, v := range list {
		if !containsInt(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func sortedKeys(m map[groupKey]*GroupSummary) []groupKey {
	keys := make([]groupKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].MediaKey < keys[j].MediaKey })
	return keys
}

func withSpace(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = path.Join
