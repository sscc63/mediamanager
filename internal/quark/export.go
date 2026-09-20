package quark

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"regexp"
	"strings"
)

// ShareFIDToken 分享文件条目 token 字段。
func (f *ShareFile) Token() string { return f.ShareFIDToken }

// ShareFIDToken 分享文件 token（字段单独解析，见 List 处理）。

// ExtractShareInfoFromURL 从分享 URL 提取分享 ID 和密码。
func ExtractShareInfoFromURL(shareURL string) (string, string, error) {
	re := regexp.MustCompile(`/s/([a-zA-Z0-9]+)`)
	m := re.FindStringSubmatch(shareURL)
	if m == nil {
		return "", "", &ErrInvalidURL{URL: shareURL}
	}
	shareID := m[1]
	password := ""
	if pm := regexp.MustCompile(`pwd=([a-zA-Z0-9]+)`).FindStringSubmatch(shareURL); pm != nil {
		password = pm[1]
	}
	return shareID, password, nil
}

// ErrInvalidURL 无效分享 URL。
type ErrInvalidURL struct{ URL string }

func (e *ErrInvalidURL) Error() string { return "无效的分享URL: " + e.URL }

// SanitizeString 清理字符串中的无效 Unicode 字符。
func SanitizeString(s string) string {
	return strings.ToValidUTF8(s, "?")
}

// ExportShareInfo 导出夸克分享秒传数据（JSON），失败返回 (nil, err)。
func (c *Client) ExportShareInfo(shareURL, cookie string) (map[string]any, error) {
	ctx := context.Background()
	code, password, err := ExtractShareInfoFromURL(shareURL)
	if err != nil {
		log.Printf("[夸克] 错误: %v", err)
		return nil, err
	}
	log.Printf("[夸克] 从URL提取到分享ID: %s，密码: %s", code, orNone(password))

	stoken, err := c.GetShareInfo(ctx, code, password)
	if err != nil {
		log.Printf("[夸克] 获取分享信息失败: %v", err)
		return nil, err
	}
	log.Printf("[夸克] 正在收集文件信息")

	// 收集文件（递归）: fid -> file_base, 以及 (fid, token) 列表
	fileMapping := map[string]map[string]any{}
	var filesInfo [][2]string
	err = c.WalkShareFiles(ctx, code, password, stoken, "0", "", func(f *ShareFile) error {
		if f.Dir {
			return nil
		}
		base := map[string]any{
			"size": f.Size,
			"path": SanitizeString(strings.TrimPrefix(f.Path, "/")),
		}
		fileMapping[f.FID] = base
		filesInfo = append(filesInfo, [2]string{f.FID, f.ShareFIDToken})
		return nil
	})
	if err != nil {
		log.Printf("[夸克] 收集文件失败: %v", err)
		return nil, err
	}

	total := len(filesInfo)
	log.Printf("[夸克] 已收集 %d 个文件信息，开始批量获取MD5值 (批次大小: 50)", total)
	if total == 0 {
		log.Printf("[夸克] 未找到任何文件")
		return nil, errors.New("分享链接里没有文件")
	}

	fids := make([]string, 0, total)
	tokens := make([]string, 0, total)
	for _, ft := range filesInfo {
		fids = append(fids, ft[0])
		tokens = append(tokens, ft[1])
	}
	md5Results := c.BatchGetFileDownloadInfo(ctx, code, password, stoken, fids, tokens)

	jsonData := map[string]any{
		"usesBase62EtagsInExport": false,
		"files":                   []any{},
	}
	var fileList []any
	for fid, base := range fileMapping {
		if info, ok := md5Results[fid]; ok && info.Md5 != "" {
			md5 := info.Md5
			if strings.Contains(md5, "==") {
				dec, derr := base64.StdEncoding.DecodeString(md5)
				if derr == nil {
					md5 = hex.EncodeToString(dec)
				}
			}
			base["etag"] = md5
			fileList = append(fileList, base)
		}
	}
	if len(fileList) == 0 {
		log.Printf("[夸克] 未获取到任何文件的MD5值")
		return nil, errors.New("未获取到任何文件的MD5值")
	}
	jsonData["files"] = fileList
	log.Printf("[夸克] 导出完成，共 %d 个文件", len(fileList))
	return jsonData, nil
}

func orNone(s string) string {
	if s == "" {
		return "无"
	}
	return s
}
