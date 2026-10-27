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
	"sync/atomic"
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

// dlAttempt 一次取链尝试。headers 覆盖 pan123 请求的默认头；passUA 为 true 时把播放器 UA 透传给 123。
// fileID/noFileID 控制 payload 里的 FileID 字段：历史经验是 FileID=0 取不到、FileID=1 能取到，
// 说明该字段是「语义开关」而非真实文件 id，故这里按取值逐档试。
type dlAttempt struct {
	name     string
	headers  map[string]string
	passUA   bool
	fileID   any
	noFileID bool
}

// redirectAttempts 取链尝试序列。
// 2026-10-27 12:16 起 123 收紧校验，原「FileID=1 + platform=android + 播放器 UA」组合被统一回
// 5066「文件不存在」（新旧文件、网盘有无文件均复现）。故按「最接近原行为」到「最标准」顺序逐个降级：
// 先试 FileID 的三种形态（1 / 不传 / 0），再叠加 Referer。任一组合成功即返回。
// 日志会记录每个组合的真实 code/msg，用于定位 123 到底认哪一套。
var redirectAttempts = []dlAttempt{
	{name: "legacy(FileID=1+android+播放器UA)", headers: map[string]string{"platform": "android"}, passUA: true, fileID: 1},
	{name: "noFileID(open_platform)", headers: map[string]string{"platform": "open_platform"}, noFileID: true},
	{name: "FileID=1(open_platform)", headers: map[string]string{"platform": "open_platform"}, fileID: 1},
	{name: "noFileID+referer", headers: map[string]string{"platform": "open_platform", "Referer": "https://www.123pan.com/"}, noFileID: true},
	{name: "FileID=1+referer", headers: map[string]string{"platform": "open_platform", "Referer": "https://www.123pan.com/"}, fileID: 1},
	{name: "FileID=0(open_platform)", headers: map[string]string{"platform": "open_platform"}, fileID: 0},
}

// goodAttempt 记住上次成功的组合下标+1（0 表示未知），后续优先试它，避免每次播放都逐个重试。
var goodAttempt atomic.Int32

// attemptOrder 返回尝试顺序：上次成功的组合优先，其余按定义顺序。
func attemptOrder() []int {
	g := int(goodAttempt.Load()) - 1
	order := make([]int, 0, len(redirectAttempts))
	if g >= 0 && g < len(redirectAttempts) {
		order = append(order, g)
	}
	for i := range redirectAttempts {
		if i != g {
			order = append(order, i)
		}
	}
	return order
}

// GetRedirectURL 获取 302 跳转目标地址（严格模式：s3_key_flag 必须非空）。
func GetRedirectURL(ctx context.Context, client *pan123.Client, name string, size int64, md5, s3KeyFlag, userAgent string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("123 客户端未初始化（token 无效或网络故障），无法获取下载链接")
	}
	if s3KeyFlag == "" {
		return "", fmt.Errorf("缺少 s3_key_flag 参数，无法获取下载链接")
	}
	key := redirectCacheKey(name, size, md5, s3KeyFlag)
	if url, ok := redirectCacheGet(key); ok {
		return url, nil
	}

	// 逐个组合尝试：单一组合可能被 123 拒绝，任一成功即缓存并返回。
	lastCode, lastMsg := 0, ""
	var lastErr error
	for _, idx := range attemptOrder() {
		att := redirectAttempts[idx]
		// 每个组合独立构造 payload：FileID 的取值本身就是被试探的变量，不能共用同一个 map。
		payload := map[string]any{
			"S3KeyFlag": s3KeyFlag,
			"FileName":  name,
			"Etag":      md5,
			"Size":      size,
		}
		if !att.noFileID && att.fileID != nil {
			payload["FileID"] = att.fileID
		}
		headers := make(map[string]string, len(att.headers)+1)
		for k, v := range att.headers {
			headers[k] = v
		}
		// legacy 组合复现原行为：透传播放器 UA。用小写 key，避免与 req() 默认的 user-agent 重复导致取值随机。
		if att.passUA && userAgent != "" {
			headers["user-agent"] = userAgent
		}
		resp, err := client.DownloadInfoWithPayload(ctx, payload, headers)
		if err != nil {
			lastErr = err
			log.Printf("【302跳转服务】取链尝试[%s]出错 name=%s: %v", att.name, name, err)
			continue
		}
		if !resp.IsSuccess() {
			lastCode, lastMsg = resp.Code, resp.MessageString()
			log.Printf("【302跳转服务】取链尝试[%s]被拒 name=%s: code=%d msg=%s", att.name, name, resp.Code, resp.MessageString())
			continue
		}
		var data struct {
			DownloadURL string `json:"DownloadUrl"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			log.Printf("【302跳转服务】取链尝试[%s]响应解析失败 name=%s: %v", att.name, name, err)
			continue
		}
		if data.DownloadURL == "" {
			log.Printf("【302跳转服务】取链尝试[%s]返回成功但 DownloadUrl 为空 name=%s", att.name, name)
			continue
		}
		goodAttempt.Store(int32(idx + 1))
		redirectCacheSet(key, data.DownloadURL)
		log.Printf("【302跳转服务】获取 123 下载地址成功（组合 %s）: %s", att.name, data.DownloadURL)
		return data.DownloadURL, nil
	}

	if lastCode == 0 && lastErr != nil {
		return "", fmt.Errorf("获取 123 下载地址失败: %v", lastErr)
	}
	return "", fmt.Errorf("123 返回错误 code=%d msg=%s（已尝试 %d 种 FileID/请求头组合）", lastCode, lastMsg, len(redirectAttempts))
}

// CheckResponse 检查 123 响应。
func CheckResponse(resp *pan123.Response) error {
	if !resp.IsSuccess() {
		return fmt.Errorf("123 API 错误 code=%d msg=%s", resp.Code, resp.MessageString())
	}
	return nil
}
