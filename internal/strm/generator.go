// STRM 文件生成器（对应 generator.py）。
package strm

import (
	"context"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mmbot/internal/pan123"
)

// ---------- 网盘路径定位 ----------

// RawItemPayload 从 FileInfo 构造 download_info payload。
func RawItemPayload(raw *pan123.FileInfo) map[string]any {
	return map[string]any{
		"Etag":      raw.Etag,
		"FileID":    raw.FileID,
		"FileName":  raw.FileName,
		"S3KeyFlag": raw.Etag,
		"Size":      raw.Size,
	}
}

// ---------- FullSyncStrmHelper ----------

// FullSyncStrmHelper 全量生成 STRM 文件。
type FullSyncStrmHelper struct {
	client           *pan123.Client
	rmtMediaext      []string
	downloadMediaext []string
	serverAddress    string
	apiKey           string
	autoDownload     bool
	concurrency      int

	mu                 sync.Mutex
	strmCount          int
	strmFailCount      int
	mediainfoCount     int
	mediainfoFailCount int
	strmFailDict       map[string]string
	mediainfoFailList  []string
	downloadList       []DownloadItem
	mediainfoDl        *MediaInfoDownloader
}

// NewFullSyncStrmHelper 创建全量 STRM 生成器。
func NewFullSyncStrmHelper(client *pan123.Client, rmtMediaext, downloadMediaext string, serverAddress, apiKey string, autoDownload bool, concurrency int) *FullSyncStrmHelper {
	if concurrency < 1 {
		concurrency = 1
	}
	return &FullSyncStrmHelper{
		client:           client,
		rmtMediaext:      ParseMediaext(rmtMediaext),
		downloadMediaext: ParseMediaext(downloadMediaext),
		serverAddress:    strings.TrimRight(serverAddress, "/"),
		apiKey:           apiKey,
		autoDownload:     autoDownload,
		concurrency:      concurrency,
		strmFailDict:     make(map[string]string),
		downloadList:     make([]DownloadItem, 0, 64), // 预分配，减少 goroutine 内 append 扩容拷贝
		mediainfoDl:      NewMediaInfoDownloader(client),
	}
}

// writeStrm 写入单个 STRM 文件（线程安全）。
func (h *FullSyncStrmHelper) writeStrm(newFilePath string, raw *pan123.FileInfo) bool {
	dir := filepath.Dir(newFilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("【全量STRM生成】创建目录失败 %s: %v", dir, err)
		h.mu.Lock()
		h.strmFailCount++
		h.strmFailDict[newFilePath] = err.Error()
		h.mu.Unlock()
		return false
	}
	s3KeyFlag := raw.S3KeyFlag
	if s3KeyFlag == "" {
		s3KeyFlag = raw.Etag
	}
	strmURL := BuildRedirectURL(
		h.serverAddress,
		h.apiKey,
		raw.FileName,
		raw.Size,
		raw.Etag,
		s3KeyFlag,
		raw.FileID,
	)
	if err := os.WriteFile(newFilePath, []byte(strmURL), 0o644); err != nil {
		log.Printf("【全量STRM生成】写入 STRM 文件失败 %s: %v", newFilePath, err)
		h.mu.Lock()
		h.strmFailCount++
		h.strmFailDict[newFilePath] = err.Error()
		h.mu.Unlock()
		return false
	}
	h.mu.Lock()
	h.strmCount++
	h.mu.Unlock()
	log.Printf("【全量STRM生成】生成 STRM 文件成功: %s", newFilePath)
	return true
}

// GenerateStrmFiles 生成 STRM 文件（支持并发写入）。
func (h *FullSyncStrmHelper) GenerateStrmFiles(ctx context.Context, fullSyncStrmPaths, overwriteMode string) (int, bool) {
	lines := strings.Split(fullSyncStrmPaths, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "#", 2)
		if len(parts) < 2 {
			log.Printf("【全量STRM生成】映射格式错误，跳过: %s", line)
			continue
		}
		panMediaDir := strings.TrimSpace(parts[1])
		targetDir := strings.TrimSpace(parts[0])
		if panMediaDir == "" || targetDir == "" {
			log.Printf("【全量STRM生成】映射格式错误，跳过: %s", line)
			continue
		}

		parentID := GetDirIDByPath(ctx, h.client, panMediaDir)
		if parentID == 0 {
			log.Printf("【全量STRM生成】网盘媒体目录 ID 获取失败: %s", panMediaDir)
			return h.strmCount, false
		}
		log.Printf("【全量STRM生成】网盘媒体目录 ID 获取成功: %d", parentID)

		items, err := h.client.IterDirConcurrent(ctx, parentID, -1, true, time.Second, h.concurrency)
		if err != nil {
			log.Printf("【全量STRM生成】遍历目录失败: %v", err)
			return h.strmCount, false
		}

		// 并发处理：semaphore 控制 goroutine 数量
		sem := make(chan struct{}, h.concurrency)
		var wg sync.WaitGroup

		for item := range items {
			if item.IsDir {
				continue
			}
			raw := item.Raw
			if raw == nil {
				continue
			}
			relPath := item.RelPath
			filePath := filepath.Join(targetDir, relPath)
			fileTargetDir := filepath.Dir(filePath)
			stem := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filepath.Base(filePath)))
			newFilePath := filepath.Join(fileTargetDir, stem+".strm")

			ext := strings.ToLower(filepath.Ext(filePath))
			matched := false
			for _, rmtExt := range h.rmtMediaext {
				if ext == rmtExt {
					matched = true
					break
				}
			}
			if !matched {
				log.Printf("【全量STRM生成】跳过网盘路径: %s", relPath)
				continue
			}

			if _, err := os.Stat(newFilePath); err == nil {
				if overwriteMode == "never" {
					log.Printf("【全量STRM生成】%s 已存在，覆盖模式 %s，跳过", newFilePath, overwriteMode)
					continue
				}
				log.Printf("【全量STRM生成】%s 已存在，覆盖模式 %s", newFilePath, overwriteMode)
			}

			sem <- struct{}{}
			wg.Add(1)
			go func(raw *pan123.FileInfo, newFilePath, filePath, relPath string) {
				defer func() {
					<-sem
					wg.Done()
				}()

				// autoDownload：在 goroutine 内追加到 downloadList
				if h.autoDownload {
					if _, err := os.Stat(filePath); err == nil {
						if overwriteMode == "never" {
							return
						}
					}
					ext := strings.ToLower(filepath.Ext(filePath))
					for _, dlExt := range h.downloadMediaext {
						if ext == dlExt {
							h.mu.Lock()
							h.downloadList = append(h.downloadList, DownloadItem{
								Payload:   RawItemPayload(raw),
								LocalPath: filePath,
							})
							h.mu.Unlock()
							break
						}
					}
				}

				h.writeStrm(newFilePath, raw)
			}(raw, newFilePath, filePath, relPath)
		}
		wg.Wait()
	}

	if len(h.downloadList) > 0 {
		h.mediainfoCount, h.mediainfoFailCount, h.mediainfoFailList = h.mediainfoDl.AutoDownloader(ctx, h.downloadList, h.concurrency)
	}

	for fpath, errMsg := range h.strmFailDict {
		log.Printf("【全量STRM生成】%s 生成错误原因: %s", fpath, errMsg)
	}
	for _, fpath := range h.mediainfoFailList {
		log.Printf("【全量STRM生成】%s 下载错误", fpath)
	}
	log.Printf("【全量STRM生成】完成，生成 %d 个 STRM，下载 %d 个媒体数据文件", h.strmCount, h.mediainfoCount)
	if h.strmFailCount != 0 || h.mediainfoFailCount != 0 {
		log.Printf("【全量STRM生成】%d 个 STRM 生成失败，%d 个媒体数据文件下载失败", h.strmFailCount, h.mediainfoFailCount)
	}
	return h.strmCount, true
}

// ---------- TransferLinkedStrmGenerator ----------

// TransferLinkedStrmGenerator 整理联动 STRM 生成器。
type TransferLinkedStrmGenerator struct {
	client        *pan123.Client
	mappings      [][2]string
	serverAddress string
	apiKey        string
	rmtMediaext   []string
	concurrency   int
	mu            sync.Mutex
}

// NewTransferLinkedStrmGenerator 创建整理联动 STRM 生成器。
func NewTransferLinkedStrmGenerator(client *pan123.Client, fullSyncStrmPaths, serverAddress, apiKey, rmtMediaext string, concurrency int) *TransferLinkedStrmGenerator {
	if concurrency < 1 {
		concurrency = 1
	}
	g := &TransferLinkedStrmGenerator{
		client:        client,
		serverAddress: strings.TrimRight(serverAddress, "/"),
		apiKey:        apiKey,
		rmtMediaext:   ParseMediaext(rmtMediaext),
		concurrency:   concurrency,
	}
	for _, line := range strings.Split(fullSyncStrmPaths, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "#", 2)
		g.mappings = append(g.mappings, [2]string{strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])})
	}
	return g
}

// matchMapping 返回匹配的 (localDir, panDir) 映射。
func (g *TransferLinkedStrmGenerator) matchMapping(netdiskPath string) (string, string) {
	norm := strings.TrimSpace(strings.ReplaceAll(netdiskPath, "\\", "/"))
	for _, mapping := range g.mappings {
		localDir, panDir := mapping[0], mapping[1]
		if HasPrefix(norm, panDir) {
			return localDir, panDir
		}
	}
	if norm != "" && !strings.HasPrefix(norm, "/") {
		for _, mapping := range g.mappings {
			return mapping[0], mapping[1]
		}
	}
	return "", ""
}

// GenerateByPaths 根据网盘文件路径列表生成 STRM（支持并发写入）。
func (g *TransferLinkedStrmGenerator) GenerateByPaths(ctx context.Context, netdiskPaths []string, overwriteMode string) map[string]int {
	result := map[string]int{"success": 0, "fail": 0, "skip": 0}
	if len(netdiskPaths) == 0 {
		return result
	}

	parents := map[string][]string{}
	for _, p := range netdiskPaths {
		p = strings.ReplaceAll(p, "\\", "/")
		parent := path.Dir(p)
		parents[parent] = append(parents[parent], path.Base(p))
	}

	for parent, fnames := range parents {
		localDir, panDir := g.matchMapping(parent)
		if localDir == "" {
			log.Printf("【监控整理STRM生成】%s 未匹配到任何映射，跳过", parent)
			g.mu.Lock()
			result["fail"] += len(fnames)
			g.mu.Unlock()
			continue
		}
		var panFullPath, relDir string
		if HasPrefix(parent, panDir) {
			panFullPath = parent
			relDir = strings.TrimPrefix(parent, strings.TrimSuffix(panDir, "/")+"/")
		} else {
			panFullPath = strings.TrimRight(panDir, "/") + "/" + strings.TrimLeft(parent, "/")
			relDir = parent
		}
		parentID := GetDirIDByPath(ctx, g.client, panFullPath)
		if parentID == 0 {
			log.Printf("【监控整理STRM生成】网盘目录定位失败: %s", panFullPath)
			g.mu.Lock()
			result["fail"] += len(fnames)
			g.mu.Unlock()
			continue
		}

		items, err := g.client.IterDir(ctx, parentID, 1, true, 0)
		if err != nil {
			log.Printf("【监控整理STRM生成】遍历目录失败 %s: %v", panFullPath, err)
			g.mu.Lock()
			result["fail"] += len(fnames)
			g.mu.Unlock()
			continue
		}

		// 并发处理：semaphore 控制 goroutine 数量
		sem := make(chan struct{}, g.concurrency)
		var wg sync.WaitGroup
		matchedCount := 0

		for item := range items {
			if item.IsDir {
				continue
			}
			raw := item.Raw
			if raw == nil {
				continue
			}
			fname := raw.FileName
			if !containsStr(fnames, fname) {
				continue
			}
			ext := strings.ToLower(filepath.Ext(fname))
			matched := false
			for _, rmtExt := range g.rmtMediaext {
				if ext == rmtExt {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			matchedCount++
			stem := strings.TrimSuffix(fname, ext)
			newFilePath := filepath.Join(localDir, relDir, stem+".strm")

			sem <- struct{}{}
			wg.Add(1)
			go func(newFilePath string, raw *pan123.FileInfo) {
				defer func() {
					<-sem
					wg.Done()
				}()

				if _, err := os.Stat(newFilePath); err == nil && overwriteMode == "never" {
					g.mu.Lock()
					result["skip"]++
					g.mu.Unlock()
					return
				}
				dir := filepath.Dir(newFilePath)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					log.Printf("【监控整理STRM生成】创建目录失败 %s: %v", dir, err)
					g.mu.Lock()
					result["fail"]++
					g.mu.Unlock()
					return
				}
				s3KeyFlag := raw.S3KeyFlag
				if s3KeyFlag == "" {
					s3KeyFlag = raw.Etag
				}
				strmURL := BuildRedirectURL(
					g.serverAddress, g.apiKey,
					raw.FileName, raw.Size, raw.Etag, s3KeyFlag, raw.FileID,
				)
				if err := os.WriteFile(newFilePath, []byte(strmURL), 0o644); err != nil {
					log.Printf("【监控整理STRM生成】写入 %s 失败: %v", newFilePath, err)
					g.mu.Lock()
					result["fail"]++
					g.mu.Unlock()
					return
				}
				g.mu.Lock()
				result["success"]++
				g.mu.Unlock()
				log.Printf("【监控整理STRM生成】生成 STRM 文件成功: %s", newFilePath)
			}(newFilePath, raw)
		}
		wg.Wait()
		if matchedCount == 0 {
			log.Printf("【监控整理STRM生成】目录 %s 中未匹配到任何已整理文件名（可能重命名未生效或文件已删除）: %v", panFullPath, fnames)
		}
	}
	return result
}
