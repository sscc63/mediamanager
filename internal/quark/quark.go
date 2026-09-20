// Package quark 实现夸克网盘分享链接 SDK（对应 Python quark.py）。
package quark

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"time"

	"mmbot/internal/httpx"
)

// QuarkUA 夸克客户端 UA（浏览器 UA 会被 file/download 拒绝，取不到 md5）。
const QuarkUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"

// Client 夸克网盘 SDK。
type Client struct {
	Cookie  string
	PR      string
	FR      string
	Timeout time.Duration
	Debug   bool
	http    *httpx.Client
}

// New 创建夸克客户端。
func New(cookie string) *Client {
	return &Client{
		Cookie: cookie,
		PR:     "ucpro",
		FR:     "pc",
		http:   httpx.New(15 * time.Second),
		Debug:  true,
	}
}

// ShareFile 分享文件条目。
type ShareFile struct {
	FID           string `json:"fid"`
	FileName      string `json:"file_name"`
	Size          int64  `json:"size"`
	Dir           bool   `json:"dir"`
	Md5           string `json:"md5"`
	ShareFIDToken string `json:"share_fid_token"`
	Path          string `json:"-"`
}

// ShareFileResp 分享文件列表响应。
type ShareFileResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List []*ShareFile `json:"list"`
	} `json:"data"`
	Metadata struct {
		Page  int `json:"_page"`
		Size  int `json:"_size"`
		Total int `json:"_total"`
	} `json:"metadata"`
}

func (c *Client) headers() map[string]string {
	h := map[string]string{
		"cookie":          c.Cookie,
		"content-type":    "application/json",
		"user-agent":      QuarkUA,
		"accept":          "application/json, text/plain, */*",
		"accept-language": "zh-CN,zh;q=0.9,en;q=0.8",
		"referer":         "https://pan.quark.cn/",
		"origin":          "https://pan.quark.cn",
	}
	return h
}

func baseParams() map[string]string {
	return map[string]string{
		"pr": "ucpro",
		"fr": "pc",
	}
}

// ShareFileList 获取分享文件列表（单页）。
func (c *Client) ShareFileList(ctx context.Context, code, passcode, stoken, dirID string, page int, pageSize int) (*ShareFileResp, error) {
	if pageSize <= 0 {
		pageSize = 1000
	}
	params := baseParams()
	params["pwd_id"] = code
	params["passcode"] = passcode
	params["stoken"] = stoken
	params["pdir_fid"] = dirID
	params["force"] = "0"
	params["_page"] = strconv.Itoa(page)
	params["_size"] = strconv.Itoa(pageSize)
	params["_fetch_banner"] = "0"
	params["_fetch_share"] = "0"
	params["_fetch_total"] = "1"
	params["_sort"] = "file_type:asc,updated_at:desc"
	params["__dt"] = strconv.Itoa(int(rand.Float64()*4*60000 + 60000))
	params["__t"] = strconv.FormatInt(time.Now().Unix(), 10)

	raw, status, err := c.http.Get(ctx, "https://pc-api.uc.cn/1/clouddrive/share/sharepage/detail?"+encodeParams(params), c.headers())
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("夸克 share_file_list HTTP %d: %s", status, string(raw))
	}
	var resp ShareFileResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return &resp, fmt.Errorf("夸克 share_file_list code=%d msg=%s", resp.Code, resp.Message)
	}
	return &resp, nil
}

// ShareInfo 获取分享信息（stoken）。
type ShareInfo struct {
	Stoken string `json:"stoken"`
}

// GetShareInfo 获取分享 token。
func (c *Client) GetShareInfo(ctx context.Context, shareID, password string) (string, error) {
	payload := map[string]string{"pwd_id": shareID, "passcode": password}
	raw, status, err := c.http.PostJSON(ctx, "https://pc-api.uc.cn/1/clouddrive/share/sharepage/token?"+encodeParams(baseParams()), c.headers(), payload)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("夸克 get_share_info HTTP %d: %s", status, string(raw))
	}
	var resp struct {
		Code int       `json:"code"`
		Msg  string    `json:"message"`
		Data ShareInfo `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.Code != 0 {
		return "", fmt.Errorf("夸克 get_share_info code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.Data.Stoken, nil
}

// WalkShareFiles 递归遍历分享中的全部文件。
// cb 在遍历到每个文件（含目录）时被调用。
func (c *Client) WalkShareFiles(ctx context.Context, code, passcode, stoken, dirID, parentPath string, cb func(f *ShareFile) error) error {
	page := 1
	for {
		resp, err := c.ShareFileList(ctx, code, passcode, stoken, dirID, page, 1000)
		if err != nil {
			return err
		}
		for _, f := range resp.Data.List {
			f.Path = parentPath + "/" + f.FileName
			if f.Dir {
				if err := c.WalkShareFiles(ctx, code, passcode, stoken, f.FID, f.Path, cb); err != nil {
					return err
				}
			}
			if err := cb(f); err != nil {
				return err
			}
		}
		md := resp.Metadata
		if md.Total > page*md.Size && len(resp.Data.List) > 0 {
			page++
			continue
		}
		break
	}
	return nil
}

// FileDownloadInfo 单个文件下载信息。
type FileDownloadInfo struct {
	Md5 string `json:"md5"`
}

// createDownloadRequest 批量创建下载请求。
func (c *Client) createDownloadRequest(ctx context.Context, code, pwd, stoken string, fids, fidsTokens []string) (map[string]any, error) {
	payload := map[string]any{
		"fids":       fids,
		"pwd_id":     code,
		"stoken":     stoken,
		"fids_token": fidsTokens,
	}
	if pwd != "" {
		payload["passcode"] = pwd
	}
	params := map[string]string{"entry": "ft", "uc_param_str": ""}
	h := c.headers()
	h["referer"] = "https://fast.uc.cn/"
	raw, status, err := c.http.PostJSON(ctx, "https://pc-api.uc.cn/1/clouddrive/file/download?"+encodeParams(params), h, payload)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("夸克 download HTTP %d: %s", status, string(raw))
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetFileDownloadInfo 获取单个分享文件下载信息。
func (c *Client) GetFileDownloadInfo(ctx context.Context, code, pwd, stoken, fid, fidsToken string) *FileDownloadInfo {
	resp, err := c.createDownloadRequest(ctx, code, pwd, stoken, []string{fid}, []string{fidsToken})
	if err != nil || resp == nil {
		return &FileDownloadInfo{}
	}
	data, ok := resp["data"].([]any)
	if !ok || len(data) == 0 {
		return &FileDownloadInfo{}
	}
	info, _ := data[0].(map[string]any)
	md5, _ := info["md5"].(string)
	return &FileDownloadInfo{Md5: md5}
}

// BatchGetFileDownloadInfo 批量获取下载信息，以 fid 为 key。
func (c *Client) BatchGetFileDownloadInfo(ctx context.Context, code, pwd, stoken string, fids, fidsTokens []string) map[string]*FileDownloadInfo {
	results := map[string]*FileDownloadInfo{}
	batchSize := 10
	for i := 0; i < len(fids); i += batchSize {
		end := i + batchSize
		if end > len(fids) {
			end = len(fids)
		}
		batchFids := fids[i:end]
		batchTokens := fidsTokens[i:end]
		resp, err := c.createDownloadRequest(ctx, code, pwd, stoken, batchFids, batchTokens)
		if err != nil {
			if c.Debug {
				log.Printf("[夸克] 批量下载请求失败: %v", err)
			}
			for _, fid := range batchFids {
				results[fid] = &FileDownloadInfo{}
			}
			continue
		}
		data, ok := resp["data"].([]any)
		if !ok {
			for _, fid := range batchFids {
				results[fid] = &FileDownloadInfo{}
			}
			continue
		}
		for idx, item := range data {
			if idx >= len(batchFids) {
				break
			}
			m, _ := item.(map[string]any)
			md5, _ := m["md5"].(string)
			results[batchFids[idx]] = &FileDownloadInfo{Md5: md5}
		}
	}
	return results
}

func encodeParams(m map[string]string) string {
	first := true
	var b []byte
	for k, v := range m {
		if !first {
			b = append(b, '&')
		}
		first = false
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, v...)
	}
	return string(b)
}
