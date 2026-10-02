// Package pan123 实现 123 云盘客户端。
//
// 认证：默认使用 Web 账号密码登录（POST /api/user/sign_in 获取 access_token）；
// OAuth/115秒传123 使用开放平台 access_token（POST /api/v1/access_token）。
// 除 OAuth/115秒传外，所有接口一律使用 yun.123pan.com Web 接口。
package pan123

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/httpx"
)

const (
	OpenBase  = "https://open-api.123pan.com"
	YunBase   = "https://yun.123pan.com"
	LoginBase = "https://login.123pan.com"
)

// Client 123 云盘客户端。
type Client struct {
	Passport     string // 手机号/邮箱（web 登录）
	Password     string // 密码（web 登录）
	ClientID     string // 开放平台 appId（OAuth/115 秒传使用）
	ClientSecret string // 开放平台 secretId
	Token        string // 当前 access_token
	TokenFile    string // token 持久化文件（可选）

	mu   sync.Mutex
	http *httpx.Client

	// 进程级请求节流：所有请求统一在此限速，从源头避免触发 123 风控（code=100011"请勿频繁操作"）。
	throttleMu   sync.Mutex
	lastReqAt    time.Time
	minReqGap    time.Duration

	uidMu    sync.Mutex
	uidCache map[string]int64 // shareKey → 上传者 UID（share.123pan.com 三级域名）
}

// New 用已有 token 创建客户端。
func New(token string) *Client {
	return &Client{Token: token, http: httpx.New(30 * time.Second), minReqGap: defaultMinReqGap}
}

// NewWithSecret 用手机号+密码（web 登录）或 client_id+client_secret（开放平台）创建客户端并获取 token。
// 优先使用 web 登录（手机号+密码），若凭证长度 >= 32 且为 hex 格式则使用开放平台登录。
func NewWithSecret(clientID, clientSecret string) (*Client, error) {
	c := &Client{http: httpx.New(30 * time.Second), minReqGap: defaultMinReqGap}
	// 判断是否为开放平台凭据（client_id >= 32 位 hex 字符串）
	if len(clientID) >= 32 && isHex(clientID) && len(clientSecret) >= 32 && isHex(clientSecret) {
		c.ClientID = clientID
		c.ClientSecret = clientSecret
		if err := c.LoginToken(context.Background()); err != nil {
			return nil, err
		}
		return c, nil
	}
	// 默认使用 web 登录（手机号/邮箱 + 密码）
	c.Passport = clientID
	c.Password = clientSecret
	if err := c.LoginPassport(context.Background()); err != nil {
		return nil, err
	}
	return c, nil
}

// isHex 判断字符串是否仅为 hex 字符。
func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// APIError 123 接口错误。
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("123API错误%d: %s", e.Code, e.Message)
}

// isAuthMsg 判断是否为鉴权失败。
func isAuthMsg(code int, msg string) bool {
	s := strings.ToLower(msg)
	return code == 401 || strings.Contains(s, "token is expired") || strings.Contains(s, "access token invalid")
}

// IsAuthError 判断鉴权失败。
func (e *APIError) IsAuthError() bool { return isAuthMsg(e.Code, e.Message) }

// Response 123 统一响应结构。
type Response struct {
	Code    int             `json:"code"`
	Message json.RawMessage `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// UnmarshalJSON 兼容 123 风控返回的字符串 code（如 {"code":"429","message":"分享界面操作频繁，请稍候再试"}）。
func (r *Response) UnmarshalJSON(b []byte) error {
	var raw struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	r.Code = codeAsInt(raw.Code)
	r.Message = raw.Message
	r.Data = raw.Data
	return nil
}

// codeAsInt 把 code 字段（数字或数字字符串）解析为 int，解析不出时返回 0。
func codeAsInt(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, _ = strconv.Atoi(s)
		return n
	}
	return 0
}

func (r *Response) MessageString() string {
	if len(r.Message) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Message, &s); err == nil {
		return s
	}
	return string(r.Message)
}

// IsSuccess 判断 123 接口是否成功（code=0 或 code=200 均视为成功）。
func (r *Response) IsSuccess() bool { return r.Code == 0 || r.Code == 200 }

// IsAuthError 判断鉴权失败。
func (r *Response) IsAuthError() bool { return isAuthMsg(r.Code, r.MessageString()) }

// ---------- 登录 ----------

// LoginPassport 通过 Web 账号密码登录（POST /api/user/sign_in）。
// 对应 Python p123client.login_passport()。
func (c *Client) LoginPassport(ctx context.Context) error {
	raw, status, err := c.http.PostJSON(ctx, LoginBase+"/api/user/sign_in",
		nil,
		map[string]any{"passport": c.Passport, "password": c.Password, "remember": true})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("123 web登录 HTTP %d: %s", status, string(raw))
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if (resp.Code != 0 && resp.Code != 200) || resp.Data.Token == "" {
		return fmt.Errorf("123 web登录失败 code=%d msg=%s", resp.Code, resp.Message)
	}
	c.Token = resp.Data.Token
	return nil
}

// LoginToken 通过开放平台 clientID/clientSecret 获取 access_token（OAuth/115秒传使用）。
func (c *Client) LoginToken(ctx context.Context) error {
	raw, status, err := c.http.PostJSON(ctx, OpenBase+"/api/v1/access_token",
		map[string]string{"platform": "open_platform"},
		map[string]any{"clientID": c.ClientID, "clientSecret": c.ClientSecret})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("获取123 token HTTP %d: %s", status, string(raw))
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if (resp.Code != 0 && resp.Code != 200) || resp.Data.AccessToken == "" {
		return fmt.Errorf("获取123 token失败 code=%d msg=%s", resp.Code, resp.Message)
	}
	c.Token = resp.Data.AccessToken
	return nil
}

// ---------- 通用请求 ----------

// defaultMinReqGap 进程级最小请求间隔。123 风控（code=100011）在低频限速下即消失。
// ponytail: 固定 1s 即可满足全量扫描/联动的并发量级；若末尾并发仍偏大再调大可配置。
const defaultMinReqGap = time.Second

// throttle 保证任意时刻距上次实际请求至少 minReqGap，超出则等待。
func (c *Client) throttle(ctx context.Context) {
	if c.minReqGap <= 0 {
		return
	}
	for {
		c.throttleMu.Lock()
		wait := c.minReqGap - time.Since(c.lastReqAt)
		if wait <= 0 {
			c.lastReqAt = time.Now()
			c.throttleMu.Unlock()
			return
		}
		c.throttleMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// req 发起请求，URL 为完整路径。自动带 Authorization: Bearer <token>。
func (c *Client) req(ctx context.Context, method, url string, headers map[string]string, params map[string]string, body any) (*Response, error) {
	c.throttle(ctx) // 进程级限速，从源头避免触发 123 风控
	h := map[string]string{
		"platform":    "open_platform",
		"app-version": "3",
		"user-agent":  "Mozilla/5.0 AppleWebKit/600 Safari/600 Chrome/124.0.0.0 Edg/124.0.0.0",
	}
	for k, v := range headers {
		h[k] = v
	}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	var raw []byte
	var status int
	var err error
	if body != nil {
		if params != nil {
			url = url + "?" + encodeQuery(params)
		}
		raw, status, err = c.http.PostJSON(ctx, url, h, body)
	} else {
		raw, status, err = c.http.Get(ctx, url+"?"+encodeQuery(params), h)
	}
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("123 HTTP %d: %s", status, string(raw))
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("123 解析响应失败: %v: %s", err, string(raw))
	}
	return &resp, nil
}

// reqAuto 与 req 相同，但遇到鉴权错误自动重新获取 token 并重试一次；
// 遇到风控限流（code=100011"请勿频繁操作"）自动退避重试最多 limitBackoffTimes 次。
// 所有走 reqAuto 的接口（FSList/FSDetail/FSMkdir 等）统一在此处获得限流容错，避免
// 目录定位/整理/联动 STRM 因风控集体失败。
// ponytail: 固定最大次数+线性退避，够用即可；若 123 限流加剧再考虑指数退避。
const (
	rateLimitCode       = 100011
	limitBackoffTimes   = 3
	limitBackoffSeconds = 3
)

func (c *Client) reqAuto(ctx context.Context, method, url string, headers map[string]string, params map[string]string, body any) (*Response, error) {
	var resp *Response
	var err error
	// 先做一次尝试；限流时退避重试
	for attempt := 0; attempt <= limitBackoffTimes; attempt++ {
		resp, err = c.req(ctx, method, url, headers, params, body)
		if err != nil {
			return nil, err
		}
		if resp.Code != rateLimitCode {
			break
		}
		if attempt < limitBackoffTimes {
			delay := time.Duration(attempt+1) * limitBackoffSeconds * time.Second
			log.Printf("[123] 频繁操作被限流(code=100011)，%v 后重试（第 %d/%d 次）", delay, attempt+1, limitBackoffTimes)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	if resp.IsAuthError() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// 优先使用 web 登录重试
		if c.Passport != "" && c.Password != "" {
			log.Printf("[123] token 过期，重新获取（web 登录）")
			if err := c.LoginPassport(ctx); err == nil {
				return c.req(ctx, method, url, headers, params, body)
			}
		} else if c.ClientID != "" && c.ClientSecret != "" {
			log.Printf("[123] token 过期，重新获取（open API）")
			if err := c.LoginToken(ctx); err == nil {
				return c.req(ctx, method, url, headers, params, body)
			}
		}
	}
	return resp, nil
}

// UserInfo 获取用户信息（开放平台 API），验证 token 有效性。
// 原始 Python 代码使用开放平台接口，Web 接口无对应端点。
func (c *Client) UserInfo(ctx context.Context) (*Response, error) {
	return c.reqAuto(ctx, "GET", OpenBase+"/api/v1/user/info", nil, nil, nil)
}

// ---------- 文件操作（Web 接口） ----------

// FileInfo 123 文件条目。
type FileInfo struct {
	FileID       int64  `json:"fileId"`
	FileName     string `json:"fileName"`
	Size         int64  `json:"size"`
	Type         int    `json:"type"` // 1=目录
	ParentFileID int64  `json:"parentFileId"`
	Etag         string `json:"etag"`
	S3KeyFlag    string `json:"S3KeyFlag"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
	Trashed      bool   `json:"trashed"`
}

// FSList 获取目录文件列表（yun Web 接口）。
func (c *Client) FSList(ctx context.Context, parentFileID any) ([]FileInfo, error) {
	params := map[string]string{
		"parentFileId":         toStr(parentFileID),
		"limit":                "100",
		"driveId":              "0",
		"Page":                 "1",
		"orderBy":              "file_id",
		"orderDirection":       "asc",
		"trashed":              "false",
		"inDirectSpace":        "false",
		"fileCategory":         "0",
		"event":                "homeListFile",
		"OnlyLookAbnormalFile": "0",
	}
	headers := map[string]string{"platform": "web"}
	resp, err := c.reqAuto(ctx, "GET", YunBase+"/api/file/list/new", headers, params, nil)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, &APIError{Code: resp.Code, Message: resp.MessageString()}
	}
	var data struct {
		InfoList []FileInfo `json:"InfoList"`
		FileList []FileInfo `json:"FileList"`
		Next     string     `json:"Next"`
		Total    int64      `json:"Total"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, err
	}
	fileList := data.FileList
	if len(fileList) == 0 {
		fileList = data.InfoList
	}
	// 翻页拉全（基于页码）
	if data.Next != "" && data.Next != "-1" {
		for {
			params["Page"] = data.Next
			r2, err := c.reqAuto(ctx, "GET", YunBase+"/api/file/list/new", headers, params, nil)
			if err != nil {
				break
			}
			if !r2.IsSuccess() {
				break
			}
			var d2 struct {
				InfoList []FileInfo `json:"InfoList"`
				FileList []FileInfo `json:"FileList"`
				Next     string     `json:"Next"`
			}
			if err := json.Unmarshal(r2.Data, &d2); err != nil {
				break
			}
			d2List := d2.FileList
			if len(d2List) == 0 {
				d2List = d2.InfoList
			}
			fileList = append(fileList, d2List...)
			data.Next = d2.Next
			if d2.Next == "" || d2.Next == "-1" {
				break
			}
		}
	}
	return fileList, nil
}

// FSMkdir 创建目录（yun Web 接口，等效 Python upload_request + type=1，参数名统一小写）。
func (c *Client) FSMkdir(ctx context.Context, name string, parentID any, duplicate int) (*Response, error) {
	body := map[string]any{
		"filename":     name,
		"parentfileid": toStr(parentID),
		"type":         1,
		"driveid":      0,
		"etag":         "",
		"size":         0,
		"notreuse":     true,
		"duplicate":    duplicate,
	}
	return c.reqAuto(ctx, "POST", YunBase+"/api/file/upload_request", nil, nil, body)
}

// TrashFile 把单个文件/目录移入回收站（yun Web 接口，驼峰键名）。
// 失败/已删除返回 false，不抛异常。
func (c *Client) TrashFile(ctx context.Context, fileID any) (bool, error) {
	body := map[string]any{
		"driveId":   0,
		"event":     "intoRecycle",
		"operation": true,
		"fileTrashInfoList": []any{
			map[string]any{"FileId": toInt64(fileID)},
		},
	}
	resp, err := c.req(ctx, "POST", YunBase+"/api/file/trash", nil, nil, body)
	if err != nil {
		log.Printf("[123] 移入回收站失败 file_id=%v: %v", fileID, err)
		return false, err
	}
	var data struct {
		InfoList []any `json:"InfoList"`
	}
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &data)
	}
	if len(data.InfoList) == 0 {
		log.Printf("[123] 移入回收站未生效 file_id=%v（已删除/已在回收站/无效id）", fileID)
		return false, nil
	}
	return true, nil
}

// FSTrash 批量删除文件至回收站（yun Web 接口）。
func (c *Client) FSTrash(ctx context.Context, fileIDs []int64) (*Response, error) {
	body := map[string]any{
		"driveId":   0,
		"event":     "intoRecycle",
		"operation": true,
		"fileTrashInfoList": []any{
			map[string]any{"FileId": fileIDs},
		},
	}
	return c.req(ctx, "POST", YunBase+"/api/file/trash", nil, nil, body)
}

// FSMove 移动文件到目标目录（yun Web 接口）。
func (c *Client) FSMove(ctx context.Context, fileIDs []int64, toParentFileID any) (*Response, error) {
	fileIDList := make([]map[string]any, 0, len(fileIDs))
	for _, id := range fileIDs {
		fileIDList = append(fileIDList, map[string]any{"FileId": id})
	}
	return c.reqAuto(ctx, "POST", YunBase+"/api/file/mod_pid", nil, nil, map[string]any{
		"fileIdList":   fileIDList,
		"parentFileId": toInt64(toParentFileID),
		"event":        "fileMove",
	})
}

// FSCopy 复制单个文件到目标目录（yun Web 接口）。
func (c *Client) FSCopy(ctx context.Context, fileID any, targetDirID any) (*Response, error) {
	return c.reqAuto(ctx, "POST", YunBase+"/api/restful/goapi/v1/file/copy/async", nil, nil, map[string]any{
		"fileList":     []any{map[string]any{"FileId": toInt64(fileID)}},
		"targetFileId": toInt64(targetDirID),
	})
}

// FileDetail 单个文件详情。
type FileDetail struct {
	FileID       int64  `json:"fileId"`
	FileName     string `json:"fileName"`
	Size         int64  `json:"size"`
	Type         int    `json:"type"`
	ParentFileID int64  `json:"parentFileId"`
}

// FSDetail 获取单个文件详情（yun Web 接口）。
func (c *Client) FSDetail(ctx context.Context, fileID any) (*FileDetail, error) {
	params := map[string]string{"fileID": toStr(fileID)}
	resp, err := c.reqAuto(ctx, "GET", YunBase+"/api/file/detail", nil, params, nil)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, &APIError{Code: resp.Code, Message: resp.MessageString()}
	}
	var d FileDetail
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// RenameFile 重命名文件（yun Web 接口 file/rename，驼峰键名）。
// 123 接口业务错误（HTTP 200 + code≠0）一律视为失败并返回 APIError，避免误判重命名成功。
func (c *Client) RenameFile(ctx context.Context, fileID any, newName string) (*Response, error) {
	body := map[string]any{
		"FileId":    toInt64(fileID),
		"fileName":  newName,
		"driveId":   0,
		"duplicate": 0,
		"event":     "fileRename",
	}
	resp, err := c.req(ctx, "POST", YunBase+"/api/file/rename", nil, nil, body)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, &APIError{Code: resp.Code, Message: resp.MessageString()}
	}
	return resp, nil
}

// ---------- 上传（Web 接口） ----------

// UploadFile 上传本地小文件到 123 云盘（yun Web 接口：upload_request + presignedURL PUT）。
// 计算 MD5 尝试秒传，失败则走预签名直传。duplicate: 0=提示 1=保留两者 2=替换。
func (c *Client) UploadFile(ctx context.Context, localPath string, parentID any, fileName string, duplicate int) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	if fileName == "" {
		fileName = filepathBase(localPath)
	}
	// 文件名非法字符转全角（与 Python escape_filename 一致）
	fileName = EscapeFileName(fileName)
	size := len(data)
	md5sum := md5Hex(data)

	reqBody := map[string]any{
		"etag":         md5sum,
		"fileName":     fileName,
		"size":         size,
		"parentFileId": toInt64(parentID),
		"type":         0,
		"duplicate":    duplicate,
	}
	resp, err := c.req(ctx, "POST", YunBase+"/api/file/upload_request", nil, nil, reqBody)
	if err != nil {
		return err
	}
	if !resp.IsSuccess() {
		return &APIError{Code: resp.Code, Message: resp.MessageString()}
	}
	var up struct {
		Reuse        bool   `json:"Reuse"`
		PresignedURL string `json:"presignedURL"`
	}
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &up)
	}
	if up.Reuse {
		return nil // 秒传成功
	}
	if up.PresignedURL == "" {
		return fmt.Errorf("upload_request 返回无 presignedURL: %s", string(resp.Data))
	}
	// PUT 直传
	raw, status, err := c.http.Put(ctx, up.PresignedURL, map[string]string{"Content-Type": "application/octet-stream"}, data)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("上传文件 PUT 失败 HTTP %d: %s", status, string(raw))
	}
	return nil
}

// UploadRequest 请求上传（秒传/建目录，yun Web 接口）。
// type=0 文件、type=1 目录；etag 为文件 MD5。
func (c *Client) UploadRequest(ctx context.Context, body map[string]any) (*Response, error) {
	payload := map[string]any{
		"driveId":      0,
		"duplicate":    0,
		"etag":         "",
		"parentFileId": 0,
		"size":         0,
		"type":         1,
		"NotReuse":     false,
	}
	for k, v := range body {
		payload[k] = v
	}
	if sz, _ := payload["size"].(int64); sz > 0 {
		payload["type"] = 0
	} else if e, _ := payload["etag"].(string); e != "" {
		payload["type"] = 0
	}
	return c.req(ctx, "POST", YunBase+"/api/file/upload_request", nil, nil, payload)
}

// ---------- 分享（Web 接口） ----------

// ShareFileEntry 分享文件条目（yun share/get 返回）。
type ShareFileEntry struct {
	FileID       int64  `json:"FileId"`
	FileName     string `json:"FileName"`
	Size         int64  `json:"Size"`
	Type         int    `json:"Type"` // 1=目录
	Etag         string `json:"Etag"`
	ParentFileID int64  `json:"ParentFileId"`
}

// ShareGetResp yun /api/share/get 响应。
type ShareGetResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		InfoList []ShareFileEntry `json:"InfoList"`
		Next     string           `json:"Next"`
	} `json:"data"`
}

// shareUID 解析分享 key 对应的上传者 UID（按 key 缓存，一次转存只解析一次）。
// 网页版分享页（phoenix-share）先调 /gsb/s/share-key 解析 UID，再跳转到
// <uid>.share.123pan.com 加载文件列表与转存；该三级域名不受旧接口的热门分享风控影响。
func (c *Client) shareUID(ctx context.Context, shareKey string) (int64, error) {
	c.uidMu.Lock()
	if c.uidCache == nil {
		c.uidCache = make(map[string]int64)
	}
	if uid, ok := c.uidCache[shareKey]; ok {
		c.uidMu.Unlock()
		return uid, nil
	}
	c.uidMu.Unlock()

	raw, status, err := c.http.Get(ctx, "https://www.123pan.cn/gsb/s/share-key?shareKey="+shareKey, nil)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("123 share-key HTTP %d: %s", status, string(raw))
	}
	var r struct {
		Info struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				UserID int64 `json:"UserID"`
			} `json:"data"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, fmt.Errorf("解析 share-key 响应失败: %v, body: %s", err, string(raw))
	}
	if r.Info.Code != 0 {
		return 0, &APIError{Code: r.Info.Code, Message: r.Info.Message}
	}
	c.uidMu.Lock()
	c.uidCache[shareKey] = r.Info.Data.UserID
	c.uidMu.Unlock()
	return r.Info.Data.UserID, nil
}

// ShareGet 获取分享文件列表。2026-09-03 起改走新版分享域名：
// 先解析上传者 UID，再请求 https://<uid>.share.123pan.com/api/share/get
// （参数与响应结构与旧版一致）。旧域名 yun.123pan.com/api/share/get 对热门分享
// 会被 123 风控返回 429"分享界面操作频繁"（网页前端早已迁移，不受影响），
// 实测同一分享同一时刻：yun.123pan.com 429、<uid>.share.123pan.com code 0 秒回。
func (c *Client) ShareGet(ctx context.Context, shareKey, sharePwd string, parentFileID any, page int) (*ShareGetResp, error) {
	uid, err := c.shareUID(ctx, shareKey)
	if err != nil {
		return nil, err
	}
	params := map[string]string{
		"ShareKey":       shareKey,
		"SharePwd":       sharePwd,
		"parentFileId":   toStr(parentFileID),
		"Page":           strconv.Itoa(page),
		"limit":          "100",
		"next":           "0",
		"event":          "homeListFile",
		"orderBy":        "file_name",
		"orderDirection": "asc",
	}
	base := fmt.Sprintf("https://%d.share.123pan.com", uid)
	raw, status, err := c.http.Get(ctx, base+"/api/share/get?"+encodeQuery(params), map[string]string{
		"Authorization": "Bearer " + c.Token,
		"Referer":       base + "/123pan/" + shareKey,
	})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("123 share/get HTTP %d: %s", status, string(raw))
	}
	return parseShareGetResponse(raw)
}

// parseShareGetResponse 解析 share/get 响应。123 风控的响应 code 为字符串、data 为字符串
// "null"（如 {"code":"429","message":"分享界面操作频繁，请稍候再试","data":"null"}），
// 按 int 结构体直接解析会报 JSON 错；这里先宽松解析业务码，失败时返回带真实 message 的错误。
func parseShareGetResponse(raw []byte) (*ShareGetResp, error) {
	var head struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("解析 share/get 响应失败: %v, body: %s", err, string(raw))
	}
	if c := codeAsInt(head.Code); c != 0 && c != 200 {
		var msg string
		if len(head.Message) > 0 {
			json.Unmarshal(head.Message, &msg) // 忽略非字符串 message
		}
		return nil, &APIError{Code: c, Message: msg}
	}
	var resp ShareGetResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 share/get 响应失败: %v, body: %s", err, string(raw))
	}
	return &resp, nil
}

// ShareCopy 转存分享文件（yun /api/file/copy/async，snake_case 字段）。
func (c *Client) ShareCopy(ctx context.Context, shareKey, sharePwd string, fileList []map[string]any) (*Response, error) {
	body := map[string]any{
		"share_key":     shareKey,
		"share_pwd":     sharePwd,
		"current_level": 1,
		"event":         "transfer",
		"file_list":     fileList,
	}
	return c.req(ctx, "POST", YunBase+"/api/file/copy/async", nil, nil, body)
}

// ---------- 下载（Web 接口） ----------

// DownloadInfo 获取文件下载信息（yun Web 接口，按 fileID）。
func (c *Client) DownloadInfo(ctx context.Context, fileID any) (*Response, error) {
	params := map[string]string{"fileId": toStr(fileID)}
	return c.reqAuto(ctx, "GET", YunBase+"/api/v1/file/download_info", map[string]string{"platform": "android"}, params, nil)
}

// DownloadInfoWithPayload 获取文件下载信息（yun Web 接口）。
func (c *Client) DownloadInfoWithPayload(ctx context.Context, payload map[string]any, headers map[string]string) (*Response, error) {
	h := map[string]string{"platform": "android"}
	for k, v := range headers {
		h[k] = v
	}
	return c.reqAuto(ctx, "POST", YunBase+"/api/file/download_info", h, nil, payload)
}

// ---------- 工具 ----------

// IterDirItem 递归遍历目录时的输出项。
type IterDirItem struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	RelPath  string    `json:"relpath"`
	ParentID int64     `json:"parent_id"`
	Raw      *FileInfo `json:"raw,omitempty"` // keep_raw=true 时保留
}

// IterDir 递归遍历目录文件列表（对应 p123client.tool.iterdir）。
// maxDepth < 0 表示无限深度。cooldown 为两次 API 调用之间的最小间隔。
func (c *Client) IterDir(ctx context.Context, parentFileID any, maxDepth int, keepRaw bool, cooldown time.Duration) (<-chan IterDirItem, error) {
	ch := make(chan IterDirItem, 100)
	go func() {
		defer close(ch)
		c.iterDirRecursive(ctx, ch, parentFileID, 0, maxDepth, keepRaw, cooldown, "")
	}()
	return ch, nil
}

func (c *Client) iterDirRecursive(ctx context.Context, ch chan<- IterDirItem, parentFileID any, depth, maxDepth int, keepRaw bool, cooldown time.Duration, relPrefix string) {
	if maxDepth >= 0 && depth >= maxDepth {
		return
	}
	list, err := c.FSList(ctx, parentFileID)
	if err != nil {
		log.Printf("[123] IterDir 列表失败 parent=%v: %v", parentFileID, err)
		return
	}
	for _, fi := range list {
		relPath := relPrefix + fi.FileName
		if fi.Type == 1 { // 目录
			ch <- IterDirItem{
				ID:       fi.FileID,
				Name:     fi.FileName,
				IsDir:    true,
				RelPath:  relPath + "/",
				ParentID: fi.ParentFileID,
			}
			if maxDepth < 0 || depth+1 < maxDepth {
				time.Sleep(cooldown)
				c.iterDirRecursive(ctx, ch, fi.FileID, depth+1, maxDepth, keepRaw, cooldown, relPath+"/")
			}
		} else { // 文件
			item := IterDirItem{
				ID:       fi.FileID,
				Name:     fi.FileName,
				IsDir:    false,
				RelPath:  relPath,
				ParentID: fi.ParentFileID,
			}
			if keepRaw {
				item.Raw = &fi
			}
			ch <- item
		}
	}
}

// IterDirConcurrent 并发递归遍历目录：由 concurrency 个 worker 并行拉取目录列表，
// cooldown 为单个 worker 两次列表请求之间的最小间隔，maxDepth < 0 表示无限深度。
// 与 IterDir 的串行深度优先不同，子目录列表请求可并行执行，适合大目录树全量扫描。
func (c *Client) IterDirConcurrent(ctx context.Context, parentFileID any, maxDepth int, keepRaw bool, cooldown time.Duration, concurrency int) (<-chan IterDirItem, error) {
	return iterDirConcurrent(ctx, c.FSList, parentFileID, maxDepth, keepRaw, cooldown, concurrency)
}

func iterDirConcurrent(ctx context.Context, listFn func(ctx context.Context, parentFileID any) ([]FileInfo, error), parentFileID any, maxDepth int, keepRaw bool, cooldown time.Duration, concurrency int) (<-chan IterDirItem, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	ch := make(chan IterDirItem, 100)
	go func() {
		defer close(ch)
		type task struct {
			id    any
			depth int
			rel   string
		}
		// 任务队列：mutex+cond 实现，避免关闭竞态（worker 可能一边投递一边被要求退出）。
		// pending 表示尚未完成的目录任务数（队列中 + 正在处理），归零即全部完成。
		var mu sync.Mutex
		var queue []task
		pending := 1 // 根目录
		cond := sync.NewCond(&mu)
		queue = append(queue, task{id: parentFileID, depth: 0, rel: ""})

		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					mu.Lock()
					for len(queue) == 0 && pending > 0 && ctx.Err() == nil {
						cond.Wait()
					}
					if pending == 0 || ctx.Err() != nil {
						mu.Unlock()
						return
					}
					t := queue[0]
					queue = queue[1:]
					mu.Unlock()

					if cooldown > 0 {
						time.Sleep(cooldown)
					}
					if maxDepth >= 0 && t.depth >= maxDepth {
						mu.Lock()
						pending--
						mu.Unlock()
						cond.Broadcast()
						continue
					}
					list, err := listFn(ctx, t.id)
					if err != nil {
						log.Printf("[123] IterDir 列表失败 parent=%v: %v", t.id, err)
						mu.Lock()
						pending--
						mu.Unlock()
						cond.Broadcast()
						continue
					}
					for _, fi := range list {
						relPath := t.rel + fi.FileName
						if fi.Type == 1 { // 目录
							ch <- IterDirItem{ID: fi.FileID, Name: fi.FileName, IsDir: true, RelPath: relPath + "/", ParentID: fi.ParentFileID}
							if maxDepth < 0 || t.depth+1 < maxDepth {
								mu.Lock()
								pending++
								queue = append(queue, task{id: fi.FileID, depth: t.depth + 1, rel: relPath + "/"})
								mu.Unlock()
							}
						} else { // 文件
							item := IterDirItem{ID: fi.FileID, Name: fi.FileName, IsDir: false, RelPath: relPath, ParentID: fi.ParentFileID}
							if keepRaw {
								item.Raw = &fi
							}
							ch <- item
						}
					}
					mu.Lock()
					pending--
					mu.Unlock()
					cond.Broadcast()
				}
			}()
		}
		wg.Wait()
	}()
	return ch, nil
}

// ---------- 磁力链接离线下载 ----------

// SubmitMagnet 提交磁力链接离线下载（add_mag.py）。
func (c *Client) SubmitMagnet(ctx context.Context, magnet, uploadDirID string) (map[string]any, error) {
	// 1. 解析磁力链
	resolveURL := YunBase + "/api/v2/offline_download/task/resolve"
	respRaw, status, err := c.http.PostJSON(ctx, resolveURL, c.authHeaders(), map[string]string{"urls": magnet})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("解析磁力链 HTTP %d: %s", status, string(respRaw))
	}
	var resolve struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []struct {
				ID    any `json:"id"`
				Files []struct {
					ID       any    `json:"id"`
					Name     string `json:"name"`
					Size     int64  `json:"size"`
					Category int    `json:"category"`
				} `json:"files"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respRaw, &resolve); err != nil {
		return nil, err
	}
	if resolve.Code != 0 && resolve.Code != 200 {
		return nil, fmt.Errorf("解析磁力链返回错误: %s", resolve.Message)
	}
	if len(resolve.Data.List) == 0 {
		return nil, fmt.Errorf("未找到对应的资源数据")
	}
	resource := resolve.Data.List[0]
	resourceID := toStr(resource.ID)

	// 2. 筛选视频文件
	const minSize = 50 * 1024 * 1024
	excluded := []string{"sample", "trailer", "bonus", "extra"}
	var videoIDs []any
	for _, f := range resource.Files {
		name := strings.ToLower(f.Name)
		isVideo := f.Category == 2 || strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".mkv")
		if !isVideo || f.Size <= minSize {
			continue
		}
		skip := false
		for _, kw := range excluded {
			if strings.Contains(name, kw) {
				skip = true
				break
			}
		}
		if !skip {
			videoIDs = append(videoIDs, f.ID)
		}
	}
	if len(videoIDs) == 0 {
		return nil, fmt.Errorf("未找到大小大于50MB且不含sample/trailer/bonus/extra字样的视频文件")
	}

	// 3. 提交下载任务
	submitURL := YunBase + "/api/v2/offline_download/task/submit"
	submitPayload := map[string]any{
		"resource_list": []any{
			map[string]any{"resource_id": resourceID, "select_file_id": videoIDs},
		},
		"upload_dir": toInt64(uploadDirID),
	}
	raw2, status2, err := c.http.PostJSON(ctx, submitURL, c.authHeaders(), submitPayload)
	if err != nil {
		return nil, err
	}
	if status2 != 200 {
		return nil, fmt.Errorf("提交下载任务 HTTP %d: %s", status2, string(raw2))
	}
	var submit map[string]any
	if err := json.Unmarshal(raw2, &submit); err != nil {
		return nil, err
	}
	return submit, nil
}

// ---------- 开放平台接口（仅 OAuth/115秒传使用） ----------

// UploadSha1Reuse sha1 哈希值文件上传（open-api，仅 115→123 秒传使用）。
func (c *Client) UploadSha1Reuse(ctx context.Context, payload map[string]any) (*Response, error) {
	p := map[string]any{
		"filename":     fmt.Sprintf("%v-%v", payload["sha1"], payload["size"]),
		"parentFileId": "0",
	}
	for k, v := range payload {
		p[k] = v
	}
	return c.reqAuto(ctx, "POST", OpenBase+"/upload/v2/file/sha1_reuse", nil, nil, p)
}

// Request 通用 open-api 请求（仅 OAuth/115秒传使用）。path 为 OpenBase 下的路径，如 "/api/v1/share/create"。
func (c *Client) Request(ctx context.Context, method, path string, params map[string]string, body any) (*Response, error) {
	return c.reqAuto(ctx, method, OpenBase+path, nil, params, body)
}

// ---------- 辅助函数 ----------

func (c *Client) authToken() string {
	return c.Token
}

func (c *Client) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + c.Token, "Content-Type": "application/json"}
}

func filepathBase(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// EscapeFileName 把 123 云盘不允许的文件名字符（\\ / : * ? | < > "）转成全角。
func EscapeFileName(name string) string {
	repl := map[rune]rune{'\\': '＼', '/': '／', ':': '：', '*': '＊', '?': '？', '|': '｜', '<': '＜', '>': '＞', '"': '＂'}
	return strings.Map(func(r rune) rune {
		if nr, ok := repl[r]; ok {
			return nr
		}
		return r
	}, name)
}

// LoadTokenFromFile 从 token 文件加载 token（db/data/config.txt 格式：裸 token 或 JSON）。
func (c *Client) LoadTokenFromFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return false
	}
	// 兼容 JSON {access_token: ...}
	if strings.HasPrefix(s, "{") {
		var t struct {
			AccessToken string `json:"access_token"`
			Token       string `json:"token"`
		}
		if err := json.Unmarshal(data, &t); err == nil {
			if t.AccessToken != "" {
				c.Token = t.AccessToken
			} else if t.Token != "" {
				c.Token = t.Token
			}
			return c.Token != ""
		}
	}
	c.Token = s
	return true
}

// SaveTokenToFile 保存 token 到文件。
func (c *Client) SaveTokenToFile(path string) error {
	return os.WriteFile(path, []byte(strings.TrimSpace(c.Token)), 0o644)
}

// IsTokenExpired 简单判断 token 是否可能过期（非空即视为有效，交由接口验证）。
func (c *Client) IsTokenExpired() bool { return c.Token == "" }

func encodeQuery(m map[string]string) string {
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

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

func toInt64(v any) int64 {
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
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func md5Hex(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}
