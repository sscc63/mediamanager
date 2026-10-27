// STRM 302 跳转服务（对应 redirect.py）。
package strm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"mmbot/internal/pan123"
)

// 302 缓存：key=(name,size,md5,s3KeyFlag) -> (url, timestamp)
// 有界 LRU 缓存，TTL 2 分钟，最多 1000 条。
type redirectCacheEntry struct {
	url       string
	timestamp time.Time
}

var (
	redirectCache   = map[string]redirectCacheEntry{}
	redirectCacheMu sync.Mutex
	redirectOrder   []string
)

const (
	redirectCacheTTL     = 2 * time.Minute
	redirectCacheMaxSize = 1000
)

func redirectCacheKey(name string, size int64, md5, s3KeyFlag string) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", name, size, md5, s3KeyFlag)
}

func redirectCacheGet(key string) (string, bool) {
	redirectCacheMu.Lock()
	defer redirectCacheMu.Unlock()
	entry, ok := redirectCache[key]
	if !ok {
		return "", false
	}
	if time.Since(entry.timestamp) > redirectCacheTTL {
		delete(redirectCache, key)
		removeOrder(key)
		return "", false
	}
	moveToEnd(key)
	return entry.url, true
}

func redirectCacheSet(key, url string) {
	redirectCacheMu.Lock()
	defer redirectCacheMu.Unlock()
	redirectCache[key] = redirectCacheEntry{url: url, timestamp: time.Now()}
	moveToEnd(key)
	for len(redirectCache) > redirectCacheMaxSize {
		if len(redirectOrder) > 0 {
			oldest := redirectOrder[0]
			delete(redirectCache, oldest)
			redirectOrder = redirectOrder[1:]
		} else {
			break
		}
	}
}

func moveToEnd(key string) {
	for i, k := range redirectOrder {
		if k == key {
			redirectOrder = append(redirectOrder[:i], redirectOrder[i+1:]...)
			break
		}
	}
	redirectOrder = append(redirectOrder, key)
}

func removeOrder(key string) {
	for i, k := range redirectOrder {
		if k == key {
			redirectOrder = append(redirectOrder[:i], redirectOrder[i+1:]...)
			break
		}
	}
}

// BuildRedirectURL 构造 STRM 文件内容（302 跳转 URL）。
func BuildRedirectURL(serverAddress, apiKey, name string, size int64, etag, s3KeyFlag string) string {
	serverAddress = strings.TrimRight(serverAddress, "/")
	return fmt.Sprintf("%s/api/strm/redirect?apikey=%s&name=%s&size=%d&md5=%s&s3_key_flag=%s",
		serverAddress,
		url.QueryEscape(apiKey),
		url.QueryEscape(name),
		size,
		url.QueryEscape(etag),
		url.QueryEscape(s3KeyFlag),
	)
}

// ensureFileID 用 MD5 秒传把文件引到网盘根目录，返回真实 FileID。
// 注意：真实 id 在 data.Info.FileId，顶层 data.FileId 是上传会话 id，用它取链会 5066。
func ensureFileID(ctx context.Context, client *pan123.Client, size int64, md5 string) (int64, error) {
	// 临时名带 md5 前 8 位：不同影片互不覆盖，同一影片重复播则替换同名。
	resp, err := client.UploadRequest(ctx, map[string]any{
		"etag": md5, "fileName": "_strm_" + md5[:8] + ".mkv",
		"size": size, "parentFileId": 0, "duplicate": 2,
	})
	if err != nil {
		return 0, err
	}
	var data struct {
		Info struct {
			FileID int64 `json:"FileId"`
		} `json:"Info"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return 0, err
	}
	if data.Info.FileID == 0 {
		return 0, fmt.Errorf("秒传失败 code=%d msg=%s", resp.Code, resp.MessageString())
	}
	return data.Info.FileID, nil
}

// GetRedirectURL 获取 302 跳转目标地址（严格模式：s3_key_flag 必须非空）。
func GetRedirectURL(ctx context.Context, client *pan123.Client, name string, size int64, md5, s3KeyFlag, userAgent string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("123 客户端未初始化（token 无效或网络故障），无法获取下载链接")
	}
	if s3KeyFlag == "" || len(md5) < 8 {
		return "", fmt.Errorf("STRM 参数缺失（s3_key_flag/md5）")
	}
	key := redirectCacheKey(name, size, md5, s3KeyFlag)
	if url, ok := redirectCacheGet(key); ok {
		return url, nil
	}

	// 秒传拿真实 FileID；直链签发后与文件解耦，取完即可删（失败也无妨：同名会被下次覆盖）。
	fileID, err := ensureFileID(ctx, client, size, md5)
	if err != nil {
		return "", err
	}
	defer func() { go func() { _, _ = client.TrashFile(context.Background(), fileID) }() }()

	payload := map[string]any{
		"S3KeyFlag": s3KeyFlag,
		"FileName":  name,
		"Etag":      md5,
		"Size":      size,
		"FileID":    fileID,
	}
	headers := map[string]string{}
	if userAgent != "" {
		headers["User-Agent"] = userAgent
	}
	resp, err := client.DownloadInfoWithPayload(ctx, payload, headers)
	if err != nil {
		return "", fmt.Errorf("获取 123 下载地址失败: %v", err)
	}
	if !resp.IsSuccess() {
		return "", fmt.Errorf("123 返回错误 code=%d msg=%s", resp.Code, resp.MessageString())
	}
	var data struct {
		DownloadURL string `json:"DownloadUrl"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", fmt.Errorf("解析下载地址失败: %v", err)
	}
	if data.DownloadURL == "" {
		return "", fmt.Errorf("123 返回的 DownloadUrl 为空")
	}

	redirectCacheSet(key, data.DownloadURL)
	log.Printf("【302】%s 成功", name)
	return data.DownloadURL, nil
}

// CheckResponse 检查 123 响应。
func CheckResponse(resp *pan123.Response) error {
	if !resp.IsSuccess() {
		return fmt.Errorf("123 API 错误 code=%d msg=%s", resp.Code, resp.MessageString())
	}
	return nil
}
