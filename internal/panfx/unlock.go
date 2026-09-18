package panfx

// 解锁：把资源帖正文里被「回复可见」挡住的内容取出来。
//
// ⚠️ 站点机制（2026-09 实测，与最初的猜测完全不同）：
// 资源站**不是积分制**。账号有 66 金币，但帖子正文被隐藏的真实原因是
// 「回复后再查看」——服务端**根本不下发**那段 HTML（不是前端 CSS 隐藏），
// 所以读源码、带 Cookie 直取都拿不到；唯一途径是该账号在该帖下**发过一条回复**。
//
// 关键证据（同一帖 tid 56180 对比）：
//   回复前：含 <div class="alert alert-warning">您好，本帖含有特定内容，请回复后再查看。</div>，无 alert-success
//   回复后：alert-warning 消失，出现 <div class="alert alert-success"> ... 分享链接 ... </div>
//
// 所以解锁 = 发一条回复 + 重新拉页面 + 抽链接。这是**以用户身份公开发言**的行为，
// 因此受三重约束（见 unlockWithReply）：
//   1. 必须用户显式点「解锁」才触发，绝不自动回访；
//   2. 同一 tid 只回复一次（服务端去重 + 本地已回复集合）；
//   3. 全局最短回复间隔（默认 30s），避免短时间刷帖触发风控。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// UnlockResult 解锁结果。
type UnlockResult struct {
	TID        string `json:"tid"`
	URL        string `json:"url"`         // 原帖地址，前端「查看原帖」用
	NeedUnlock bool   `json:"need_unlock"` // true=正文仍被挡住（未配置/回复失败）
	Message    string `json:"message"`     // 给用户看的提示
	Replied    bool   `json:"replied"`     // 本次是否真的发出了回复
	Links      []Link `json:"links"`       // 已拿到的网盘链接
}

// Link 一条网盘分享链接。
type Link struct {
	Pan  string `json:"pan"`  // 网盘类型：123 / 夸克 / 115 / 其他
	URL  string `json:"url"`  // 分享地址
	Pwd  string `json:"pwd"`  // 提取码（有则填）
	Text string `json:"text"` // 原文片段，便于人工判断
}

// ---------- 站点上的「回复可见」标记 ----------

// gateHints 回帖门的提示语。命中即说明该帖需要回复才能看到正文。
var gateHints = []string{
	"请回复后再查看",
	"回复后再查看",
	"回复可见",
	"隐藏内容",
	"本部分内容设定了隐藏",
	"您所在的用户组",
}

// noNeedReplyMarkers 明确的「不需要回复」信号：alert-success 是放行后的容器。
const unlockedMarker = "alert-success"

// replyTexts 站点自带的快捷回复（?post-create 页面里的 quick_reply_message 选项）。
// 用站点预设用语最自然、最不像机器人 —— 自造的句子在同一个号下反复出现反而更可疑。
var replyTexts = []string{
	"感谢分享,太棒了！",
	"不错的帖子！",
	"这不正是我苦苦寻找的资源吗！",
	"非常棒！！！",
	"哈哈，不错哦！",
}

var (
	// 123 网盘分享：域名后缀白名单 + /s/ 或 /123pan/ 路径 + 分享码。
	// 站点实际用的是 <uid>.share.123pan.cn/123pan/xxxx-yyyy 这种三级域名形式，
	// 所以 `[0-9A-Za-z.-]*\b` 前缀必须保留，否则匹配不到。
	rePan123 = regexp.MustCompile(`https?://[0-9A-Za-z.-]*\b(?:123pan|123865|123684|123912)\.[a-z]+/(?:s|123pan)/[0-9A-Za-z_-]+(?:\?[0-9A-Za-z_=&%-]*)?`)
	// 夸克
	reQuark = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9A-Za-z]+`)
	// 115
	re115 = regexp.MustCompile(`https?://(?:115|anxia)\.com/s/[0-9A-Za-z]+`)
	// 提取码：链接后面跟的「提取码 XXXX」「密码:XXXX」「?pwd=XXXX」
	rePwd = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|pwd|password)\s*[:：=]?\s*([0-9A-Za-z]{4})`)
	// 正文里 <a href="..."> 的地址（in 标签"里"的链接要靠它拿）
	reHref = regexp.MustCompile(`href="(https?://[^"]+)"`)
	// 帖子正文容器
	reMessageBody = regexp.MustCompile(`(?s)<div class="message[^"]*"[^>]*>(.*?)(?:<div class="card-footer|</div>\s*</div>\s*<a[^>]*class="btn btn-secondary)`)
	// 剥标签
	reTagStrip = regexp.MustCompile(`<[^>]+>`)
)

// Unlock 取某帖正文里的资源链接。
//
// 流程：拉页面 → 正文里已有链接就直接返回（零副作用）；
// 被回帖门挡住时，先看本地是否已为此 tid 回复过（去过一次就不重复发言），
// 需要回复则由调用方决定是否回复（见 UnlockOptions.Reply）。
func (c *Client) Unlock(ctx context.Context, tid string) (*UnlockResult, error) {
	return c.unlockWithReply(ctx, tid, true)
}

// UnlockReadOnly 只读解锁：绝不发回复。用于「不想在站点留痕」的场景，
// 或回复功能被用户关闭时。已回复过的帖子依然能正常读出链接。
func (c *Client) UnlockReadOnly(ctx context.Context, tid string) (*UnlockResult, error) {
	return c.unlockWithReply(ctx, tid, false)
}

func (c *Client) unlockWithReply(ctx context.Context, tid string, allowReply bool) (*UnlockResult, error) {
	tid = strings.TrimSpace(tid)
	if tid == "" || !regexp.MustCompile(`^\d+$`).MatchString(tid) {
		return nil, fmt.Errorf("无效的资源 id")
	}
	pageURL := fmt.Sprintf("%s/thread-%s.htm", c.Base, tid)
	res := &UnlockResult{TID: tid, URL: pageURL}

	if c.Cookie == "" {
		res.NeedUnlock = true
		res.Message = "未配置资源站登录 Cookie，无法在面板内解锁，请到原帖查看"
		return res, nil
	}

	html, err := c.fetchThread(ctx, tid)
	if err != nil {
		return nil, err
	}
	// 登录态失效
	if strings.Contains(html, "退出登录") == false && strings.Contains(html, "请先登录") {
		res.NeedUnlock = true
		res.Message = "资源站登录状态已失效，请在「用户配置」更新 Cookie"
		return res, nil
	}

	// 一、正文里已有链接 → 直接返回，不产生任何副作用
	if links := extractLinks(html); len(links) > 0 {
		res.Links = links
		res.Message = "已获取资源链接"
		return res, nil
	}

	// 二、被回帖门挡住
	if !hasReplyGate(html) {
		// 既没链接也没有回帖门：正文可见但确实没有网盘链接（可能只有磁力/附件）
		res.Message = "帖子正文里没有找到网盘分享链接"
		return res, nil
	}

	// 三、需要回复才可见
	if !allowReply {
		res.NeedUnlock = true
		res.Message = "该资源需在资源站回复后才能查看链接（当前为只读取模式，未自动回复）"
		return res, nil
	}
	if !c.mayReply(tid) {
		res.NeedUnlock = true
		res.Message = "该资源需回复后才能查看，但近期回复过于频繁，请稍后再试"
		return res, nil
	}

	// 四、发回复 → 重新拉页面 → 抽链接
	if err := c.postReply(ctx, tid, pageURL); err != nil {
		res.NeedUnlock = true
		res.Message = "自动回复失败：" + err.Error() + "　可到原帖手动回复后重试"
		return res, nil
	}
	res.Replied = true

	html2, err := c.fetchThread(ctx, tid)
	if err != nil {
		res.NeedUnlock = true
		res.Message = "已回复成功，但重新读取帖子失败，请稍后再点一次解锁"
		return res, nil
	}
	if links := extractLinks(html2); len(links) > 0 {
		res.Links = links
		res.Message = "已回复并解锁资源链接"
		return res, nil
	}
	// 回复成功了但内容还没放出来：可能是审核或延迟
	res.NeedUnlock = true
	res.Message = "已回复成功，但链接尚未显示，请稍后重试（或到原帖查看）"
	return res, nil
}

// fetchThread 拉取帖子页 HTML。
func (c *Client) fetchThread(ctx context.Context, tid string) (string, error) {
	pageURL := fmt.Sprintf("%s/thread-%s.htm", c.Base, tid)
	body, status, err := c.http.Get(ctx, pageURL, c.headers(map[string]string{"Referer": c.Base + "/"}))
	if err != nil {
		return "", fmt.Errorf("请求资源帖失败：%w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("资源帖返回 HTTP %d", status)
	}
	return string(body), nil
}

// hasReplyGate 判断页面是否处于「回复可见」状态。
func hasReplyGate(html string) bool {
	for _, h := range gateHints {
		if strings.Contains(html, h) {
			return true
		}
	}
	return false
}

// postReply 以当前账号在该帖下发一条回复。
//
// 端点形如 POST /?post-create-<tid>-1.htm，表单字段（实测）：
//
//	doctype=1  return_html=1  quotepid=0  message=<回复内容>  quick_reply_message=<快捷语 id>
//
// 返回 JSON，code=="0" 为成功。
func (c *Client) postReply(ctx context.Context, tid, referer string) error {
	// 从预设池里轮着选，避免同一账号连续多帖都用同一句
	text := replyTexts[int(time.Now().Unix())%len(replyTexts)]
	form := url.Values{}
	form.Set("doctype", "1")
	form.Set("return_html", "1")
	form.Set("quotepid", "0")
	form.Set("message", text)
	form.Set("quick_reply_message", "4") // 4=「感谢分享,太棒了！」

	u := fmt.Sprintf("%s/?post-create-%s-1.htm", c.Base, tid)
	h := c.headers(map[string]string{
		"Referer":          referer,
		"Content-Type":     "application/x-www-form-urlencoded",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           c.Base,
	})
	body, status, err := c.http.Post(ctx, u, h, []byte(form.Encode()))
	if err != nil {
		return fmt.Errorf("网络错误（%v）", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("站点返回 HTTP %d", status)
	}
	// 站点回的是 JSON：{"code":"0","message":"<新楼层HTML>"}
	var r struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		// 非 JSON 多半是被风控页拦了
		if len(body) > 0 && (strings.Contains(string(body), "验证") || strings.Contains(string(body), "频繁")) {
			return fmt.Errorf("站点要求人工验证，可能存在风控，请到原帖手动回复")
		}
		return fmt.Errorf("站点响应无法识别（可能被风控拦截）")
	}
	if r.Code != "0" {
		msg := flattenHTML(r.Message)
		if msg == "" {
			msg = "站点拒绝该操作"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// mayReply 回复频率闸门：同一 tid 24h 内只回一次 + 全局最短间隔。
//
// 站点本身对重复回复也不会再放行（回复过即已解锁），这里提前挡住主要是
// 省一次无谓请求，更关键的是避免用户连点多个帖子把账号刷成刷帖号。
// 已回复过的 tid 直接放行且不占最小间隔 —— 因为不会再发新回复。
func (c *Client) mayReply(tid string) bool {
	c.replyMu.Lock()
	defer c.replyMu.Unlock()
	if c.replied == nil {
		c.replied = map[string]time.Time{}
	}
	// 清理 24h 前的记录，防止长期运行无限增长
	now := time.Now()
	for k, t := range c.replied {
		if now.Sub(t) > 24*time.Hour {
			delete(c.replied, k)
		}
	}
	if _, done := c.replied[tid]; done {
		return true
	}
	if !c.lastReply.IsZero() && now.Sub(c.lastReply) < c.minReplyGap {
		return false
	}
	c.replied[tid] = now
	c.lastReply = now
	c.saveRepliedLocked()
	return true
}

// flattenHTML 去掉标签与空白，用于把站点的 HTML 错误信息变成一行人话。
func flattenHTML(s string) string {
	s = reTagStrip.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&quot;", `"`, "&#039;", "'").Replace(s)
	return reSpacesTrim(s)
}

// extractLinks 从帖子 HTML 里抽网盘分享链接并去重。
//
// 关键顺序：**先扫原始 HTML（含 href 属性），再剥标签扫可见文本**。
// 站点的分享链接两种形态都有：<a href="...">...</a> 与正文里直接写 URL。
// 先剥标签会把 href 里的地址一起吃掉 —— 那样会漏掉一大半资源。
func extractLinks(html string) []Link {
	region := html
	if i := strings.Index(html, `class="message`); i >= 0 {
		region = html[i:]
	}

	seen := map[string]bool{}
	out := make([]Link, 0, 4)
	add := func(text string) {
		for _, m := range rePan123.FindAllString(text, -1) {
			addLink(&out, seen, "123", m, region)
		}
		for _, m := range reQuark.FindAllString(text, -1) {
			addLink(&out, seen, "夸克", m, region)
		}
		for _, m := range re115.FindAllString(text, -1) {
			addLink(&out, seen, "115", m, region)
		}
	}
	add(region)
	add(reTagStrip.ReplaceAllString(region, "\n"))
	return out
}

func addLink(out *[]Link, seen map[string]bool, pan, raw, page string) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "。，,、；;）)】]}")
	if raw == "" || seen[raw] {
		return
	}
	raw = strings.ReplaceAll(raw, "&amp;", "&")
	seen[raw] = true
	l := Link{Pan: pan, URL: raw, Text: raw}
	if u, err := url.Parse(raw); err == nil {
		if p := u.Query().Get("pwd"); p != "" {
			l.Pwd = p
		}
	}
	if l.Pwd == "" {
		// 提取码常写在链接后面的文字里，取链接附近一小段来找
		idx := strings.Index(page, raw)
		if idx >= 0 {
			end := idx + len(raw) + 200
			if end > len(page) {
				end = len(page)
			}
			if m := rePwd.FindStringSubmatch(page[idx:end]); len(m) > 1 {
				l.Pwd = m[1]
			}
		}
	}
	*out = append(*out, l)
}
