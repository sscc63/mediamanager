package transfer

// TMDB 榜单/搜索/详情/日历接口（对应 tmdb_client.py 的 get_trending/get_now_playing/get_top_rated/get_upcoming/search_media/get_detail_dict）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// TmdbListItem TMDB 列表项（前端友好的统一结构）。
type TmdbListItem struct {
	ID               int     `json:"id"`
	Title            string  `json:"title"`
	MediaType        string  `json:"media_type"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	Overview         string  `json:"overview"`
	VoteAverage      float64 `json:"vote_average"`
	ReleaseDate      string  `json:"release_date"`
	OriginalLanguage string  `json:"original_language"`
	GenreIDs         []int   `json:"genre_ids"`
}

// TmdbCastMember 演职员信息。
type TmdbCastMember struct {
	Name        string `json:"name"`
	Character   string `json:"character"`
	ProfilePath string `json:"profile_path"`
}

// TmdbDetail TMDB 完整详情（含演职员/中文译名），供前端弹窗展示。
type TmdbDetail struct {
	ID                  int              `json:"id"`
	Title               string           `json:"title"`
	TitleZh             string           `json:"title_zh"`
	OriginalTitle       string           `json:"original_title"`
	MediaType           string           `json:"media_type"`
	Overview            string           `json:"overview"`
	PosterPath          string           `json:"poster_path"`
	BackdropPath        string           `json:"backdrop_path"`
	LogoPath            string           `json:"logo_path"`
	TextlessPosterPath  string           `json:"textless_poster_path"`
	VoteAverage         float64          `json:"vote_average"`
	VoteCount           int              `json:"vote_count"`
	ReleaseDate         string           `json:"release_date"`
	Genres              []string         `json:"genres"`
	OriginalLanguage    string           `json:"original_language"`
	ProductionCountries []string         `json:"production_countries"`
	Status              string           `json:"status"`
	Homepage            string           `json:"homepage"`
	Runtime             int              `json:"runtime"`
	NumberOfEpisodes    int              `json:"number_of_episodes"`
	NumberOfSeasons     int              `json:"number_of_seasons"`
	Directors           []string         `json:"directors"`
	Creators            []string         `json:"creators"`
	Cast                []TmdbCastMember `json:"cast"`
}

// tmdbListRaw TMDB 列表接口原始项（电影/剧集字段兼容）。
type tmdbListRaw struct {
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

// tmdbDetailRaw TMDB 详情接口原始响应。
type tmdbDetailRaw struct {
	ID               int     `json:"id"`
	Title            string  `json:"title"`
	Name             string  `json:"name"`
	OriginalTitle    string  `json:"original_title"`
	OriginalName     string  `json:"original_name"`
	ReleaseDate      string  `json:"release_date"`
	FirstAirDate     string  `json:"first_air_date"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	VoteAverage      float64 `json:"vote_average"`
	VoteCount        int     `json:"vote_count"`
	OriginalLanguage string  `json:"original_language"`
	Status           string  `json:"status"`
	Homepage         string  `json:"homepage"`
	Runtime          int     `json:"runtime"`
	NumberOfEpisodes int     `json:"number_of_episodes"`
	NumberOfSeasons  int     `json:"number_of_seasons"`
	CreatedBy        []struct {
		Name string `json:"name"`
	} `json:"created_by"`
	Genres []struct {
		Name string `json:"name"`
	} `json:"genres"`
	ProductionCountries []struct {
		Name string `json:"name"`
	} `json:"production_countries"`
	Credits struct {
		Cast []struct {
			Name        string `json:"name"`
			Character   string `json:"character"`
			ProfilePath string `json:"profile_path"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
		} `json:"crew"`
	} `json:"credits"`
	Images struct {
		Logos []struct {
			FilePath string `json:"file_path"`
			ISO6391  string `json:"iso_639_1"`
		} `json:"logos"`
	} `json:"images"`
}

func selectTmdbLogo(logos []struct {
	FilePath string `json:"file_path"`
	ISO6391  string `json:"iso_639_1"`
}) string {
	for _, language := range []string{"zh", "en", ""} {
		for _, logo := range logos {
			if logo.FilePath != "" && logo.ISO6391 == language {
				return logo.FilePath
			}
		}
	}
	return ""
}

// GetTrending 获取 TMDB 趋势榜单（最多 30 条）。
func (t *TmdbClient) GetTrending(mediaType, timeWindow string) []TmdbListItem {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	if timeWindow != "day" {
		timeWindow = "week"
	}
	items := t.getListMultiPage("/trending/"+mediaType+"/"+timeWindow, 30, nil)
	return normalizeTmdbList(items, mediaType)
}

// GetNowPlaying 获取正在上映/正在播出（电影 /movie/now_playing，电视剧 /tv/on_the_air）。
func (t *TmdbClient) GetNowPlaying(mediaType string) []TmdbListItem {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	if mediaType == "tv" {
		globalItems := t.getListMultiPage("/tv/on_the_air", 30, nil)
		localItems := t.getListMultiPage("/discover/tv", 10, map[string]string{
			"with_origin_country": "CN",
			"sort_by":             "popularity.desc",
		})
		items := mergeTmdbRegionFocus(globalItems, localItems, 30)
		return normalizeTmdbList(items, "tv")
	}
	items := t.getListMultiPage("/movie/now_playing", 30, map[string]string{"region": "CN"})
	return normalizeTmdbList(items, "movie")
}

// GetTopRated 获取 TMDB 高分榜单（电影/电视剧），稍微侧重华语/国产内容。
func (t *TmdbClient) GetTopRated(mediaType string) []TmdbListItem {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	if mediaType == "tv" {
		globalItems := t.getListMultiPage("/tv/top_rated", 30, nil)
		localItems := t.getListMultiPage("/discover/tv", 10, map[string]string{
			"with_origin_country": "CN",
			"sort_by":             "vote_average.desc",
			"vote_count.gte":      "100",
		})
		items := mergeTmdbRegionFocus(globalItems, localItems, 30)
		return normalizeTmdbList(items, "tv")
	}
	items := t.getListMultiPage("/movie/top_rated", 30, map[string]string{"region": "CN"})
	return normalizeTmdbList(items, "movie")
}

// GetUpcoming 获取即将上映/即将播出的影视（最多 30 条）。
func (t *TmdbClient) GetUpcoming(mediaType string) []TmdbListItem {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	today := time.Now().Format("2006-01-02")
	if mediaType == "tv" {
		// Discover 按“首播日期 >= 今天”筛选真正未开播的剧，再排一批国产剧在前面
		globalItems := t.getListMultiPage("/discover/tv", 30, map[string]string{
			"first_air_date.gte": today,
			"sort_by":            "popularity.desc",
		})
		localItems := t.getListMultiPage("/discover/tv", 10, map[string]string{
			"first_air_date.gte":  today,
			"sort_by":             "popularity.desc",
			"with_origin_country": "CN",
		})
		merged := mergeTmdbRegionFocus(globalItems, localItems, 30)
		var items []tmdbListRaw
		for _, i := range merged {
			if i.FirstAirDate != "" && i.FirstAirDate >= today {
				items = append(items, i)
			}
		}
		return normalizeTmdbList(items, "tv")
	}
	// 电影：多翻几页过滤已上映，凑满 30 部未上映影片
	var items []tmdbListRaw
	seen := map[int]bool{}
	for page := 1; page <= 5; page++ {
		data, err := t.getJSON(context.Background(), "/movie/upcoming", map[string]string{
			"region": "CN",
			"page":   strconv.Itoa(page),
		})
		if err != nil {
			if len(items) > 0 {
				log.Printf("TMDB 翻页失败，使用已获取的 %d 条数据 (/movie/upcoming, page=%d)", len(items), page)
				break
			}
			log.Printf("TMDB 列表请求失败 (/movie/upcoming): %v", err)
			return nil
		}
		var r struct {
			Results []tmdbListRaw `json:"results"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			if len(items) > 0 {
				break
			}
			return nil
		}
		for _, item := range r.Results {
			if item.ID != 0 {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			if item.ReleaseDate != "" && item.ReleaseDate >= today {
				items = append(items, item)
				if len(items) >= 30 {
					return normalizeTmdbList(items, "movie")
				}
			}
		}
	}
	return normalizeTmdbList(items, "movie")
}

// SearchMedia 搜索电影/电视剧（最多 30 条）。
func (t *TmdbClient) SearchMedia(query, mediaType string) []TmdbListItem {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	if strings.TrimSpace(query) == "" {
		return nil
	}
	items := t.getListMultiPage("/search/"+mediaType, 30, map[string]string{"query": strings.TrimSpace(query)})
	return normalizeTmdbList(items, mediaType)
}

// GetDetailDict 获取 TMDB 完整详情（含演职员/中文译名），供前端弹窗展示。
func (t *TmdbClient) GetDetailDict(tmdbID int, mediaType string) *TmdbDetail {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	// external_ids 本函数结构体未声明任何字段，取回来纯属浪费响应体
	data, err := t.getJSON(context.Background(), fmt.Sprintf("/%s/%d", mediaType, tmdbID), map[string]string{
		"append_to_response":     "credits,images",
		"include_image_language": "zh,en,null",
	})
	if err != nil {
		log.Printf("TMDB 详情请求失败 (id=%d, type=%s): %v", tmdbID, mediaType, err)
		return nil
	}
	var raw tmdbDetailRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("TMDB 解析详情失败 (id=%d): %v", tmdbID, err)
		return nil
	}

	title := raw.Title
	if title == "" {
		title = raw.Name
	}
	if title == "" {
		title = raw.OriginalTitle
	}
	if title == "" {
		title = raw.OriginalName
	}
	origTitle := raw.OriginalTitle
	if origTitle == "" {
		origTitle = raw.OriginalName
	}
	date := raw.ReleaseDate
	if date == "" {
		date = raw.FirstAirDate
	}

	d := &TmdbDetail{
		ID:               raw.ID,
		Title:            title,
		OriginalTitle:    origTitle,
		MediaType:        mediaType,
		Overview:         raw.Overview,
		PosterPath:       raw.PosterPath,
		BackdropPath:     raw.BackdropPath,
		LogoPath:         selectTmdbLogo(raw.Images.Logos),
		TextlessPosterPath: t.GetTextlessPoster(tmdbID, mediaType),
		VoteAverage:      raw.VoteAverage,
		VoteCount:        raw.VoteCount,
		ReleaseDate:      date,
		OriginalLanguage: raw.OriginalLanguage,
		Status:           raw.Status,
		Homepage:         raw.Homepage,
		Runtime:          raw.Runtime,
		NumberOfEpisodes: raw.NumberOfEpisodes,
		NumberOfSeasons:  raw.NumberOfSeasons,
		TitleZh:          t.fetchTitleZh(tmdbID, mediaType),
	}
	for _, g := range raw.Genres {
		if g.Name != "" {
			d.Genres = append(d.Genres, g.Name)
		}
	}
	for _, c := range raw.ProductionCountries {
		if c.Name != "" {
			d.ProductionCountries = append(d.ProductionCountries, c.Name)
		}
	}
	if mediaType == "tv" {
		for _, c := range raw.CreatedBy {
			if c.Name != "" {
				d.Creators = append(d.Creators, c.Name)
			}
		}
	} else {
		for _, c := range raw.Credits.Crew {
			if c.Job == "Director" && c.Name != "" {
				d.Directors = append(d.Directors, c.Name)
			}
		}
	}
	for _, c := range raw.Credits.Cast[:min(8, len(raw.Credits.Cast))] {
		d.Cast = append(d.Cast, TmdbCastMember{Name: c.Name, Character: c.Character, ProfilePath: c.ProfilePath})
	}
	return d
}

// GetJSON 请求 TMDB 接口并返回完整 JSON（供日历/进度等接口使用）。
func (t *TmdbClient) GetJSON(path string, params map[string]string) ([]byte, error) {
	return t.getJSON(context.Background(), path, params)
}

// GetTextlessPoster 获取该影视的无文字竖向海报路径。
// 走独立 /images 端点（append_to_response=images 不支持 include_image_language=null），
// 只取 iso_639_1 为空（无文字）的海报，再从其中选 vote_count 最高的一张；无则返回空。
func (t *TmdbClient) GetTextlessPoster(tmdbID int, mtype string) string {
	if mtype != "tv" {
		mtype = "movie"
	}
	data, err := t.getJSON(context.Background(), fmt.Sprintf("/%s/%d/images", mtype, tmdbID),
		map[string]string{"include_image_language": "null"})
	if err != nil {
		return ""
	}
	var r struct {
		Posters []struct {
			FilePath  string `json:"file_path"`
			ISO6391   string `json:"iso_639_1"`
			VoteCount int    `json:"vote_count"`
		} `json:"posters"`
	}
	if json.Unmarshal(data, &r) != nil {
		return ""
	}
	best := ""
	bestVotes := -1
	for _, p := range r.Posters {
		if p.FilePath == "" || p.ISO6391 != "" {
			continue
		}
		if p.VoteCount > bestVotes {
			best, bestVotes = p.FilePath, p.VoteCount
		}
	}
	return best
}

// fetchTitleZh 获取简体中文译名（优先中国大陆 CN）。
func (t *TmdbClient) fetchTitleZh(tmdbID int, mediaType string) string {
	transData, err := t.getJSON(context.Background(), fmt.Sprintf("/%s/%d/translations", mediaType, tmdbID), nil)
	if err != nil {
		return ""
	}
	var td struct {
		Translations []struct {
			ISO639_1  string `json:"iso_639_1"`
			ISO3166_1 string `json:"iso_3166_1"`
			Data      struct {
				Title string `json:"title"`
				Name  string `json:"name"`
			} `json:"data"`
		} `json:"translations"`
	}
	if json.Unmarshal(transData, &td) != nil {
		return ""
	}
	titleZh := ""
	for _, tr := range td.Translations {
		if tr.ISO639_1 != "zh" {
			continue
		}
		translated := tr.Data.Title
		if translated == "" {
			translated = tr.Data.Name
		}
		if translated == "" {
			continue
		}
		if tr.ISO3166_1 == "CN" {
			return translated
		}
		if titleZh == "" {
			titleZh = translated
		}
	}
	return titleZh
}

// getListMultiPage 请求 TMDB 列表接口并自动翻页凑够 limit 条（最多 2 页）。
func (t *TmdbClient) getListMultiPage(path string, limit int, params map[string]string) []tmdbListRaw {
	var items []tmdbListRaw
	seen := map[int]bool{}
	for page := 1; page <= 2; page++ {
		p := map[string]string{}
		for k, v := range params {
			p[k] = v
		}
		p["page"] = strconv.Itoa(page)
		data, err := t.getJSON(context.Background(), path, p)
		if err != nil {
			if len(items) > 0 {
				log.Printf("TMDB 翻页失败，使用已获取的 %d 条数据 (%s, page=%d)", len(items), path, page)
				break
			}
			log.Printf("TMDB 列表请求失败 (%s): %v", path, err)
			return nil
		}
		var r struct {
			Results []tmdbListRaw `json:"results"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			if len(items) > 0 {
				break
			}
			return nil
		}
		for _, item := range r.Results {
			if item.ID != 0 {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			items = append(items, item)
			if len(items) >= limit {
				return items
			}
		}
	}
	return items
}

// normalizeTmdbList 把原始列表项统一转为前端友好结构。
func normalizeTmdbList(items []tmdbListRaw, mediaType string) []TmdbListItem {
	out := make([]TmdbListItem, 0, len(items))
	for _, item := range items {
		title := item.Title
		if title == "" {
			title = item.Name
		}
		if title == "" {
			if mediaType == "tv" {
				title = item.OriginalName
			} else {
				title = item.OriginalTitle
			}
		}
		date := item.ReleaseDate
		if date == "" {
			date = item.FirstAirDate
		}
		out = append(out, TmdbListItem{
			ID:               item.ID,
			Title:            title,
			MediaType:        mediaType,
			PosterPath:       item.PosterPath,
			BackdropPath:     item.BackdropPath,
			Overview:         item.Overview,
			VoteAverage:      item.VoteAverage,
			ReleaseDate:      date,
			OriginalLanguage: item.OriginalLanguage,
			GenreIDs:         item.GenreIDs,
		})
	}
	return out
}

// mergeTmdbRegionFocus 把华语/国产内容排在前面，再补全球内容，实现“稍微侧重本地”。
func mergeTmdbRegionFocus(globalItems, localItems []tmdbListRaw, limit int) []tmdbListRaw {
	var merged []tmdbListRaw
	seen := map[int]bool{}
	appendFn := func(items []tmdbListRaw) {
		for _, item := range items {
			if item.ID != 0 {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			merged = append(merged, item)
			if len(merged) >= limit {
				return
			}
		}
	}
	appendFn(localItems)
	if len(merged) < limit {
		appendFn(globalItems)
	}
	return merged
}
