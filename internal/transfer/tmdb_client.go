package transfer

// TMDB API 客户端（对应 tmdb_client.py）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/httpx"
)

const (
	tmdbImageBaseURL   = "https://image.tmdb.org/t/p/"
	tmdbAPIBaseURL     = "https://api.themoviedb.org/3"
	defaultPosterSize  = "w500"
	tmdbSearchCacheMax = 512
)

// TmdbClient TMDB API 客户端。
type TmdbClient struct {
	APIKey   string
	Language string
	http     *httpx.Client
	// searchCache 标题搜索结果缓存：同一标题（整部剧 43 集）只搜一次。
	mu          sync.Mutex
	searchCache map[string]*MediaInfo
}

// NewTmdbClient 创建 TMDB 客户端。
func NewTmdbClient(apiKey, language string) (*TmdbClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("TMDB API Key 不能为空")
	}

	if language == "" {
		language = "zh-CN"
	}
	c := &TmdbClient{APIKey: apiKey, Language: language, http: httpx.New(15 * time.Second), searchCache: map[string]*MediaInfo{}}
	return c, nil
}

func (t *TmdbClient) cacheSearchResult(key string, media *MediaInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.searchCache) >= tmdbSearchCacheMax {
		for oldest := range t.searchCache {
			delete(t.searchCache, oldest)
			break
		}
	}
	t.searchCache[key] = media
}

// searchResult 搜索结果项。
type searchResult struct {
	ID               int     `json:"id"`
	Title            string  `json:"title"`
	Name             string  `json:"name"`
	OriginalTitle    string  `json:"original_title"`
	OriginalName     string  `json:"original_name"`
	ReleaseDate      string  `json:"release_date"`
	FirstAirDate     string  `json:"first_air_date"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	Overview         string  `json:"overview"`
	VoteAverage      float64 `json:"vote_average"`
	OriginalLanguage string  `json:"original_language"`
	GenreIDs         []int   `json:"genre_ids"`
}

// Search 搜索媒体，返回第一个匹配的 MediaInfo。
func (t *TmdbClient) Search(name string, year int, mtype string) *MediaInfo {
	if name == "" {
		return nil
	}
	cacheKey := mtype + "|" + name + "|" + strconv.Itoa(year)
	t.mu.Lock()
	if cached, ok := t.searchCache[cacheKey]; ok {
		t.mu.Unlock()
		return cached
	}
	t.mu.Unlock()

	path := "/search/movie"
	if mtype == "tv" {
		path = "/search/tv"
	}
	results, err := t.getList(context.Background(), path, map[string]string{"query": name})
	if err != nil || len(results) == 0 {
		if err != nil {
			log.Printf("TMDB 搜索失败 (%s, %d, %s): %v", name, year, mtype, err)
		}
		// 意译标题降级：文件名可能是"The Story of/by X"类意译，TMDB 查不到时剥掉修饰词用核心名再搜（须年份匹配防错配）
		if d := t.searchFallback(name, year, mtype); d != nil {
			t.cacheSearchResult(cacheKey, d)
			return d
		}
		log.Printf("TMDB 搜索无结果: %s (%d)", name, year)
		return nil
	}

	nameLower := strings.ToLower(strings.TrimSpace(name))
	var selected *searchResult

	// 1. 名称完全匹配 + 年份匹配
	if year > 0 {
		for i := range results {
			r := &results[i]
			rn := strings.ToLower(strings.TrimSpace(rTitle(r)))
			ro := strings.ToLower(strings.TrimSpace(rOrig(r)))
			release := rDate(r)
			if release != "" && strings.Contains(release, strconv.Itoa(year)) && (rn == nameLower || ro == nameLower) {
				selected = r
				log.Printf("TMDB 精确匹配(名称+年份): %s (id=%d)", rTitle(r), r.ID)
				break
			}
		}
	}
	// 2. 名称完全匹配
	if selected == nil {
		for i := range results {
			r := &results[i]
			rn := strings.ToLower(strings.TrimSpace(rTitle(r)))
			ro := strings.ToLower(strings.TrimSpace(rOrig(r)))
			if rn == nameLower || ro == nameLower {
				selected = r
				log.Printf("TMDB 精确匹配(名称): %s (id=%d)", rTitle(r), r.ID)
				break
			}
		}
	}
	// 3. 年份匹配
	if selected == nil && year > 0 {
		for i := range results {
			r := &results[i]
			release := rDate(r)
			if release != "" && strings.Contains(release, strconv.Itoa(year)) {
				selected = r
				log.Printf("TMDB 年份匹配: %s (id=%d)", rTitle(r), r.ID)
				break
			}
		}
	}
	// 4. 名称与年份均未命中：不取第一个结果。
	// 此前的「兜底取 results[0]」会把「搜到了但都不像」当成「搜到了」——
	// 中文名撞词时（如「挑情丑闻」命中《丑闻》）必然错配到热门片，
	// 后续按错误 id 走去重，还会把同季其余剧集当劣版删除。
	// 返回 nil 交给上层：父目录重试 → AI 兜底 → 识别失败留在原地。
	if selected == nil {
		log.Printf("TMDB 名称年份均不匹配: %s (%d)", name, year)
		return nil
	}

	if selected.ID == 0 {
		return nil
	}
	detail := t.GetDetail(selected.ID, mtype)
	if detail != nil {
		t.cacheSearchResult(cacheKey, detail)
		return detail
	}
	// 降级构造
	fallbackTitle := rTitle(selected)
	release := rDate(selected)
	fallbackYear := 0
	if len(release) >= 4 {
		fallbackYear, _ = strconv.Atoi(release[:4])
	}
	log.Printf("TMDB 获取详情失败，用搜索结果降级构造: %s (id=%d)", fallbackTitle, selected.ID)
	fb := &MediaInfo{Title: fallbackTitle, Year: fallbackYear, Type: mtype, TMDBID: selected.ID}
	t.cacheSearchResult(cacheKey, fb)
	return fb
}

// 搜索结果标题/原名/日期辅助（包级，Search 与 searchFallback 共用）。
func rTitle(r *searchResult) string {
	s := strings.TrimSpace(r.Title)
	if s == "" {
		s = strings.TrimSpace(r.Name)
	}
	return s
}

func rOrig(r *searchResult) string {
	s := strings.TrimSpace(r.OriginalTitle)
	if s == "" {
		s = strings.TrimSpace(r.OriginalName)
	}
	return s
}

func rDate(r *searchResult) string {
	if r.ReleaseDate != "" {
		return r.ReleaseDate
	}
	return r.FirstAirDate
}

// 意译标题修饰词：The Story of X / The Story by X / A Tale of X 等，TMDB 上正式标题可能不含这些前缀。
var titleQualifierRe = regexp.MustCompile(`(?i)^(?:the\s+)?(?:story|tale|legend|chronicles?|saga)\s+(?:of|by)\s+(.+)$`)

// searchFallback 原始标题搜索无结果时，剥掉意译修饰词（The Story of/by X -> X）用核心名再搜。
// 仅在年份匹配或标题含核心名时采用，避免"Story Water Margin"错配到 1992 香港版同名的坑。
func (t *TmdbClient) searchFallback(name string, year int, mtype string) *MediaInfo {
	mm := titleQualifierRe.FindStringSubmatch(strings.TrimSpace(name))
	if mm == nil {
		return nil
	}
	core := strings.TrimSpace(mm[1])
	if core == "" || strings.EqualFold(core, name) {
		return nil
	}
	path := "/search/movie"
	if mtype == "tv" {
		path = "/search/tv"
	}
	results, err := t.getList(context.Background(), path, map[string]string{"query": core})
	if err != nil || len(results) == 0 {
		if err != nil {
			log.Printf("TMDB 意译降级搜索失败 (%s -> %s): %v", name, core, err)
		} else {
			log.Printf("TMDB 意译降级搜索无结果: %s -> %s", name, core)
		}
		return nil
	}
	coreLower := strings.ToLower(core)
	sel := pickFallbackResult(results, coreLower, year, name)
	if sel == nil {
		log.Printf("TMDB 意译降级无匹配: %s -> %s", name, core)
		return nil
	}
	return t.detailOrFallback(sel, mtype)
}

// pickFallbackResult 从降级搜索结果中选匹配项：年份匹配优先（防同名错配，如 1992 vs 1998 水浒传），其次标题完全相等或含核心名。
func pickFallbackResult(results []searchResult, coreLower string, year int, origName string) *searchResult {
	// 第一遍：年份匹配（强约束，优先级最高）
	if year > 0 {
		for i := range results {
			r := &results[i]
			if release := rDate(r); release != "" && strings.Contains(release, strconv.Itoa(year)) {
				log.Printf("TMDB 意译降级命中(年份): %s -> %s (id=%d)", origName, rTitle(r), r.ID)
				return r
			}
		}
	}
	// 第二遍：标题完全相等或含核心名（无年份时回退）
	for i := range results {
		r := &results[i]
		rn := strings.ToLower(strings.TrimSpace(rTitle(r)))
		ro := strings.ToLower(strings.TrimSpace(rOrig(r)))
		if rn == coreLower || ro == coreLower || strings.Contains(rn, coreLower) || strings.Contains(coreLower, rn) {
			log.Printf("TMDB 意译降级命中(名称): %s -> %s (id=%d)", origName, rTitle(r), r.ID)
			return r
		}
	}
	return nil
}

// detailOrFallback 取搜索结果详情，失败时降级构造。
func (t *TmdbClient) detailOrFallback(selected *searchResult, mtype string) *MediaInfo {
	detail := t.GetDetail(selected.ID, mtype)
	if detail != nil {
		return detail
	}
	fallbackTitle := rTitle(selected)
	release := rDate(selected)
	fallbackYear := 0
	if len(release) >= 4 {
		fallbackYear, _ = strconv.Atoi(release[:4])
	}
	log.Printf("TMDB 获取详情失败，用搜索结果降级构造: %s (id=%d)", fallbackTitle, selected.ID)
	return &MediaInfo{Title: fallbackTitle, Year: fallbackYear, Type: mtype, TMDBID: selected.ID}
}

// detailResponse TMDB 详情响应。
type detailResponse struct {
	ID                  int      `json:"id"`
	Title               string   `json:"title"`
	Name                string   `json:"name"`
	OriginalTitle       string   `json:"original_title"`
	OriginalName        string   `json:"original_name"`
	ReleaseDate         string   `json:"release_date"`
	FirstAirDate        string   `json:"first_air_date"`
	OriginCountry       []string `json:"origin_country"`
	ProductionCountries []struct {
		ISO3166_1 string `json:"iso_3166_1"`
		Name      string `json:"name"`
	} `json:"production_countries"`
	NumberOfEpisodes int `json:"number_of_episodes"`
	NumberOfSeasons  int `json:"number_of_seasons"`
	Genres           []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"genres"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	IMDBID           string  `json:"imdb_id"`
	OriginalLanguage string  `json:"original_language"`
	Runtime          int     `json:"runtime"`
	VoteAverage      float64 `json:"vote_average"`
}

// GetDetail 获取媒体详情。
func (t *TmdbClient) GetDetail(tmdbID int, mtype string) *MediaInfo {
	if mtype == "" {
		mtype = "movie"
	}
	data, err := t.getJSON(context.Background(), "/"+mtype+"/"+strconv.Itoa(tmdbID), nil)
	if err != nil {
		log.Printf("TMDB 获取详情失败 (tmdb_id=%d, type=%s): %v", tmdbID, mtype, err)
		return nil
	}
	var d detailResponse
	if err := json.Unmarshal(data, &d); err != nil {
		log.Printf("TMDB 解析详情失败 (tmdb_id=%d): %v", tmdbID, err)
		return nil
	}

	title := d.Title
	if title == "" {
		title = d.Name
	}
	if title == "" {
		title = d.OriginalTitle
	}
	if title == "" {
		title = d.OriginalName
	}
	release := d.ReleaseDate
	if release == "" {
		release = d.FirstAirDate
	}
	year := 0
	if len(release) >= 4 {
		year, _ = strconv.Atoi(release[:4])
	}
	origTitle := d.OriginalTitle
	if origTitle == "" {
		origTitle = d.OriginalName
	}

	mi := &MediaInfo{
		Title:            title,
		Year:             year,
		Type:             mtype,
		TMDBID:           tmdbID,
		IMDBID:           d.IMDBID,
		OriginalTitle:    origTitle,
		OriginalLanguage: d.OriginalLanguage,
		NumberOfEpisodes: d.NumberOfEpisodes,
		NumberOfSeasons:  d.NumberOfSeasons,
		Overview:         d.Overview,
		PosterPath:       d.PosterPath,
		BackdropPath:     d.BackdropPath,
		Runtime:          d.Runtime,
		VoteAverage:      d.VoteAverage,
	}
	if mtype == "tv" {
		mi.OriginCountry = d.OriginCountry
	} else {
		for _, pc := range d.ProductionCountries {
			if pc.ISO3166_1 != "" {
				mi.ProductionCountries = append(mi.ProductionCountries, pc.ISO3166_1)
			}
		}
	}
	for _, g := range d.Genres {
		if g.ID > 0 {
			mi.GenreIDs = append(mi.GenreIDs, g.ID)
		}
	}
	return mi
}

// EpisodeInfo 单集信息。
type EpisodeInfo struct {
	EpisodeNumber int
	Name          string
	Overview      string
	StillPath     string
	AirDate       string
}

// GetEpisodes 获取某季的所有集信息。
func (t *TmdbClient) GetEpisodes(tmdbID, season int) []EpisodeInfo {
	data, err := t.getJSON(context.Background(), "/tv/"+strconv.Itoa(tmdbID)+"/season/"+strconv.Itoa(season), nil)
	if err != nil {
		log.Printf("TMDB 获取剧集失败 (tmdb_id=%d, season=%d): %v", tmdbID, season, err)
		return nil
	}
	var d struct {
		Episodes []struct {
			EpisodeNumber int    `json:"episode_number"`
			Name          string `json:"name"`
			Overview      string `json:"overview"`
			StillPath     string `json:"still_path"`
			AirDate       string `json:"air_date"`
		} `json:"episodes"`
	}
	_ = json.Unmarshal(data, &d)
	var out []EpisodeInfo
	for _, ep := range d.Episodes {
		out = append(out, EpisodeInfo{
			EpisodeNumber: ep.EpisodeNumber,
			Name:          ep.Name,
			Overview:      ep.Overview,
			StillPath:     ep.StillPath,
			AirDate:       ep.AirDate,
		})
	}
	return out
}

// getList 请求列表接口，返回 results。
func (t *TmdbClient) getList(ctx context.Context, path string, params map[string]string) ([]searchResult, error) {
	data, err := t.getJSON(ctx, path, params)
	if err != nil {
		return nil, err
	}
	var r struct {
		Results []searchResult `json:"results"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return r.Results, nil
}

// getJSON 请求 TMDB 接口并返回完整 JSON。
func (t *TmdbClient) getJSON(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := url.Values{}
	q.Set("api_key", t.APIKey)
	q.Set("language", t.Language)
	for k, v := range params {
		q.Set(k, v)
	}
	rawURL := tmdbAPIBaseURL + path + "?" + q.Encode()
	raw, status, err := t.http.Get(ctx, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("TMDB HTTP %d: %s", status, string(raw))
	}
	return raw, nil
}

// DownloadImage 下载 TMDB 图片到本地。
func (t *TmdbClient) DownloadImage(imagePath, savePath, size string) bool {
	if imagePath == "" {
		return false
	}
	if size == "" {
		size = defaultPosterSize
	}
	rawURL := tmdbImageBaseURL + size + imagePath
	resp, err := t.http.Req(context.Background(), http.MethodGet, rawURL, nil, nil)
	if err != nil {
		log.Printf("下载 TMDB 图片失败 (%s): %v", rawURL, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("下载 TMDB 图片失败 (%s): HTTP %d", rawURL, resp.StatusCode)
		return false
	}
	data, err := httpx.ReadBody(resp)
	if err != nil {
		log.Printf("下载 TMDB 图片失败 (%s): %v", rawURL, err)
		return false
	}
	if err := os.MkdirAll(dirOf(savePath), 0o755); err == nil {
		if err := os.WriteFile(savePath, data, 0o644); err == nil {
			return true
		}
	}
	log.Printf("保存 TMDB 图片失败: %s", savePath)
	return false
}

func dirOf(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "."
	}
	return p[:i]
}
