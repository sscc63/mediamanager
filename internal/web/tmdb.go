package web

// TMDB 榜单/搜索/订阅/详情/集数/日历 API（对应 server.py 的 TMDB 与订阅进度部分）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmbot/internal/strm"
	"mmbot/internal/transfer"
)

// ttlCache 带 TTL 与容量上限的内存缓存：榜单/搜索、详情、集数、日历共用这一份实现。
// 取值、回源、写回是分开的三步——回源期间不持锁，否则并发请求会互相排队。
type ttlCache struct {
	mu       sync.Mutex
	m        map[string]ttlEntry
	inflight map[string]*inflightCall
	ttl      time.Duration
	max      int
}

type ttlEntry struct {
	at time.Time
	v  any
}

// inflightCall 一次正在进行的回源；done 关闭后 v/ok 才可读。
type inflightCall struct {
	done chan struct{}
	v    any
	ok   bool
}

func newTTLCache(ttl time.Duration, max int) *ttlCache {
	return &ttlCache{
		m:        map[string]ttlEntry{},
		inflight: map[string]*inflightCall{},
		ttl:      ttl,
		max:      max,
	}
}

// do 「取缓存 → 未命中则回源 → 写回」一条龙，并把同一 key 的并发回源合并成一次。
// get/set 分开调用做不到这一点：冷启动时首页会同时打多个榜单、多标签页同时打开也会各请求一遍，
// 未合并时这些并发请求都会打穿到上游。回源仍然不持锁，并发者只是等同一个结果。
//
// skipCache 对应接口的 refresh=1：跳过缓存读取，但依然参与合并并写回新值。
// fetch 返回 (值, 是否值得缓存)；返回 nil 表示回源失败（不写缓存，调用方据值是否为 nil 判错）。
func (c *ttlCache) do(key string, skipCache bool, fetch func() (any, bool)) (any, bool) {
	if !skipCache {
		if v, ok := c.get(key); ok {
			return v, true
		}
	}

	c.mu.Lock()
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.v, call.ok
	}
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	var v any
	var cacheable bool
	// defer 兜住 panic：否则 inflight 残留，同 key 的后续请求会永久阻塞在 <-call.done
	defer func() {
		call.v, call.ok = v, cacheable
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		close(call.done)
	}()

	v, cacheable = fetch()
	if cacheable && v != nil {
		c.set(key, v)
	}
	return v, cacheable
}

// get 命中且未过期时返回 (值, true)。
func (c *ttlCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) >= c.ttl {
		return nil, false
	}
	return e.v, true
}

// set 写入并维持条目上限，满了先淘汰最旧的一条。
// ponytail: 淘汰是 O(n) 扫描（n ≤ max，最多几百），不值得为它引入 LRU 链表。
func (c *ttlCache) set(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok && len(c.m) >= c.max {
		oldest := ""
		for k, e := range c.m {
			if oldest == "" || e.at.Before(c.m[oldest].at) {
				oldest = k
			}
		}
		if oldest != "" {
			delete(c.m, oldest)
		}
	}
	c.m[key] = ttlEntry{at: time.Now(), v: v}
}

// 各接口缓存：TTL 按「数据多久才会变」定，容量按「最多同时用多少个 key」定
var (
	// listCache 榜单 + 关键词搜索。容量要同时覆盖两条最费缓存的路径：
	// 已订阅页 60 个关键词 × movie/tv = 120 个 key；频道更新页 limit=200 时最多 200 个片名 × movie/tv = 400 个 key。
	// 原先只按 160 定，频道更新页一开就整批淘汰自己刚写的条目 —— 等于每次打开都重新打一遍 TMDB。
	listCache = newTTLCache(24*time.Hour, 512)
	// detailCache 详情（演职员 + 图片，单个值最重）
	detailCache = newTTLCache(24*time.Hour, 200)
	// episodeTotalCache TMDB 总集数，剧集只会增季不会改总数
	episodeTotalCache = newTTLCache(24*time.Hour, 300)
	// embyCountCache Emby 已收录集数，随下载变化
	embyCountCache = newTTLCache(10*time.Minute, 300)
	// calendarCache 单部剧的排播（key 为 tmdb id）；日期过滤在读取时做，跨天不会串味
	calendarCache = newTTLCache(24*time.Hour, 300)
	// textlessCache 无文字海报路径（key 为 type:id）。空串也缓存——它表示「TMDB 确实没有这张图」，
	// 不缓存的话手机端每次打开入口海报都要为同一部片重打一次 /images。
	textlessCache = newTTLCache(24*time.Hour, 200)
)

var (
	webTMDBMu  sync.Mutex
	webTMDBKey string
	webTMDB    *transfer.TmdbClient
)

func tmdbAPIKey() string { return envGet("ENV_TMDB_API_KEY", "") }

func tmdbClient() *transfer.TmdbClient {
	key := tmdbAPIKey()
	if key == "" {
		return nil
	}
	if executor := transfer.GetTransferExecutor(); executor != nil && executor.TMDB != nil && executor.TMDB.APIKey == key {
		return executor.TMDB
	}
	webTMDBMu.Lock()
	defer webTMDBMu.Unlock()
	if webTMDB != nil && webTMDBKey == key {
		return webTMDB
	}
	c, err := transfer.NewTmdbClient(key, "")
	if err != nil {
		return nil
	}
	webTMDBKey = key
	webTMDB = c
	return webTMDB
}

func embyClient() *strm.EmbyClient {
	return strm.GetEmbyRuntime()
}

// ---------- GET /api/tmdb ----------

func (s *Server) handleTMDBSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	category := q.Get("category")
	if category == "" {
		category = "trending"
	}
	mediaType := q.Get("type")
	if mediaType == "" {
		mediaType = "movie"
	}
	timeWindow := q.Get("time_window")
	if timeWindow == "" {
		timeWindow = "week"
	}
	query := strings.TrimSpace(q.Get("q"))
	if query != "" {
		category = "search"
	}

	// 混合搜索：搜索时 type=all 表示电影 + 电视剧一起搜（前端搜索框走这条）。
	// 只有搜索支持 all —— 榜单里 movie/tv 是两个完全不同的列表，混起来没有意义。
	requestedType := mediaType
	hybrid := mediaType == "all" && query != ""
	if hybrid {
		mediaType = "movie" // 占位：只为通过下面的类型校验，实际两个类型都搜
	}

	validCategory := map[string]bool{"trending": true, "top_rated": true, "now_playing": true, "upcoming": true, "search": true}
	if !validCategory[category] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的 category，可选 trending/top_rated/now_playing/upcoming"})
		return
	}
	if mediaType != "movie" && mediaType != "tv" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的 type，可选 movie/tv"})
		return
	}
	if category == "trending" && timeWindow != "day" && timeWindow != "week" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的 time_window，可选 day/week"})
		return
	}

	if tmdbAPIKey() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "未配置 TMDB API Key，请前往「用户配置」填写 ENV_TMDB_API_KEY",
		})
		return
	}

	refresh := q.Get("refresh") == "1"
	var cacheKey string
	if query != "" {
		// 用 requestedType 而不是 mediaType：hybrid 时 mediaType 已被占位成 movie，
		// 直接用它会让混合搜索结果覆盖掉纯电影搜索的缓存条目。
		cacheKey = "search:" + requestedType + ":" + strings.ToLower(query)
	} else {
		cacheKey = category + ":" + mediaType + ":" + timeWindow
	}

	client := tmdbClient()
	// 回源闭包：nil 表示失败（不缓存）；空列表算成功但不缓存（下次仍然重试）
	fetchItems := func() (any, bool) {
		var items []transfer.TmdbListItem
		switch {
		case hybrid:
			items = client.SearchMediaHybrid(query, 30)
		case query != "":
			items = client.SearchMedia(query, mediaType, 30)
		case category == "trending":
			items = client.GetTrending(mediaType, timeWindow)
		case category == "now_playing":
			items = client.GetNowPlaying(mediaType)
		case category == "top_rated":
			items = client.GetTopRated(mediaType)
		case category == "upcoming":
			items = client.GetUpcoming(mediaType)
		}
		if items == nil {
			return nil, false
		}
		return items, len(items) > 0
	}

	itemsVal, _ := listCache.do(cacheKey, refresh, fetchItems)
	if itemsVal == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"items":      []any{},
			"error":      "TMDB 请求失败，请检查 ENV_TMDB_API_KEY 是否正确或网络是否可访问 TMDB",
		})
		return
	}
	items, _ := itemsVal.([]transfer.TmdbListItem)
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":  true,
		"category":    category,
		"type":        requestedType,
		"time_window": timeWindowIf(category, timeWindow),
		"query":       queryOrNil(query),
		"items":       items,
	})
}

func timeWindowIf(category, tw string) any {
	if category == "trending" {
		return tw
	}
	return nil
}

func queryOrNil(q string) any {
	if q != "" {
		return q
	}
	return nil
}

// ---------- POST /api/tmdb/subscribe ----------

func (s *Server) handleTMDBSubscribe(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Title  string `json:"title"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效请求"})
		return
	}
	title := strings.TrimSpace(data.Title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "影片标题不能为空"})
		return
	}
	var ok bool
	var message, filterValue string
	if data.Action == "unsubscribe" {
		ok, message, filterValue = s.removeFilterKeyword(title)
	} else {
		ok, message, filterValue = s.addFilterKeyword(title)
	}
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": message})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": message, "filter_value": filterValue})
}

// ---------- GET /api/tmdb/detail ----------

func (s *Server) handleTMDBDetail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idStr := q.Get("id")
	mediaType := q.Get("type")
	if mediaType == "" {
		mediaType = "movie"
	}
	tmdbID, err := strconv.Atoi(idStr)
	if err != nil || tmdbID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少有效的 TMDB ID"})
		return
	}
	if mediaType != "movie" && mediaType != "tv" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的 type，可选 movie/tv"})
		return
	}
	if tmdbAPIKey() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "未配置 TMDB API Key，请前往「用户配置」填写 ENV_TMDB_API_KEY",
		})
		return
	}

	// 缓存 24h；需要强刷时带 refresh=1（与榜单/搜索接口同款约定）
	// 走 do() 合并并发回源：入口海报与详情页可能同时请求同一部片
	cacheKey := mediaType + ":" + idStr
	val, _ := detailCache.do(cacheKey, q.Get("refresh") == "1", func() (any, bool) {
		client := tmdbClient()
		if client == nil {
			return nil, false
		}
		d := client.GetDetailDict(tmdbID, mediaType)
		if d == nil {
			return nil, false
		}
		return d, true
	})
	detail, _ := val.(*transfer.TmdbDetail)
	if detail == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": true, "error": "请求失败，请检查 ENV_TMDB_API_KEY 或网络"})
		return
	}

	// 无文字海报只有手机端（≤768px）的入口海报用得到，而它是详情里唯一需要额外一次 /images
	// 往返的字段。只有显式带 textless=1 才去补，桌面端与详情弹窗都不付这个代价。
	if q.Get("textless") == "1" && detail.TextlessPosterPath == "" {
		if client := tmdbClient(); client != nil {
			key := mediaType + ":" + idStr
			v, _ := textlessCache.do(key, false, func() (any, bool) {
				return client.GetTextlessPoster(tmdbID, mediaType), true
			})
			if path, _ := v.(string); path != "" {
				// 浅拷贝再填：detail 是缓存里的共享对象，直接写字段会和并发读它的请求打架；
				// 其余字段（含切片）与缓存对象共享且只读，浅拷贝足够。
				cp := *detail
				cp.TextlessPosterPath = path
				detail = &cp
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "detail": detail})
}

// ---------- GET /api/media/episodes ----------

func (s *Server) handleMediaEpisodes(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	mediaType := r.URL.Query().Get("type")
	if mediaType == "" {
		mediaType = "tv"
	}
	tmdbID, err := strconv.Atoi(idStr)
	if err != nil || tmdbID <= 0 || (mediaType != "movie" && mediaType != "tv") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}

	cacheKey := mediaType + ":" + idStr

	// TMDB 总集数（电影为 1）：剧集只会增季、不会改总集数，缓存 24h。
	// 原先与 Emby 收录数共用一个 10 分钟 TTL，等于每 10 分钟白打一次 TMDB。
	total, ok := episodeTotalCache.get(cacheKey)
	if !ok && tmdbAPIKey() != "" {
		client := tmdbClient()
		data, err := client.GetJSON(fmt.Sprintf("/%s/%d", mediaType, tmdbID), nil)
		if err == nil {
			var d struct {
				NumberOfEpisodes any `json:"number_of_episodes"`
			}
			if json.Unmarshal(data, &d) == nil {
				if mediaType == "tv" {
					total = d.NumberOfEpisodes
				} else {
					total = 1
				}
				if total != nil {
					episodeTotalCache.set(cacheKey, total)
				}
			}
		}
	}

	// Emby 已收录条目数：随下载变化，保留 10 分钟 TTL
	emby, ok := embyCountCache.get(cacheKey)
	if !ok {
		emby = embyOwnedCount(idStr, mediaType)
		if emby != nil {
			embyCountCache.set(cacheKey, emby)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"emby": emby, "total": total})
}

// embyOwnedCount 查询该 TMDB 条目在 Emby 里已收录的集数（电影为条目数）；未配置或请求失败返回 nil。
func embyOwnedCount(idStr, mediaType string) any {
	client := embyClient()
	if client == nil {
		return nil
	}
	ctx := context.Background()
	if mediaType == "movie" {
		if resp := embyRequest(ctx, client, "/emby/Items", map[string]string{
			"Recursive":           "true",
			"IncludeItemTypes":    "Movie",
			"AnyProviderIdEquals": "tmdb." + idStr,
			"Limit":               "1",
		}); resp != nil {
			return resp["TotalRecordCount"]
		}
		return nil
	}
	resp := embyRequest(ctx, client, "/emby/Items", map[string]string{
		"Recursive":           "true",
		"IncludeItemTypes":    "Series",
		"AnyProviderIdEquals": "tmdb." + idStr,
		"Limit":               "1",
	})
	items, _ := resp["Items"].([]any)
	if len(items) == 0 {
		return nil
	}
	first, _ := items[0].(map[string]any)
	if first == nil {
		return nil
	}
	if resp2 := embyRequest(ctx, client, "/emby/Items", map[string]string{
		"ParentId":         fmt.Sprintf("%v", first["Id"]),
		"Recursive":        "true",
		"IncludeItemTypes": "Episode",
		"Limit":            "1",
	}); resp2 != nil {
		return resp2["TotalRecordCount"]
	}
	return nil
}

// embyRequest 请求 Emby GET API，失败返回 nil。
func embyRequest(ctx context.Context, c *strm.EmbyClient, endpoint string, params map[string]string) map[string]any {
	raw, status, err := c.GetJSON(ctx, endpoint, params)
	if err != nil || status != 200 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// ---------- GET /api/media/calendar ----------

func (s *Server) handleMediaCalendar(w http.ResponseWriter, r *http.Request) {
	idsParam := r.URL.Query().Get("ids")
	tvSet := map[int]bool{}
	for _, seg := range strings.Split(idsParam, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		mtype, mid := "tv", seg
		if strings.Contains(seg, ":") {
			parts := strings.SplitN(seg, ":", 2)
			mtype, mid = parts[0], parts[1]
		}
		if mtype == "tv" {
			if id, err := strconv.Atoi(mid); err == nil {
				tvSet[id] = true
			}
		}
	}
	var tvIDs []int
	for id := range tvSet {
		tvIDs = append(tvIDs, id)
	}

	today := time.Now().Format("2006-01-02")

	if tmdbAPIKey() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "未配置 TMDB API Key，请前往「用户配置」填写 ENV_TMDB_API_KEY",
		})
		return
	}

	shows := map[string]any{}
	dates := map[string]any{}
	client := tmdbClient()
	for _, tmdbID := range tvIDs {
		// 按剧缓存：订阅列表变化时只补拉新增的那几部，不再整表重算
		key := strconv.Itoa(tmdbID)
		v, ok := calendarCache.get(key)
		if !ok {
			if info := getUpcomingEpisodes(client, tmdbID, today); info != nil {
				calendarCache.set(key, info)
				v = info
			}
		}
		info, _ := v.(*upcomingInfo)
		if info == nil {
			continue
		}
		// 缓存里存的是整季排播，已播过的在这里按今天过滤掉（跨天由这层保证，缓存不需要跟着作废）
		var upcoming bool
		for airDate, eps := range info.byDate {
			if airDate < today {
				continue
			}
			upcoming = true
			if dates[airDate] == nil {
				dates[airDate] = map[string]any{}
			}
			dates[airDate].(map[string]any)[key] = eps
		}
		if upcoming {
			shows[key] = map[string]any{"title": info.title, "season": info.season}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "today": today, "shows": shows, "dates": dates,
	})
}

// upcomingInfo 一部剧当前播出季的排播（含已播出的集，按今天过滤由调用方做）。
type upcomingInfo struct {
	title  string
	season int
	byDate map[string][]int
}

func getUpcomingEpisodes(client *transfer.TmdbClient, tmdbID int, today string) *upcomingInfo {
	data, err := client.GetJSON(fmt.Sprintf("/tv/%d", tmdbID), nil)
	if err != nil {
		return nil
	}
	var d struct {
		Name    string `json:"name"`
		Orig    string `json:"original_name"`
		Seasons []struct {
			SeasonNumber int    `json:"season_number"`
			EpisodeCount int    `json:"episode_count"`
			AirDate      string `json:"air_date"`
		} `json:"seasons"`
	}
	if json.Unmarshal(data, &d) != nil {
		return nil
	}
	title := d.Name
	if title == "" {
		title = d.Orig
	}
	var seasons []struct {
		SeasonNumber int    `json:"season_number"`
		EpisodeCount int    `json:"episode_count"`
		AirDate      string `json:"air_date"`
	}
	for _, s := range d.Seasons {
		if s.SeasonNumber > 0 && s.EpisodeCount > 0 {
			seasons = append(seasons, s)
		}
	}
	if len(seasons) == 0 {
		return nil
	}
	// 当前播出季 = 已开播（首播日 ≤ 今天）的最晚一季；否则回退到最新一季
	target := seasons[0]
	latest := seasons[0]
	for _, s := range seasons {
		if s.SeasonNumber > latest.SeasonNumber {
			latest = s
		}
	}
	for _, s := range seasons {
		if s.AirDate != "" && s.AirDate <= today && s.SeasonNumber > target.SeasonNumber {
			target = s
		}
	}
	if !(target.AirDate != "" && target.AirDate <= today) {
		target = latest
	}

	episodes := client.GetEpisodes(tmdbID, target.SeasonNumber)
	byDate := map[string][]int{}
	for _, ep := range episodes {
		airDate := strings.TrimSpace(ep.AirDate)
		if airDate == "" || ep.EpisodeNumber <= 0 {
			continue
		}
		byDate[airDate] = append(byDate[airDate], ep.EpisodeNumber)
	}
	if len(byDate) == 0 {
		return nil
	}
	return &upcomingInfo{title: title, season: target.SeasonNumber, byDate: byDate}
}
