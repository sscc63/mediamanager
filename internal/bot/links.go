package bot

// 链接提取与解析（对应 123bot.py 的 extract_target_url / extract_kuake_target_url /
// extract_115_target_url / extract_123_links_from_full_text / parse_share_link /
// robust_normalize_md5 / optimized_etag_to_hex）。

import (
	"encoding/base64"
	"encoding/hex"
	"log"
	"math/big"
	"regexp"
	"strings"
)

// FileEntry 秒传文件条目（parse_share_link 解析结果，兼容 JSON/夸克/115 转存）。
type FileEntry struct {
	Path     string // 完整文件路径
	Etag     string
	Size     int64
	IsV2Etag bool
}

var (
	reTarget123 = regexp.MustCompile(`(?i)https?://(?:[^/\s"'<>]*\.)?(?:123pan|123865|123684)\.[^/\s"'<>]+(?:/s/|/123pan/)[\w-]+(?:\?pwd=\w+)?`)
	reTarget115 = regexp.MustCompile(`(?i)https?://(?:[^/\s"'<>]*\.)?(?:115|115cdn|anxia)\.com/s/[\w-]+(?:\?(?:password|pwd)=\w+)?`)
	reKuakeLink = regexp.MustCompile(`(?i)https?://pan\.quark\.cn/s/([\w-]+)(?:[#?].*)?`)
	reKuakePwd  = regexp.MustCompile(`[?&]pwd=(\w+)`)
	rePwdText   = regexp.MustCompile(`(?i)提取码[：:]?\s*(\w+)`)
	reMagnet    = regexp.MustCompile(`(?i)magnet:\?xt=urn(?:%3A|:)btih(?:%3A|:)(?:[A-Fa-f0-9]{40}\b|[A-Za-z0-9]{32}\b)(?:&.*?)?`)
	// 秒传链接：以 123FSLinkV1/2、123FLCPV1/2 开头，到文本 "\n"、"'}"、"'," 或结尾停止
	reFSLink = regexp.MustCompile(`(?s)(123FSLinkV[12]|123FLCPV[12]).*?(?:\\n|\}'|',|$)`)
)

const base62Chars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// extractTargetURL 提取 123 云盘分享链接（/s/ 与 /123pan/ 两种路径，可带子域前缀）。
func extractTargetURL(text string) []string {
	text = strings.TrimSpace(strings.Trim(text, "`"))
	matches := reTarget123.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		m = strings.TrimSpace(strings.Trim(m, "`"))
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// extract115TargetURL 提取 115 分享链接。
func extract115TargetURL(text string) []string {
	matches := reTarget115.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// extractKuakeTargetURL 提取夸克分享链接，自动附带 pwd（优先链接自带，其次文本提取码按序匹配）。
func extractKuakeTargetURL(text string) []string {
	type linkInfo struct {
		shareID    string
		builtInPwd string
	}
	processed := map[string]bool{}
	var infos []linkInfo
	for _, m := range reKuakeLink.FindAllStringSubmatch(text, -1) {
		shareID := m[1]
		if shareID == "" || processed[shareID] {
			continue
		}
		original := m[0]
		builtInPwd := ""
		if pm := reKuakePwd.FindStringSubmatch(original); pm != nil {
			builtInPwd = pm[1]
		}
		processed[shareID] = true
		infos = append(infos, linkInfo{shareID: strings.TrimSpace(shareID), builtInPwd: builtInPwd})
	}
	if len(infos) == 0 {
		return nil
	}
	// 文本提取码（去重保序）
	var passwords []string
	seenPwd := map[string]bool{}
	for _, pm := range rePwdText.FindAllStringSubmatch(text, -1) {
		p := pm[1]
		if !seenPwd[p] {
			seenPwd[p] = true
			passwords = append(passwords, p)
		}
	}
	var out []string
	for i, info := range infos {
		base := "https://pan.quark.cn/s/" + info.shareID
		finalPwd := info.builtInPwd
		if finalPwd == "" && i < len(passwords) {
			finalPwd = passwords[i]
		}
		if finalPwd != "" {
			out = append(out, base+"?pwd="+finalPwd)
		} else {
			out = append(out, base)
		}
	}
	// 最终去重（保序）
	seen := map[string]bool{}
	var uniq []string
	for _, u := range out {
		if !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	return uniq
}

// extract123LinksFromFullText 提取符合条件的 123 系列秒传链接（去重保序）。
func extract123LinksFromFullText(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reFSLink.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// extractMagnetLinks 提取磁力链接（兼容 URL 编码），返回 URL 解码后的去重列表。
func extractMagnetLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reMagnet.FindAllString(text, -1) {
		if strings.Contains(m, "%3A") {
			m = urlDecode(m)
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func urlDecode(s string) string {
	// 仅解码 %3A / %3a
	s = strings.ReplaceAll(s, "%3A", ":")
	s = strings.ReplaceAll(s, "%3a", ":")
	return s
}

// parseShareLink 解析秒传链接（123FSLinkV1/2、123FLCPV1/2）。
// 返回 nil 表示不是有效的秒传链接或未解析到文件。
func parseShareLink(shareLink string) []FileEntry {
	if !strings.Contains(shareLink, "#") || !strings.Contains(shareLink, "$") {
		return nil
	}
	log.Printf("解析秒传链接...")
	const (
		legacyV1  = "123FSLinkV1$"
		legacyV2  = "123FSLinkV2$"
		commonV1  = "123FLCPV1$"
		commonV2  = "123FLCPV2$"
		delimiter = "%"
	)
	isCommonPath := false
	isV2Etag := false
	switch {
	case strings.HasPrefix(shareLink, commonV2):
		isCommonPath = true
		isV2Etag = true
		shareLink = strings.TrimPrefix(shareLink, commonV2)
	case strings.HasPrefix(shareLink, commonV1):
		isCommonPath = true
		shareLink = strings.TrimPrefix(shareLink, commonV1)
	case strings.HasPrefix(shareLink, legacyV2):
		isV2Etag = true
		shareLink = strings.TrimPrefix(shareLink, legacyV2)
	case strings.HasPrefix(shareLink, legacyV1):
		shareLink = strings.TrimPrefix(shareLink, legacyV1)
	}

	commonBasePath := ""
	if isCommonPath {
		if pos := strings.Index(shareLink, delimiter); pos > -1 {
			commonBasePath = shareLink[:pos]
			shareLink = shareLink[pos+1:]
		}
	}

	var files []FileEntry
	for _, sLink := range strings.Split(shareLink, "$") {
		if sLink == "" {
			continue
		}
		parts := strings.Split(sLink, "#")
		if len(parts) < 3 {
			continue
		}
		etag := parts[0]
		size := parseSize(parts[1])
		filePath := strings.Join(parts[2:], "#")
		if isCommonPath && commonBasePath != "" {
			filePath = commonBasePath + filePath
		}
		files = append(files, FileEntry{Path: filePath, Etag: etag, Size: size, IsV2Etag: isV2Etag})
	}
	log.Printf("解析到 %d 个文件", len(files))
	if len(files) == 0 {
		return nil
	}
	return files
}

func parseSize(s string) int64 {
	var n int64
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// robustNormalizeMD5 自动识别 MD5 格式并转换为十六进制（小写），失败返回原输入。
func robustNormalizeMD5(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return input
	}
	// 32 位十六进制直接返回
	if len(input) == 32 && isHexString(input) {
		return strings.ToLower(input)
	}
	// 尝试 Base64 解码（标准 + URL 安全）
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(input); err == nil && len(raw) == 16 {
			return hex.EncodeToString(raw)
		}
	}
	return input
}

func isHexString(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// optimizedEtagToHex 将 Base62 编码的 ETag 转换为十六进制（isV2 时生效）。
func optimizedEtagToHex(etag string, isV2 bool) string {
	if !isV2 {
		return etag
	}
	if len(etag) == 32 && isHexString(etag) {
		return strings.ToLower(etag)
	}
	num := new(big.Int)
	for _, c := range etag {
		idx := strings.IndexRune(base62Chars, c)
		if idx < 0 {
			log.Printf("❌ ETag包含无效字符: %c", c)
			return etag
		}
		num.Mul(num, big.NewInt(62))
		num.Add(num, big.NewInt(int64(idx)))
	}
	hexStr := strings.ToLower(num.Text(16))
	if len(hexStr) > 32 {
		hexStr = hexStr[len(hexStr)-32:]
		log.Printf("ETag转换后长度超过32位，截断为: %s", hexStr)
	} else if len(hexStr) < 32 {
		hexStr = strings.Repeat("0", 32-len(hexStr)) + hexStr
		log.Printf("ETag转换后不足32位，补零后: %s", hexStr)
	}
	if len(hexStr) != 32 || !isHexString(hexStr) {
		log.Printf("❌ 转换后ETag格式无效: %s", hexStr)
		return etag
	}
	return hexStr
}
