// 媒体删除统一模块（对应 media_delete.py：本地 STRM + 网盘回收站 + Emby 通知 + 整理历史）。
package strm

import (
	"context"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"mmbot/internal/pan123"
	"mmbot/internal/transfer"
)

// DeleteItem 待删除条目。
type DeleteItem struct {
	ID        string // 本地 STRM 路径（/delete、查重删除场景）
	Name      string // 显示名（日志用）
	Path      string // 本地 STRM 绝对路径
	PanPath   string // 网盘文件相对路径（覆盖整理场景，用于反推本地 STRM 路径）
	Size      int64  // 文件大小（网盘定位兜底用）
	PanFileID int64  // 网盘文件 ID（覆盖整理有精确 id，跳过网盘定位）
}

// DeleteMedia 媒体信息（按媒体清理整理历史）。
type DeleteMedia struct {
	Title string
	Year  string
	Kind  string
}

// FailItem 删除失败项。
type FailItem struct {
	Name  string
	Error string
}

// DeleteResult 删除结果。
type DeleteResult struct {
	Success      int
	FailList     []FailItem
	DeletedPaths []string
}

// JoinStrm 网盘相对路径段 -> 本地 STRM 路径（文件名去扩展名后补 .strm，与生成器一致）。
func JoinStrm(localDir string, relParts []string) string {
	if len(relParts) == 0 {
		return ""
	}
	last := relParts[len(relParts)-1]
	ext := filepath.Ext(last)
	stem := strings.TrimSuffix(last, ext)
	parts := append(append([]string{}, relParts[:len(relParts)-1]...), stem+".strm")
	joined := append([]string{localDir}, parts...)
	return filepath.Join(joined...)
}

// LocalStrmCandidates 网盘文件相对路径 -> 可能的本地 STRM 路径（与 STRM 生成器同语义，存在即删）。
func LocalStrmCandidates(panPath string, mappings [][2]string) []string {
	if panPath == "" {
		return nil
	}
	norm := strings.TrimSpace(strings.ReplaceAll(panPath, "\\", "/"))
	parts := splitPath(norm)
	if len(parts) == 0 {
		return nil
	}
	var candidates []string
	for _, mapping := range mappings {
		localDir, panDir := mapping[0], mapping[1]
		panParts := splitPath(strings.ReplaceAll(panDir, "\\", "/"))
		if len(panParts) > 0 && len(parts) >= len(panParts) && equalParts(parts[:len(panParts)], panParts) {
			candidates = append(candidates, JoinStrm(localDir, parts[len(panParts):]))
		} else if len(parts) > 0 {
			// 整理历史使用相对网盘根目录的路径（如“电影/xxx.mkv”）。
			candidates = append(candidates, JoinStrm(localDir, parts))
		}
	}
	if len(candidates) == 0 && !strings.HasPrefix(norm, "/") && len(mappings) > 0 {
		candidates = append(candidates, JoinStrm(mappings[0][0], parts))
	}
	var out []string
	for _, c := range candidates {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func equalParts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetDirIDByPath 根据网盘路径（如 /媒体库/电影）逐级定位目录 ID。
// 根目录 ID 为 0。失败返回 0。
func GetDirIDByPath(ctx context.Context, client *pan123.Client, p string) int64 {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	parts := splitPath(p)
	parentID := int64(0)
	for _, part := range parts {
		pid, err := findChildDir(ctx, client, parentID, part)
		if err != nil {
			log.Printf("[delete] 网盘路径定位失败: %s（在 %d 下找不到 %s）: %v", p, parentID, part, err)
			return 0
		}
		parentID = pid
	}
	return parentID
}

// findChildDir 在 parentID 目录下查找名为 name 的子目录，返回其 ID。
func findChildDir(ctx context.Context, client *pan123.Client, parentID int64, name string) (int64, error) {
	list, err := client.FSList(ctx, parentID)
	if err != nil {
		return 0, err
	}
	for _, f := range list {
		if f.Type == 1 && f.FileName == name {
			return f.FileID, nil
		}
	}
	return 0, errDirNotFound(name)
}

type dirNotFoundError struct{ name string }

func (e *dirNotFoundError) Error() string { return "目录不存在: " + e.name }

func errDirNotFound(name string) error { return &dirNotFoundError{name: name} }

// FindPanFileID 本地 STRM 路径 → 映射转网盘路径 → 定位目录 → 按 stem+size 匹配文件 id。
// dirIDCache / listingCache 批内缓存复用，避免同目录多文件重复 API 调用。
func FindPanFileID(ctx context.Context, client *pan123.Client, strmPath string, size int64, mappings [][2]string, dirIDCache, listingCache map[string]any) int64 {
	norm := strings.ReplaceAll(strmPath, "\\", "/")
	normParts := splitPath(norm)
	if len(normParts) == 0 {
		return 0
	}
	base := path.Base(norm)
	stem := strings.TrimSuffix(base, path.Ext(base))
	for _, mapping := range mappings {
		localDir, panDir := mapping[0], mapping[1]
		local := strings.TrimSuffix(strings.ReplaceAll(localDir, "\\", "/"), "/")
		localParts := splitPath(local)
		if len(localParts) == 0 || !equalParts(normParts[:min(len(localParts), len(normParts))], localParts) {
			continue
		}
		pan := strings.TrimSuffix(strings.ReplaceAll(panDir, "\\", "/"), "/")
		relParts := normParts[len(localParts):]
		panFull := strings.Join(append([]string{pan}, relParts...), "/")
		panParent := path.Dir(panFull)

		// 1. 目录 ID：批内缓存
		var parentID int64
		if v, ok := dirIDCache[panParent]; ok {
			parentID = toInt64Any(v)
		} else {
			parentID = GetDirIDByPath(ctx, client, panParent)
			if parentID != 0 {
				dirIDCache[panParent] = parentID
			}
		}
		if parentID == 0 {
			log.Printf("[delete] 网盘目录定位失败: %s（%s）", panParent, strmPath)
			continue
		}

		// 2. 目录列举：批内缓存
		rawListing, ok := listingCache[panParent]
		var listing map[string][][2]any
		if !ok {
			listing = map[string][][2]any{}
			listingCache[panParent] = listing
			list, err := client.FSList(ctx, parentID)
			if err != nil {
				log.Printf("[delete] 遍历网盘目录失败 %s: %v", panParent, err)
				continue
			}
			for _, item := range list {
				if item.Type == 1 {
					continue
				}
				itemStem := strings.TrimSuffix(item.FileName, path.Ext(item.FileName))
				listing[itemStem] = append(listing[itemStem], [2]any{item.FileID, item.Size})
			}
		} else {
			listing = rawListing.(map[string][][2]any)
		}
		// 3. 匹配：优先 size 精确匹配；无 size 时取第一个
		candidates, ok := listing[stem]
		if ok {
			if size > 0 {
				for _, cand := range candidates {
					if toInt64Any(cand[1]) == size {
						return toInt64Any(cand[0])
					}
				}
			} else {
				return toInt64Any(candidates[0][0])
			}
		}
	}
	return 0
}

func toInt64Any(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// CleanEmptyDirs 从 STRM 所在目录向上清理空目录（Season 层/剧目录/电影目录），到非空为止。
func CleanEmptyDirs(strmPath string) {
	d := filepath.Dir(strmPath)
	for d != "" && d != "." && d != "/" {
		entries, err := os.ReadDir(d)
		if err != nil {
			break
		}
		if len(entries) > 0 {
			break
		}
		if err := os.Remove(d); err != nil {
			break
		}
		d = filepath.Dir(d)
	}
}

// DeleteItems 四件套删除：本地 STRM + 网盘回收站（移入回收站）+ Emby 通知 + 整理历史。
// 返回 {success, fail_list, deleted_paths}。
func DeleteItems(ctx context.Context, client *pan123.Client, items []DeleteItem, media *DeleteMedia, emby *EmbyClient, strmPaths string, history *transfer.TransferHistory) DeleteResult {
	var result DeleteResult
	mappings := ParseMappings(strmPaths)
	dirIDCache := map[string]any{}
	listingCache := map[string]any{}
	var deletedFileIDs []string

	for _, item := range items {
		name := item.Name
		if name == "" {
			name = item.ID
		}
		localPath := item.Path
		panPath := item.PanPath
		panFileID := item.PanFileID
		// 编目 orphan 记录的 target_path 存的是本地 STRM 路径而非网盘路径（见 catalog.go 的 buildHistoryRecord）
		if localPath == "" && strings.HasSuffix(strings.ToLower(strings.ReplaceAll(panPath, "\\", "/")), ".strm") {
			localPath, panPath = panPath, ""
		}
		tryDelete := func() error {
			// 1. 本地 STRM：直接路径 + 从网盘路径反推的候选路径（覆盖整理场景），存在即删
			var strmPathsDel []string
			if localPath != "" {
				strmPathsDel = append(strmPathsDel, localPath)
			}
			strmPathsDel = append(strmPathsDel, LocalStrmCandidates(panPath, mappings)...)
			seen := map[string]bool{}
			for _, sp := range strmPathsDel {
				if sp == "" || seen[sp] {
					continue
				}
				seen[sp] = true
				if _, err := os.Stat(sp); err == nil {
					if err := os.Remove(sp); err != nil {
						return err
					}
					result.DeletedPaths = append(result.DeletedPaths, sp)
					log.Printf("[delete] 已删除本地 STRM: %s", sp)
					CleanEmptyDirs(sp)
				}
			}
			// 2. 网盘源文件（移入回收站）
			if client != nil {
				log.Printf("[delete] 开始处理网盘源文件删除: %s (file_id=%d)", name, panFileID)
				targetID := panFileID
				if targetID == 0 && strings.HasSuffix(strings.ToLower(localPath), ".strm") {
					targetID = FindPanFileID(ctx, client, localPath, item.Size, mappings, dirIDCache, listingCache)
				}
				if targetID != 0 {
					if ok, err := client.TrashFile(ctx, targetID); ok {
						log.Printf("[delete] 已删除网盘源文件: file_id=%d（%s）", targetID, name)
						deletedFileIDs = append(deletedFileIDs, int64ToString(targetID))
					} else if err != nil {
						log.Printf("[delete] 删除网盘源文件失败: %s, 原因: %v", name, err)
					} else {
						log.Printf("[delete] 删除网盘源文件未生效（已手动删除/已在回收站/无效id）: %s", name)
					}
				} else {
					log.Printf("[delete] 未定位到网盘源文件: %s", name)
				}
			} else {
				log.Printf("[delete] 123客户端未初始化，跳过网盘源文件删除: %s", name)
			}
			return nil
		}
		if err := tryDelete(); err != nil {
			log.Printf("[delete] 删除失败 %s: %v", name, err)
			result.FailList = append(result.FailList, FailItem{Name: name, Error: err.Error()})
			continue
		}
		result.Success++
	}

	// 3. 整理历史：按实际删除的 file_id 精准清理 + 整部剧删除时按媒体清理
	if history != nil {
		for _, fid := range deletedFileIDs {
			history.DeleteByFileID(fid)
		}
		for _, item := range items {
			if item.ID != "" {
				history.DeleteByFileID(item.ID)
			}
			if item.PanFileID != 0 {
				history.DeleteByFileID(int64ToString(item.PanFileID))
			}
		}
		if media != nil && media.Title != "" {
			history.DeleteByMedia(media.Title, media.Year, media.Kind)
		}
	}

	// 4. 通知 Emby 清理条目（未配置则跳过，条目随下次扫描自动清除）
	if emby != nil && len(result.DeletedPaths) > 0 {
		if emby.NotifyMediaDeleted(result.DeletedPaths) {
			log.Printf("[delete] 已通知 Emby 清理 %d 个条目", len(result.DeletedPaths))
		} else {
			log.Printf("[delete] 通知 Emby 清理失败（条目将在下次库扫描时自动清除）")
		}
	}

	return result
}

func int64ToString(v int64) string {
	if v == 0 {
		return ""
	}
	// 直接 strconv 格式化
	return itoa(v)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
