package web

// 猫眼榜单 API（piaofang.maoyan.com 的两个公开接口，无需 Cookie/签名，只需 User-Agent）。
// 榜单只取前 9 条，匹配 TMDB 拿到海报与评分后复用 TMDB 卡片渲染；
// 订阅沿用 ENV_FILTER 关键词机制，不需要新代码。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"mmbot/internal/httpx"
	"mmbot/internal/transfer"
)

const (
	maoyanBase = "https://piaofang.maoyan.com"
	// 每个榜单只取前 9 条（电影票房接口会返回 94 条；电视剧热度接口本身只返回 10 条，最多取满 9）
	maoyanListSize = 9
	// 猫眼对空 UA 会拒绝；固定一个桌面 Chrome UA 即可，不需要随机列表
	maoyanUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"
	// maoyanMatchConcurrency 匹配 TMDB 的并发度。9 部片每部一次搜索，串行就是 9 个上游往返，
	// 首屏冷启动要干等好几秒；并发 6 路把总等待压到 2 个往返。不再调高是因为 TMDB 有速率限制，
	// 而任务总数本来就只有 9 个，6 路已经够铺满。
	maoyanMatchConcurrency = 6
)

// maoyanHTTP 猫眼抓取共享客户端，与频道监控同规格（15s 超时 + 连接复用）。
var maoyanHTTP = httpx.New(15 * time.Second)

var (
	// maoyanCache 榜单结果，key 只有 tv/movie 两个，常驻 ≤20 条
	maoyanCache = newTTLCache(24*time.Hour, 8)
	// maoyanFailCache 失败负缓存：失败若不缓存，猫眼挂掉时每次打开页面都会打一次上游
	maoyanFailCache = newTTLCache(5*time.Minute, 8)
	// maoyanMu 同一视图并发抓取（多标签页/多端）只打一次上游。
	// ponytail: 单把全局锁，代价是抓 tv 时并发的 movie 请求要一起排队；
	// 每视图首次加载本来就要等几秒，且 24h 内只发生一次，不值得为它做 per-key 锁。
	maoyanMu sync.Mutex
)

// ---------- 抓取 ----------

// maoyanGet 发起一次猫眼接口请求并返回响应体。
func maoyanGet(path string) ([]byte, error) {
	body, status, err := maoyanHTTP.Get(context.Background(), maoyanBase+path,
		map[string]string{"User-Agent": maoyanUA})
	if err != nil || status != 200 {
		return nil, fmt.Errorf("HTTP %d: %v", status, err)
	}
	return body, nil
}

// maoyanHeatTitles 取剧集热度榜片名。
// seriesType=4 就是「电视剧 + 网络剧」的并集，接口已按热度降序排好且本身返回 10 条，
// 因此合并/去重/排序全部不需要，一次请求即可。
func maoyanHeatTitles() ([]string, error) {
	body, err := maoyanGet("/dashboard/webHeatData?seriesType=4&platformType=&showDate=2")
	if err != nil {
		return nil, err
	}
	return parseMaoyanHeat(body)
}

func parseMaoyanHeat(body []byte) ([]string, error) {
	var r struct {
		Status   bool `json:"status"`
		DataList struct {
			List []struct {
				SeriesInfo struct {
					Name string `json:"name"`
				} `json:"seriesInfo"`
			} `json:"list"`
		} `json:"dataList"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("响应解析失败: %v", err)
	}
	// 剧集榜的失败信号是 HTTP 200 + status=false
	if !r.Status {
		return nil, fmt.Errorf("接口返回 status=false（参数失效或被拒绝）")
	}
	var out []string
	for _, it := range r.DataList.List {
		if n := strings.TrimSpace(it.SeriesInfo.Name); n != "" {
			out = append(out, n)
		}
		if len(out) >= maoyanListSize {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("接口返回空列表")
	}
	return out, nil
}

// maoyanBoxOfficeTitles 取电影票房榜片名（接口返回 94 条，只取前 9）。
// 排序是猫眼自己的综合序，不是票房降序，原样截取不重排。
func maoyanBoxOfficeTitles() ([]string, error) {
	body, err := maoyanGet("/dashboard-ajax/movie")
	if err != nil {
		return nil, err
	}
	return parseMaoyanBox(body)
}

func parseMaoyanBox(body []byte) ([]string, error) {
	var r struct {
		MovieList struct {
			List []struct {
				MovieInfo struct {
					MovieName string `json:"movieName"`
				} `json:"movieInfo"`
			} `json:"list"`
		} `json:"movieList"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("响应解析失败: %v", err)
	}
	// 电影榜响应顶层没有 status 字段，失败只能靠 movieList 缺失/为空判断
	var out []string
	for _, it := range r.MovieList.List {
		if n := strings.TrimSpace(it.MovieInfo.MovieName); n != "" {
			out = append(out, n)
		}
		if len(out) >= maoyanListSize {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("接口未返回 movieList（结构可能已变更）")
	}
	return out, nil
}

// ---------- TMDB 匹配 ----------

var maoyanTitleNoise = regexp.MustCompile(`[\s:：·\-*'!,?.。]+`)

// maoyanNormalize 归一化片名用于精确匹配（去掉空白与常见标点，忽略大小写）。
func maoyanNormalize(s string) string {
	return maoyanTitleNoise.ReplaceAllString(strings.ToLower(s), "")
}

// maoyanMatch 把猫眼片名匹配到 TMDB：先按归一化精确匹配，命中不了退化取最相关的一条。
// 标题始终保留猫眼原名——订阅是靠 strings.Contains 撞频道消息，
// 关键词必须是频道里真实出现的名字，换成 TMDB 译名会订不上；
// TMDB 只贡献 id（详情页）、海报、评分、年份。匹配不到时退回仅有标题的条目，仍可订阅。
func maoyanMatch(client *transfer.TmdbClient, name, mediaType string) transfer.TmdbListItem {
	item := transfer.TmdbListItem{Title: name, MediaType: mediaType}
	results := client.SearchMedia(name, mediaType, 20)
	if len(results) == 0 {
		log.Printf("[猫眼] 未能为 '%s' 匹配到 TMDB 条目，保留原标题", name)
		return item
	}
	best := results[0]
	norm := maoyanNormalize(name)
	for _, r := range results {
		if maoyanNormalize(r.Title) == norm {
			best = r
			break
		}
	}
	item.ID = best.ID
	item.PosterPath = best.PosterPath
	item.VoteAverage = best.VoteAverage
	item.ReleaseDate = best.ReleaseDate
	return item
}

// ---------- GET /api/maoyan ----------

func (s *Server) handleMaoyanRank(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if view != "tv" && view != "movie" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的 view，可选 tv/movie"})
		return
	}
	if tmdbAPIKey() == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      "未配置 TMDB API Key，请前往「用户配置」填写 ENV_TMDB_API_KEY",
		})
		return
	}

	// 成功/失败响应各构造一次；失败必须带 error，前端走「加载失败」提示而不是「暂无数据」
	maoyanOK := func(items any) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": true, "view": view, "items": items})
	}
	maoyanErr := func(msg string) {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true, "view": view, "items": []any{}, "error": msg,
		})
	}

	if items, ok := maoyanCache.get(view); ok {
		maoyanOK(items)
		return
	}
	if _, bad := maoyanFailCache.get(view); bad {
		maoyanErr("猫眼榜单暂时不可用（5 分钟内不再重试），请稍后再试")
		return
	}

	client := tmdbClient()
	if client == nil {
		maoyanErr("TMDB 客户端初始化失败，请检查 ENV_TMDB_API_KEY")
		return
	}

	maoyanMu.Lock()
	defer maoyanMu.Unlock()
	// 等锁期间可能已被前一个并发请求填好
	if items, ok := maoyanCache.get(view); ok {
		maoyanOK(items)
		return
	}

	var names []string
	var err error
	if view == "tv" {
		names, err = maoyanHeatTitles()
	} else {
		names, err = maoyanBoxOfficeTitles()
	}
	if err != nil {
		log.Printf("[猫眼] %s 榜单抓取失败: %v", view, err)
		maoyanFailCache.set(view, true)
		maoyanErr("猫眼榜单获取失败：" + err.Error())
		return
	}

	// 猫眼的 view 取值（tv/movie）与 TMDB 的 media_type 同名，直接传。
	// 固定并发池 + 按下标回填，顺序仍与猫眼榜单一致；maoyanMu 已经保证同时只有一个榜单在抓，
	// 所以这里并发只是同一请求内部的并发，不会放大上游压力。
	items := make([]transfer.TmdbListItem, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maoyanMatchConcurrency)
	for i, n := range names {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			defer func() { <-sem }()
			items[i] = maoyanMatch(client, n, view)
		}(i, n)
	}
	wg.Wait()
	maoyanCache.set(view, items)
	log.Printf("[猫眼] %s 榜单已获取并缓存 %d 条", view, len(items))
	maoyanOK(items)
}
