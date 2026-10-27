// Package strm 本地 STRM 库扫描/解析/搜索（对应 emby_scanner_engine/runtime.py 的扫描部分）。
// 配置复用 ENV_STRM_PATHS 映射（本地目录#网盘目录），供 TG /delete 命令与查重删除使用。
package strm

import (
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// StrmMeta STRM 路径解析出的媒体信息。
type StrmMeta struct {
	Kind    string // movie / tv
	Title   string
	Year    int
	Season  int // -1 表示无
	Episode int // -1 表示无
	Label   string
}

// StrmInfo STRM 文件内容解析结果（重定向 URL 参数）。
type StrmInfo struct {
	Size      int64
	Name      string
	MD5       string
	S3KeyFlag string
	FileID    int64
}

// StrmGroup 按 (类型, 标题, 年份) 分组的 STRM 资源（/delete 候选）。
type StrmGroup struct {
	Kind      string
	Title     string
	Year      int
	Count     int
	TotalSize int64
	Files     []string
}

// LocalRoots 解析 STRM 映射中的本地根目录列表（去重）。
func LocalRoots(strmPaths string) []string {
	var roots []string
	for _, line := range strings.Split(strmPaths, "\n") {
		line = strings.TrimSpace(strings.Trim(line, `"`))
		if line == "" {
			continue
		}
		localDir := line
		if idx := strings.Index(line, "#"); idx >= 0 {
			localDir = line[:idx]
		}
		localDir = strings.TrimSpace(localDir)
		localDir = strings.ReplaceAll(localDir, "\\", "/")
		localDir = strings.TrimSuffix(localDir, "/")
		if localDir != "" && !containsStr(roots, localDir) {
			roots = append(roots, localDir)
		}
	}
	return roots
}

// IterStrmFiles 递归遍历本地 STRM 文件（.strm 后缀）。
func IterStrmFiles(roots []string, fn func(path string)) {
	for _, root := range roots {
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if fi.IsDir() {
				return nil
			}
			if strings.HasSuffix(strings.ToLower(fi.Name()), ".strm") {
				fn(p)
			}
			return nil
		})
	}
}

// ReadURL 读取 STRM 文件内容，解析重定向 URL 参数（size/name/md5）。
func ReadURL(strmPath string) StrmInfo {
	var info StrmInfo
	data, err := os.ReadFile(strmPath)
	if err != nil {
		return info
	}
	u := strings.TrimSpace(string(data))
	parsed, err := url.Parse(u)
	if err != nil {
		return info
	}
	q := parsed.Query()
	if v := q.Get("size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			info.Size = n
		}
	}
	info.Name = q.Get("name")
	info.MD5 = q.Get("md5")
	info.S3KeyFlag = q.Get("s3_key_flag")
	if v := q.Get("file_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			info.FileID = n
		}
	}
	return info
}

// RewriteStrmURLs 遍历本地 STRM 文件，用新的 serverAddress + apiKey 重写 URL。
// 返回 (总文件数, 成功数, 失败数)。
func RewriteStrmURLs(roots []string, serverAddress, apiKey string, concurrency int) (total, okCount, failCount int) {
	serverAddress = strings.TrimRight(serverAddress, "/")
	if concurrency < 1 {
		concurrency = 1
	}
	type result struct {
		path   string
		ok     bool
		reason string
	}
	ch := make(chan string, concurrency)
	resCh := make(chan result, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ch {
				info := ReadURL(p)
				if info.Name == "" || info.Size == 0 {
					resCh <- result{p, false, "缺少关键参数"}
					continue
				}
				s3KeyFlag := info.S3KeyFlag
				if s3KeyFlag == "" {
					s3KeyFlag = info.MD5
				}
				newURL := BuildRedirectURL(serverAddress, apiKey, info.Name, info.Size, info.MD5, s3KeyFlag, info.FileID)
				if err := os.WriteFile(p, []byte(newURL), 0o644); err != nil {
					resCh <- result{p, false, err.Error()}
					continue
				}
				resCh <- result{p, true, ""}
			}
		}()
	}
	go func() {
		IterStrmFiles(roots, func(p string) { ch <- p })
		close(ch)
	}()
	go func() {
		wg.Wait()
		close(resCh)
	}()
	for r := range resCh {
		total++
		if r.ok {
			okCount++
		} else {
			failCount++
			log.Printf("【改写STRM】%s 失败: %s", r.path, r.reason)
		}
	}
	return
}

// hasCJK 判断是否包含中文字符。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// cleanTitle 清洗目录名/文件名中的装饰内容（tmdbid、年份、季/集、完结等）。
func cleanTitle(raw string) string {
	if raw == "" {
		return ""
	}
	t := strings.TrimSpace(raw)
	t = regexp.MustCompile(`\{tmdb[^}]*\}`).ReplaceAllString(t, "")
	t = regexp.MustCompile(`\[tmdb[^\]]*\]`).ReplaceAllString(t, "")
	t = regexp.MustCompile(`[\(\[（]\s*\d{4}\s*[\)\]）]`).ReplaceAllString(t, "")
	t = regexp.MustCompile(`第\s*\d+\s*季`).ReplaceAllString(t, "")
	t = regexp.MustCompile(`(?i)Season\s*\d+`).ReplaceAllString(t, "")
	t = regexp.MustCompile(`更新至\s*\d+\s*集`).ReplaceAllString(t, "")
	t = regexp.MustCompile(`全\s*\d+\s*集`).ReplaceAllString(t, "")
	t = strings.ReplaceAll(t, "完结", "")
	t = strings.ReplaceAll(t, "_", " ")
	t = strings.ReplaceAll(t, ".", " ")
	t = regexp.MustCompile(`\s{2,}`).ReplaceAllString(t, " ")
	t = strings.Trim(t, " -–—")
	return t
}

// titleFromPath 从文件路径提取中文标题：优先父目录名，其次文件名首段。
func titleFromPath(p string) string {
	if p == "" {
		return ""
	}
	norm := strings.ReplaceAll(p, "\\", "/")
	norm = strings.TrimSuffix(norm, "/")
	parts := splitPath(norm)
	if len(parts) == 0 {
		return ""
	}
	if len(parts) >= 2 {
		parent := cleanTitle(parts[len(parts)-2])
		if parent != "" && hasCJK(parent) {
			return parent
		}
	}
	base := strings.TrimSuffix(path.Base(norm), path.Ext(path.Base(norm)))
	for _, seg := range strings.Split(base, ".") {
		seg = strings.TrimSpace(seg)
		if seg != "" && hasCJK(seg) {
			if cleaned := cleanTitle(seg); cleaned != "" {
				return cleaned
			}
		}
	}
	return ""
}

func splitPath(p string) []string {
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}

var (
	sxxexxRe = regexp.MustCompile(`[Ss](\d{1,2})[Ee](\d{1,3})`)
	// 年份匹配：独立的 4 位数字（前后不是数字）
	yearRe = regexp.MustCompile(`(?:^|[^\d])(\d{4})(?:[^\d]|$)`)
	// tmdb 标识：[tmdb=xxx] [tmdb-xxx] {tmdbid=xxx} {tmdbid-xxx} 等
	tmdbAnyRe    = regexp.MustCompile(`(?i)[{\[]tmdb(?:id)?(?:=|-)\d+[}\]]`)
	seasonDirRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)season\s*(\d+)`),
		regexp.MustCompile(`第\s*(\d+)\s*季`),
		regexp.MustCompile(`^[Ss](\d{1,2})$`),
	}
	episodeAlwaysRes  = []*regexp.Regexp{regexp.MustCompile(`第\s*(\d+)\s*[集話话]`)}
	episodeSeasonOnly = []*regexp.Regexp{
		regexp.MustCompile(`[Ee](\d{1,3})(?:\D|$)`),
		regexp.MustCompile(`[\x{4e00}-\x{9fff}](\d{1,3})[\x{4e00}-\x{9fff}]`),
		regexp.MustCompile(`(?:^|[^\d.])(\d{1,3})\s*完?$`),
	}
)

// ParseMeta 从本地 STRM 路径解析媒体信息（电影/剧集、标题、年份、季、集）。
func ParseMeta(p string) StrmMeta {
	var meta StrmMeta
	meta.Season = -1
	meta.Episode = -1
	norm := strings.ReplaceAll(p, "\\", "/")
	parts := splitPath(norm)
	if len(parts) < 2 {
		return meta
	}
	base := path.Base(norm)
	stem := strings.TrimSuffix(base, path.Ext(base))
	dirs := parts[:len(parts)-1]

	// 季/集：优先文件名 SxxEyy
	season, episode := -1, -1
	if m := sxxexxRe.FindStringSubmatch(stem); m != nil {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
	}

	// 从文件所在目录向根找季目录
	titleDirIdx := len(dirs) - 1
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		matched := false
		for _, pat := range seasonDirRes {
			if m := pat.FindStringSubmatch(d); m != nil {
				if season == -1 {
					season, _ = strconv.Atoi(m[1])
				}
				// 默认 Season 的直接上一级
				titleDirIdx = i - 1
				// 继续向上找更可靠的标题目录：带年份或 tmdb 标识
				for j := i - 1; j >= 0; j-- {
					candidate := dirs[j]
					if yearRe.MatchString(candidate) || tmdbAnyRe.MatchString(candidate) {
						titleDirIdx = j
						break
					}
				}
				matched = true
				break
			}
		}
		if matched {
			break
		}
	}

	// 文件名补充提取集号
	if episode == -1 && stem != "" {
		for _, pat := range episodeAlwaysRes {
			if m := pat.FindStringSubmatch(stem); m != nil {
				episode, _ = strconv.Atoi(m[1])
				break
			}
		}
	}
	if episode == -1 && stem != "" && season != -1 {
		for _, pat := range episodeSeasonOnly {
			if m := pat.FindStringSubmatch(stem); m != nil {
				episode, _ = strconv.Atoi(m[1])
				break
			}
		}
	}

	if titleDirIdx < 0 {
		return meta
	}
	titleDirRaw := dirs[titleDirIdx]
	year := 0
	if ym := regexp.MustCompile(`(\d{4})`).FindStringSubmatch(titleDirRaw); ym != nil {
		year, _ = strconv.Atoi(ym[1])
	}
	title := cleanTitle(titleDirRaw)

	if season != -1 || episode != -1 {
		se := ""
		if season != -1 {
			se += "S" + pad2(season)
		}
		if episode != -1 {
			se += "E" + pad2(episode)
		}
		label := se
		if year > 0 {
			label = title + " (" + strconv.Itoa(year) + ") " + se
		} else {
			label = title + " " + se
		}
		return StrmMeta{Kind: "tv", Title: title, Year: year, Season: season, Episode: episode, Label: label}
	}
	if title == "" {
		title = titleFromPath(p)
	}
	label := title
	if year > 0 {
		label = title + " (" + strconv.Itoa(year) + ")"
	}
	return StrmMeta{Kind: "movie", Title: title, Year: year, Season: -1, Episode: -1, Label: label}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// SearchGroups 在本地 STRM 库中按标题搜索媒体，按 (类型, 标题, 年份) 分组返回。
func SearchGroups(keyword, strmPaths string) []StrmGroup {
	roots := LocalRoots(strmPaths)
	if len(roots) == 0 {
		return nil
	}
	kw := strings.ToLower(keyword)
	groups := map[string]*StrmGroup{}
	// 保持分组顺序稳定
	var order []string
	IterStrmFiles(roots, func(p string) {
		meta := ParseMeta(p)
		if meta.Title == "" {
			return
		}
		if !strings.Contains(strings.ToLower(meta.Title), kw) {
			return
		}
		key := meta.Kind + "\x00" + meta.Title + "\x00" + strconv.Itoa(meta.Year)
		g, ok := groups[key]
		if !ok {
			g = &StrmGroup{Kind: meta.Kind, Title: meta.Title, Year: meta.Year}
			groups[key] = g
			order = append(order, key)
		}
		g.Count++
		g.TotalSize += ReadURL(p).Size
		g.Files = append(g.Files, p)
	})
	result := make([]StrmGroup, 0, len(order))
	for _, key := range order {
		result = append(result, *groups[key])
	}
	return result
}

// ParseMappings 解析 STRM 映射（每行：本地目录#网盘目录）。
func ParseMappings(strmPaths string) [][2]string {
	var mappings [][2]string
	for _, line := range strings.Split(strmPaths, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "#") {
			continue
		}
		localDir := strings.TrimSpace(line[:strings.Index(line, "#")])
		panDir := strings.TrimSpace(line[strings.Index(line, "#")+1:])
		if localDir != "" && panDir != "" {
			mappings = append(mappings, [2]string{localDir, panDir})
		}
	}
	return mappings
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
