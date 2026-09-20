package transfer

// 文件名识别模块（对应 meta_recognizer.py）。
// 使用正则模拟 guessit 解析，输出标准化的 MetaInfo。

import (
	"log"
	"regexp"
	"strconv"
	"strings"
)

// 视频文件扩展名（仅这些扩展名会被识别为视频文件）。
var VideoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".rmvb": true, ".wmv": true,
	".flv": true, ".mov": true, ".m4v": true, ".mpg": true, ".mpeg": true,
	".ts": true, ".m2ts": true, ".vob": true, ".webm": true,
}

// 字幕文件扩展名。
var SubtitleExtensions = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".idx": true,
	".sup": true, ".smi": true,
}

func isMediaExt(ext string) bool {
	return VideoExtensions[ext] || SubtitleExtensions[ext]
}

// IsVideoFile 判断是否为视频文件。
func IsVideoFile(filename string) bool {
	return VideoExtensions[extOf(filename)]
}

// IsSubtitleFile 判断是否为字幕文件。
func IsSubtitleFile(filename string) bool {
	return SubtitleExtensions[extOf(filename)]
}

func extOf(filename string) string {
	i := strings.LastIndexByte(filename, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(filename[i:])
}

// stripExt 去除视频/字幕扩展名。
func stripExt(filename string) string {
	for ext := range VideoExtensions {
		if strings.HasSuffix(strings.ToLower(filename), ext) {
			return filename[:len(filename)-len(ext)]
		}
	}
	for ext := range SubtitleExtensions {
		if strings.HasSuffix(strings.ToLower(filename), ext) {
			return filename[:len(filename)-len(ext)]
		}
	}
	return filename
}

// cleanTitle 清理标题中的分隔符，转为空格并去除多余空白。
var sepRe = regexp.MustCompile(`[\.\-_+\[\]]`)
var spaceRe = regexp.MustCompile(`\s+`)

func cleanTitle(title string) string {
	if title == "" {
		return ""
	}
	cleaned := sepRe.ReplaceAllString(title, " ")
	cleaned = spaceRe.ReplaceAllString(cleaned, " ")
	return strings.TrimSpace(cleaned)
}

// ---------- 正则识别 ----------

var (
	// 分辨率
	resRe = regexp.MustCompile(`(?i)(?:\b|_)(2160p|1080p|720p|480p|4k|8k|4320p)(?:\b|_)`)
	// 来源/片源
	sourceRe = regexp.MustCompile(`(?i)(?:\b|_)(BDREMUX|REMUX|BluRay|WEB-DL|WEBRip|HDRip|BDRip|HDTV|DVD|HDDVD|DVDRip|WEB\.DL|WEB\.Rip|Blu-Ray|CAM|CAMRip|TS|TC)(?:\b|_)`)
	// 发布组：文件名末尾 [Group] 或 -Group
	groupBracketRe = regexp.MustCompile(`[\[\(]([^\]\)]+)[\]\)]\s*$`)
	groupDashRe    = regexp.MustCompile(`[-_]([A-Za-z0-9]+)$`)
	// 季集：S01E02 / s01e02e03 / S01E02-E03 / 1x02 / 第N集 / 第X季第N集 / 纯 E192
	// 纯 E 形式（凡人修仙传.E192）在国产长番里很常见，缺它会导致 Episode=0 → 误判 movie。
	// 新分支追加在末尾（组 11/12），不能插在中间，否则下面按 se[N] 下标取值的逻辑全部错位。
	seasonEpisodeRe = regexp.MustCompile(`(?i)(?:^|[\s\.\-_\[(])([Ss])(\d{1,2})(?:\s*[Ee]\s*(\d{1,3}))?(?:([-—])\s*[Ee]\s*(\d{1,3}))?|(\d{1,2})[xX](\d{1,3})|第\s*(\d{1,2})\s*季\s*第\s*(\d{1,3})\s*集|第\s*(\d{1,3})\s*集|(?:^|[\s\.\-_\[(])[Ee]\s*(\d{1,3})(?:\s|\.|\-|_|\]|\)|$)`)
	// Season 1 形式
	seasonWordRe = regexp.MustCompile(`(?i)\bSeason\s+(\d{1,2})\b`)
	// 年份：四位数年份
	yearRe = regexp.MustCompile(`(?:\(|\[|\s|\.|\-|_|^)(\d{4})(?:\)|\]|\s|\.|\-|_|$)`)
	// 视频编码（允许 H 265 / H.265 / H265 空格或点变体）
	videoCodecRe = regexp.MustCompile(`(?i)(?:\b|_)(HEVC|H[.]? ?265|x265|H[.]? ?264|x264|AVC|AV1)(?:\b|_)`)
	// TMDB ID 标记 {tmdb-1280738} / [tmdbid-271016]
	tmdbIDRe = regexp.MustCompile(`(?i)[{\[]tmdb(?:id)?(?:=|-)(\d+)[}\]]`)
	// 纯季目录名
	seasonDirRe = regexp.MustCompile(`(?i)^(?:season\s*\d+|s\d{1,2}(?:e\d{1,3})?|specials?|extras?|bonus|sample)$`)
	// 质量标记：2160p.WEB-DL.HQ.H.265 等片源之后的 HQ/UHD，残留会导致 TMDB 搜索 0 结果而落入"未分类"
	qualityTagRe = regexp.MustCompile(`(?i)(?:^|[.\s\-_\[(])(HQ|UHD)(?:$|[.\s\-_)\]])`)
	// 标题附加剥离：音频编码（复用 format_parser.audioCodecRe）、声道、帧率，及分辨率与片源之间的发布组段
	// （如 2160p.Knowledge Network.WEB-DL）。残留这些标记词会导致 TMDB 搜索 0 结果而落入"未分类"。
	channelRe = regexp.MustCompile(`(?i)(?:\b|\.)(?:5[.\s]?1|7[.\s]?1|2[.\s]?0|3[.\s]?1|6[.\s]?1)(?:\b|\.)`)
	fpsRe     = regexp.MustCompile(`(?i)(?:\b|\.)\d{2,3}\s?fps(?:\b|\.)`)
	// ponytail: 剥「分辨率.发布组.片源」中段，仅限纯英文、无数字、≤30 字符；非标命名把标题词夹在中段时可能误剥，
	// 退化为按主标题搜索（最坏年份错配到同名前作），可接受。中间有两个点分隔 token 时不剥。
	groupMidRe = regexp.MustCompile(`(?i)(?:^|[.\s-])(?:2160p|1080p|720p|480p|4320p|4k|8k)[.\s-]([A-Za-z][A-Za-z ]{0,28}[A-Za-z]|[A-Za-z])[.\s-](?:WEB-DL|WEBRip|BluRay|REMUX|HDTV|BDRip|HDRip|DVD|HDDVD|DVDRip|WEB\.DL|WEB\.Rip|Blu-Ray)(?:[.\s-]|$)`)
)

// 父目录名清洗正则
var (
	dirNameYearRe     = regexp.MustCompile(`[(（]\s*(\d{4})\s*[)）]`)
	dirNameSeasonCNRe = regexp.MustCompile(`第\s*(\d+|[一二三四五六七八九十]+)\s*[季部]`)
	dirNameSeasonENRe = regexp.MustCompile(`(?i)\b(?:Season|S)\s*(\d{1,2})\b`)
	dirDecorationRe   = regexp.MustCompile(`(?i)更新至\s*第?\s*\d+\s*(?:-\s*\d+)?\s*集|全\s*\d+\s*集|完结|第\s*(?:\d+|[一二三四五六七八九十]+)\s*[季部]|\b(?:Season|S)\s*\d{1,2}\b|[(（]\s*\d{4}\s*[)）]`)
)

// recognizeByRegex 用正则解析文件名，输出 MetaInfo。
func recognizeByRegex(baseName string, expectedType string) MetaInfo {
	m := MetaInfo{RawName: baseName}
	orig := baseName

	// 解析 TMDB ID
	if tm := tmdbIDRe.FindStringSubmatch(orig); tm != nil {
		m.TMDBID, _ = strconv.Atoi(tm[1])
		baseName = tmdbIDRe.ReplaceAllString(baseName, " ")
	}

	// 解析年份（优先括号内，其次自由年份）
	yearMatch := yearRe.FindStringSubmatch(baseName)
	var yearStr string
	if yearMatch != nil {
		yearStr = yearMatch[1]
		m.Year, _ = strconv.Atoi(yearStr)
	}

	// 解析季集（SxxExx / SxxExxExx / Season x / xxYxx）
	se := seasonEpisodeRe.FindStringSubmatch(baseName)
	if se != nil {
		if se[1] != "" { // Sxx[Exx]
			m.Season, _ = strconv.Atoi(se[2])
			if se[3] != "" {
				m.Episode, _ = strconv.Atoi(se[3])
			}
			if se[5] != "" { // S01E01-E02 范围
				e2, _ := strconv.Atoi(se[5])
				for e := m.Episode; e <= e2; e++ {
					m.Episodes = append(m.Episodes, e)
				}
			}
		} else if se[6] != "" { // 1x02
			m.Season, _ = strconv.Atoi(se[6])
			m.Episode, _ = strconv.Atoi(se[7])
		} else if se[8] != "" { // 第X季第N集
			m.Season, _ = strconv.Atoi(se[8])
			m.Episode, _ = strconv.Atoi(se[9])
		} else if se[10] != "" { // 第N集：无季信息，按单季第 1 季处理
			m.Season = 1
			m.Episode, _ = strconv.Atoi(se[10])
		} else if se[11] != "" { // 纯 E192：无季信息，同理按第 1 季
			m.Season = 1
			m.Episode, _ = strconv.Atoi(se[11])
		}
	} else if sw := seasonWordRe.FindStringSubmatch(baseName); sw != nil {
		m.Season, _ = strconv.Atoi(sw[1])
	}

	// 解析分辨率
	if rm := resRe.FindStringSubmatch(baseName); rm != nil {
		res := strings.ToLower(rm[1])
		if res == "4k" {
			res = "2160p"
		} else if res == "8k" {
			res = "4320p"
		}
		m.Resolution = res
	}

	// 解析来源
	if sm := sourceRe.FindStringSubmatch(baseName); sm != nil {
		src := sm[1]
		if src == "WEB.DL" {
			src = "WEB-DL"
		}
		if src == "WEB.Rip" {
			src = "WEBRip"
		}
		if src == "Blu-Ray" {
			src = "BluRay"
		}
		m.Source = src
	}

	// 解析发布组
	if gb := groupBracketRe.FindStringSubmatch(orig); gb != nil {
		m.ReleaseGroup = gb[1]
	} else if gd := groupDashRe.FindStringSubmatch(orig); gd != nil && !isYearOrSeason(gd[1]) {
		m.ReleaseGroup = gd[1]
	}

	// 判断类型
	m.Type = guessType(m, expectedType)

	// 提取标题：去掉已解析片段后剩余部分
	m.Name = cleanTitle(extractTitle(baseName, m, yearStr))

	// 容器
	container := strings.ToLower(strings.TrimPrefix(extOf(orig), "."))
	if container != "" && VideoExtensions["."+container] {
		m.Container = container
	}
	_ = log.Printf
	return m
}

func isYearOrSeason(s string) bool {
	if _, err := strconv.Atoi(s); err == nil {
		return len(s) == 4
	}
	ls := strings.ToLower(s)
	if strings.HasPrefix(ls, "season") {
		return true
	}
	// 仅当是 s+数字（如 s01）才算季标；"SCOPE"/"STAR"/"SIZE" 等 s 开头词不是季，
	// 否则会被误判导致发布组不剥、残留进 TMDB 查询标题
	return len(ls) >= 2 && ls[0] == 's' && ls[1] >= '0' && ls[1] <= '9'
}

// guessType 判断媒体类型。
func guessType(m MetaInfo, expectedType string) string {
	if expectedType == "movie" {
		return "movie"
	}
	if expectedType == "tv" {
		return "tv"
	}
	if m.Season > 0 || m.Episode > 0 {
		return "tv"
	}
	return "movie"
}

// extractTitle 提取标题：优先用结构锚点（年份/季集）截断，无锚点时退回标签剥离法。
func extractTitle(baseName string, m MetaInfo, yearStr string) string {
	if name, ok := cutByAnchor(baseName, yearStr, m); ok {
		return name
	}
	return stripTitleTags(baseName, m, yearStr)
}

// cutByAnchor 稳健主路径：文件名结构恒为「标题·年份(或季集)·其后全是媒体标签」，
// 取第一个「年份或季集」锚点之前的片段作为标题。这样新增任意媒体标签（CAM/H 265/SCOPE…）
// 都不必再维护剥离正则。锚点在开头或缺失时返回 ok=false 交给降级路径。
func cutByAnchor(s string, yearStr string, m MetaInfo) (string, bool) {
	cut := -1
	if yearStr != "" {
		if mm := yearRe.FindStringSubmatchIndex(s); mm != nil && len(mm) >= 4 && mm[2] >= 0 {
			cut = mm[2]
		}
	}
	if se := seasonEpisodeRe.FindStringIndex(s); se != nil && (cut < 0 || se[0] < cut) {
		cut = se[0]
	}
	if sw := seasonWordRe.FindStringIndex(s); sw != nil && (cut < 0 || sw[0] < cut) {
		cut = sw[0]
	}
	if cut <= 0 {
		return "", false
	}
	head := cleanTitle(s[:cut])
	if head == "" {
		return "", false
	}
	return head, true
}

// stripTitleTags 降级路径：文件名没有年份/季集锚点时，用标签剥离法抹除媒体片段。
func stripTitleTags(baseName string, m MetaInfo, yearStr string) string {
	s := baseName
	// 去掉季集片段
	s = seasonEpisodeRe.ReplaceAllString(s, " ")
	s = seasonWordRe.ReplaceAllString(s, " ")
	// 发布组段（分辨率与片源之间）：在分辨率/片源被移除前、点结构完整时执行
	s = groupMidRe.ReplaceAllString(s, " ")
	// 音频编码/声道/帧率
	s = audioCodecRe.ReplaceAllString(s, " ")
	s = channelRe.ReplaceAllString(s, " ")
	s = fpsRe.ReplaceAllString(s, " ")
	// 效果词（HDR/SDR/DoVi 等）只作版本标签，残留会导致 TMDB 搜索 0 结果
	s = effectRe.ReplaceAllString(s, " ")
	// 去掉分辨率/来源
	s = resRe.ReplaceAllString(s, " ")
	s = sourceRe.ReplaceAllString(s, " ")
	s = videoCodecRe.ReplaceAllString(s, " ")
	// 质量标记（片源之后的 HQ/UHD，如 2160p.WEB-DL.HQ.H.265）
	s = qualityTagRe.ReplaceAllString(s, " ")
	// 去掉年份（含括号）
	if yearStr != "" {
		yrRe := regexp.MustCompile(`(?i)(?:\(|\[|\s|\.|\-|_)` + yearStr + `(?:\)|\]|\s|\.|\-|_|$)`)
		s = yrRe.ReplaceAllString(s, " ")
		s = regexp.MustCompile(`[\(\[]\s*[\)\]]`).ReplaceAllString(s, " ")
	}
	// 去掉 [xx] 装饰段
	s = regexp.MustCompile(`(?i)\[[^\[\]]*\]`).ReplaceAllString(s, " ")
	// 去掉发布组后缀 -Group
	if m.ReleaseGroup != "" {
		rg := regexp.MustCompile(`(?i)[-\s_]*` + regexp.QuoteMeta(m.ReleaseGroup) + `\s*$`)
		s = rg.ReplaceAllString(s, " ")
	}
	return cleanTitle(s)
}

// recognizeByFallback guessit 失败时的降级识别：简单正则提取标题和年份。
func recognizeByFallback(filename, baseName string) MetaInfo {
	m := MetaInfo{RawName: filename}
	re := regexp.MustCompile(`^(.+?)[\s\.\-_]?\(?(\d{4})\)?`)
	if mm := re.FindStringSubmatch(baseName); mm != nil {
		m.Name = cleanTitle(mm[1])
		if mm[2] != "" {
			m.Year, _ = strconv.Atoi(mm[2])
		}
		sm := regexp.MustCompile(`(?i)[Ss](\d{1,2})`).FindStringSubmatch(baseName)
		em := regexp.MustCompile(`(?i)[Ee](\d{1,3})`).FindStringSubmatch(baseName)
		if sm != nil {
			m.Season, _ = strconv.Atoi(sm[1])
		}
		if em != nil {
			m.Episode, _ = strconv.Atoi(em[1])
		}
		if m.Season > 0 || m.Episode > 0 {
			m.Type = "tv"
		} else {
			m.Type = "movie"
		}
		return m
	}
	return MetaInfo{Name: cleanTitle(baseName), RawName: filename}
}

// recognizeByName 识别单个字符串，输出标准化 MetaInfo。
func recognizeByName(filename, expectedType string) MetaInfo {
	if filename == "" {
		return MetaInfo{}
	}
	baseName := stripExt(filename)
	m := recognizeByRegex(baseName, expectedType)
	if m.Name == "" {
		m = recognizeByFallback(filename, baseName)
	} else {
		// 与 Python 原版 raw_name=filename 一致：RawName 保留完整文件名（含扩展名）。
		// 否则模板 {{fileExt}} 从无扩展名的 baseName 提取会得到错误片段（如 "Atmos {tmdbid-246246}"），
		// 导致 target_path 缺真实扩展名，整理联动 STRM 按文件名匹配失败。
		m.RawName = filename
	}
	return m
}

// cleanDirName 清洗父目录名，剥离装饰片段，提取纯剧名 + 年份 + 季数。
func cleanDirName(dirName string) (string, int, int) {
	if dirName == "" {
		return "", 0, 0
	}
	cleaned := dirName
	var year, season int
	if ym := dirNameYearRe.FindStringSubmatch(cleaned); ym != nil {
		year, _ = strconv.Atoi(ym[1])
	}
	if sm := dirNameSeasonCNRe.FindStringSubmatch(cleaned); sm != nil {
		season, _ = strconv.Atoi(sm[1])
	}
	if season == 0 {
		if sm := dirNameSeasonENRe.FindStringSubmatch(cleaned); sm != nil {
			season, _ = strconv.Atoi(sm[1])
		}
	}
	cleaned = dirDecorationRe.ReplaceAllString(cleaned, "")
	cleaned = sepRe.ReplaceAllString(cleaned, " ")
	cleaned = spaceRe.ReplaceAllString(cleaned, " ")
	cleaned = strings.Trim(cleaned, " -_.")
	return cleaned, year, season
}

// Recognize 识别文件名，输出标准化 MetaInfo。parentDirs 为从顶层到直接父目录的目录名列表。
func Recognize(filename, expectedType string, parentDirs []string) MetaInfo {
	meta := recognizeByName(filename, expectedType)

	// 从文件名/父目录名提取 TMDB ID
	if meta.TMDBID == 0 {
		if tm := tmdbIDRe.FindStringSubmatch(meta.RawName); tm != nil {
			meta.TMDBID, _ = strconv.Atoi(tm[1])
		}
	}
	if meta.TMDBID == 0 && len(parentDirs) > 0 {
		for i := len(parentDirs) - 1; i >= 0; i-- {
			if tm := tmdbIDRe.FindStringSubmatch(parentDirs[i]); tm != nil {
				meta.TMDBID, _ = strconv.Atoi(tm[1])
				break
			}
		}
	}

	// 电视剧季信息缺失时默认第 1 季
	if meta.Type == "tv" && meta.Episode > 0 && meta.Season == 0 {
		meta.Season = 1
	}

	// 标题为空 / 纯数字 / 纯季集编号（S01E181、E181）时，用父目录名辅助识别
	if (meta.Name == "" || isAllDigits(meta.Name) || looksLikeEpisodeCode(meta.Name)) && len(parentDirs) > 0 {
		for i := len(parentDirs) - 1; i >= 0; i-- {
			dirName := parentDirs[i]
			if dirName == "" {
				continue
			}
			if seasonDirRe.MatchString(strings.TrimSpace(dirName)) {
				continue
			}
			cleanName, dirYear, dirSeason := cleanDirName(dirName)
			if cleanName == "" {
				continue
			}
			dirMeta := recognizeByName(cleanName, expectedType)
			if dirMeta.Name != "" && !isAllDigits(dirMeta.Name) {
				meta.Name = dirMeta.Name
				if dirYear > 0 {
					meta.Year = dirYear
				} else if dirMeta.Year > 0 {
					meta.Year = dirMeta.Year
				}
				if dirMeta.Type != "" && meta.Type == "" {
					meta.Type = dirMeta.Type
				}
				if dirSeason > 0 {
					meta.Season = dirSeason
				} else if dirMeta.Season > 0 {
					meta.Season = dirMeta.Season
				}
				if meta.Episode == 0 {
					base := stripExt(filename)
					if nm := regexp.MustCompile(`(\d+)`).FindStringSubmatch(base); nm != nil {
						meta.Episode, _ = strconv.Atoi(nm[1])
					}
				}
				// 有季号即剧集：dirSeason 从父目录提取到但此前只写入 meta.Season，
				// 定类型却只看 Episode，导致「剧名 S01」这类纯季号目录被判成 movie。
				if meta.Episode > 0 || meta.Season > 0 {
					meta.Type = "tv"
					if meta.Season == 0 {
						meta.Season = 1
					}
				}
				log.Printf("父目录辅助识别: '%s' -> 标题='%s' 季=%d 集=%d (来自目录 '%s')",
					filename, meta.Name, meta.Season, meta.Episode, dirName)
				break
			}
		}
	}

	return meta
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// looksLikeEpisodeCode 判断字符串是否只是季集编号片段（如 S01E181、E181、S01），不是真正的标题。
var episodeCodeRe = regexp.MustCompile(`(?i)^[Ss]?\d{1,3}[Ee]\d{1,3}$|^[Ee]\d{1,3}$|^[Ss]\d{1,3}$`)

func looksLikeEpisodeCode(s string) bool {
	return episodeCodeRe.MatchString(strings.TrimSpace(s))
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
