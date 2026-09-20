package bot

// 消息处理器（对应 123bot.py 的 handle_general_message / process_json_file /
// process_text_document / handle_oauth / handle_restart / add_filter / remove_filter）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"mmbot/internal/elevenfive"
	"mmbot/internal/httpx"
	"mmbot/internal/oauth"
	"mmbot/internal/pan123"
	"mmbot/internal/quark"
	"mmbot/internal/transfer"
)

// linkProcessMu 链接处理全局锁（对应 Python link_process_lock，确保多个请求依次处理）。
var linkProcessMu sync.Mutex

// tgFileHTTP TG 文件下载用共享 HTTP 客户端。
var tgFileHTTP = httpx.New(60 * time.Second)

// saveEnvFilter 持久化保存过滤词到 db/user.env 文件。
func (b *Bot) saveEnvFilter(newFilterValue string) bool {
	envFile := filepath.Join("db", "config", "user.env")
	data, err := os.ReadFile(envFile)
	if err != nil {
		log.Printf("[Bot] %s 文件不存在: %v", envFile, err)
		return false
	}
	lines := strings.Split(string(data), "\n")
	updated := make([]string, 0, len(lines)+1)
	found := false
	insertIndex := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "ENV_FILTER=") {
			updated = append(updated, "ENV_FILTER="+newFilterValue)
			found = true
			continue
		}
		updated = append(updated, line)
		if insertIndex == -1 && strings.Contains(line, "# 检查新消息的时间间隔（分钟）") {
			insertIndex = i + 2
		}
	}
	if !found {
		if insertIndex != -1 && insertIndex <= len(updated) {
			insert := make([]string, 0, len(updated)+1)
			insert = append(insert, updated[:insertIndex]...)
			insert = append(insert, "ENV_FILTER="+newFilterValue)
			insert = append(insert, updated[insertIndex:]...)
			updated = insert
		} else {
			updated = append(updated, "", "ENV_FILTER="+newFilterValue)
		}
	}
	if err := os.WriteFile(envFile, []byte(strings.Join(updated, "\n")), 0o644); err != nil {
		log.Printf("保存环境变量失败: %v", err)
		return false
	}
	return true
}

// handleAddFilter /add 添加监控过滤词。
func (b *Bot) handleAddFilter(msg *tgbotapi.Message) {
	if !b.isAdmin(msg) {
		return
	}
	currentText := b.Filter()
	if currentText == "" {
		currentText = "无（未设置任何过滤词）"
	}
	usageText := "ℹ️ 用法：\n- 添加过滤词：/add 权力的游戏\n- 删除过滤词：/remove 权力的游戏"
	_ = usageText

	parts := strings.SplitN(msg.Text, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("📌【 当前过滤词】：\n%s \n❌ 请输入要添加的过滤词（例如：/add 权力的游戏）", currentText))
		})
		return
	}
	newFilters := splitFilter(parts[1])
	cur := splitFilter(b.Filter())
	var added, existing []string
	for _, nf := range newFilters {
		if !containsStr(cur, nf) {
			added = append(added, nf)
			cur = append(cur, nf)
		} else {
			existing = append(existing, nf)
		}
	}
	if len(added) == 0 {
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("📌 【当前过滤词】：\n%s \n⚠️ 所有过滤词「%s」已存在，无需重复添加", currentText, strings.Join(existing, ", ")))
		})
		return
	}
	newFilter := strings.Join(cur, "|")
	if !b.saveEnvFilter(newFilter) {
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("📌 【当前过滤词】：\n%s \n⚠️ 过滤词添加成功，但保存到文件失败，请手动在配置页面更新", currentText))
		})
	}
	b.SetFilter(newFilter)

	feedback := fmt.Sprintf("📌 【当前过滤词】：\n%s \n", currentText)
	if len(added) > 0 {
		feedback += fmt.Sprintf("✅ 【已添加过滤词】：「%s」\n", strings.Join(added, ", "))
	}
	if len(existing) > 0 {
		feedback += fmt.Sprintf("⚠️ 【已存在的过滤词】：「%s」\n", strings.Join(existing, ", "))
	}
	feedback += fmt.Sprintf("📌 【更新后过滤词】：\n%s", newFilter)
	b.SubmitSend(func() { b.SendReply(msg, feedback) })
	log.Printf("用户 %d 执行/add，添加过滤词：%s，已存在：%s，更新后：%s", msg.From.ID, strings.Join(added, ", "), strings.Join(existing, ", "), newFilter)
}

// handleRemoveFilter /remove 删除监控过滤词。
func (b *Bot) handleRemoveFilter(msg *tgbotapi.Message) {
	if !b.isAdmin(msg) {
		return
	}
	currentText := b.Filter()
	if currentText == "" {
		currentText = "无（未设置任何过滤词）"
	}
	if b.Filter() == "" {
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("📌 【当前过滤词】：\n%s\n⚠️ 当前无任何过滤词，无需删除", currentText))
		})
		return
	}
	parts := strings.SplitN(msg.Text, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("📌 【当前过滤词】：\n%s \n❌ 请输入要删除的过滤词（例如：/remove 权力的游戏）", currentText))
		})
		return
	}
	delFilters := splitFilter(parts[1])
	cur := splitFilter(b.Filter())
	var deleted, notFound []string
	for _, df := range delFilters {
		if containsStr(cur, df) {
			deleted = append(deleted, df)
		} else {
			notFound = append(notFound, df)
		}
	}
	var newCur []string
	for _, f := range cur {
		if !containsStr(deleted, f) {
			newCur = append(newCur, f)
		}
	}
	newFilter := strings.Join(newCur, "|")
	if !b.saveEnvFilter(newFilter) {
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("📌 【当前过滤词】：\n%s \n⚠️ 过滤词删除成功，但保存到文件失败，请手动在配置页面更新", currentText))
		})
	}
	b.SetFilter(newFilter)

	updatedText := newFilter
	if updatedText == "" {
		updatedText = "无"
	}
	feedback := fmt.Sprintf("📌 【当前过滤词】：\n%s \n", currentText)
	if len(deleted) > 0 {
		feedback += fmt.Sprintf("✅ 【已删除过滤词】：「%s」\n", strings.Join(deleted, ", "))
	}
	if len(notFound) > 0 {
		feedback += fmt.Sprintf("⚠️ 【未找到的过滤词】：「%s」\n", strings.Join(notFound, ", "))
	}
	feedback += fmt.Sprintf("📌 【更新后过滤词】：\n%s", updatedText)
	b.SubmitSend(func() { b.SendReply(msg, feedback) })
	log.Printf("用户 %d 执行/remove，删除过滤词：%s，未找到：%s，更新后：%s", msg.From.ID, strings.Join(deleted, ", "), strings.Join(notFound, ", "), newFilter)
}

func splitFilter(s string) []string {
	var out []string
	for _, f := range strings.Split(s, "|") {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// handleOAuth /oauth 发起 Litepan OAuth 认证。
func (b *Bot) handleOAuth(msg *tgbotapi.Message) {
	if !b.isAdmin(msg) {
		return
	}
	b.SubmitSend(func() { b.doOAuthFlow(msg) })
}

// doOAuthFlow 执行完整的 Litepan OAuth 认证流程。
func (b *Bot) doOAuthFlow(msg *tgbotapi.Message) {
	client := getOAuthClient()
	if client.AccessToken != "" && !oauthIsTokenExpired(client) {
		b.SendReply(msg, "✅ Litepan token 仍然有效，无需重新认证")
		log.Printf("[OAuth] 收到 /oauth，但 token 仍有效，无需重新认证")
		return
	}
	log.Printf("[OAuth] 收到 /oauth（admin=%d），发起 Litepan 认证", msg.From.ID)
	b.SendReply(msg, "🔄 正在发起 Litepan 认证请求...")
	authData, err := client.StartAuth(context.Background())
	if err != nil {
		log.Printf("[OAuth] 认证启动失败: %v", err)
		b.SendReply(msg, fmt.Sprintf("❌ 认证启动失败：%v", err))
		return
	}
	sessionID, _ := authData["session_id"].(string)
	authURL, _ := authData["oauth_url"].(string)
	if authURL == "" {
		authURL, _ = authData["auth_url"].(string)
	}
	if sessionID == "" || authURL == "" {
		log.Printf("[OAuth] 认证启动失败：未返回 session_id 或 auth_url（data=%v）", authData)
		b.SendReply(msg, "❌ 认证启动失败：未返回 session_id 或 auth_url")
		return
	}
	log.Printf("[OAuth] 认证链接已生成（session=%s），等待用户授权", sessionID)
	b.SendReply(msg, fmt.Sprintf("🔗 请扫码或点击链接完成 123云盘 授权：\n%s\n\n⏳ 等待授权完成（最多3分钟）...", authURL))

	// 轮询等待授权（最多 60 次 × 3 秒 = 3 分钟）
	tokenData, err := client.PollStatus(context.Background(), sessionID, 60)
	if err != nil {
		log.Printf("[OAuth] 等待授权超时: %v", err)
		b.SendReply(msg, fmt.Sprintf("❌ OAuth认证失败：%v", err))
		return
	}
	client.AccessToken = toString(tokenData["access_token"])
	client.RefreshToken = toString(tokenData["refresh_token"])
	expiresIn, _ := toInt(tokenData["expires_in"])
	client.TokenExpiresAt = time.Now().Unix() + int64(expiresIn)
	oauthSaveToken(client)
	client.ConfirmReceived(context.Background(), sessionID)
	log.Printf("✅ [OAuth] Litepan 认证成功（expires_at=%s），已保存到 data/oauth_token.json",
		time.Unix(client.TokenExpiresAt, 0).Format("2006-01-02 15:04:05"))
	b.SendReply(msg, "✅ Litepan OAuth 认证成功，115→123 秒传功能可用")
}

// oauthIsTokenExpired 判断 OAuth token 是否过期（对 oauth.Client 的 unexported 方法的替代）。
func oauthIsTokenExpired(c *oauth.Client) bool {
	if c.AccessToken == "" {
		return true
	}
	return time.Now().Unix() >= c.TokenExpiresAt-60
}

// oauthSaveToken 持久化 OAuth token（client.saveToken 为 unexported，这里重写一份）。
// 路径必须与 Web 层 handleOAuthStatus 一致（data/oauth_token.json），否则前端读不到认证状态。
func oauthSaveToken(c *oauth.Client) {
	tokenFile := filepath.Join("data", "oauth_token.json")
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o755); err != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.TokenExpiresAt,
	})
	_ = os.WriteFile(tokenFile, data, 0o644)
}

// handleRestart /restart 重启服务：发送提示后延迟 2 秒硬退出（由守护进程自动拉起）。
func (b *Bot) handleRestart(msg *tgbotapi.Message) {
	if !b.isAdmin(msg) {
		return
	}
	b.SubmitSend(func() { b.SendReply(msg, "🔄 正在重启服务，请稍候约 30 秒后恢复...") })
	log.Printf("收到 TG 命令 /restart（来自 %d），准备重启...", msg.From.ID)
	go func() {
		time.Sleep(2 * time.Second)
		log.Printf("TG 命令触发重启，进程将立即退出")
		os.Exit(0)
	}()
}

// handleGeneralMessage 处理非命令消息（123分享/秒传链接、磁力、夸克/115转存、删除确认）。
func (b *Bot) handleGeneralMessage(msg *tgbotapi.Message) {
	log.Printf("进入 handleGeneralMessage")
	if !b.isAdmin(msg) {
		return
	}

	linkProcessMu.Lock()
	defer linkProcessMu.Unlock()

	userID := msg.From.ID
	text := msg.Text
	caption := msg.Caption
	log.Printf("收到消息: text='%s', caption='%s'", text, caption)

	var allLinks []string
	allLinks = append(allLinks, extract123LinksFromFullText(text)...)
	allLinks = append(allLinks, extract123LinksFromFullText(caption)...)
	// 实体中的 URL
	for _, e := range msg.Entities {
		if e.Type == "url" && e.Offset >= 0 && e.Offset+e.Length <= len(text) {
			allLinks = append(allLinks, extractTargetURL(text[e.Offset:e.Offset+e.Length])...)
		} else if e.Type == "text_link" && e.URL != "" {
			allLinks = append(allLinks, extractTargetURL(e.URL)...)
		}
	}
	for _, e := range msg.CaptionEntities {
		if e.Type == "url" && e.Offset >= 0 && e.Offset+e.Length <= len(caption) {
			allLinks = append(allLinks, extractTargetURL(caption[e.Offset:e.Offset+e.Length])...)
		} else if e.Type == "text_link" && e.URL != "" {
			allLinks = append(allLinks, extractTargetURL(e.URL)...)
		}
	}

	// 资源卡片：inline keyboard 按钮里的 URL（与 Python 原版 handle_general_message 一致）
	if msg.ReplyMarkup != nil {
		for _, row := range msg.ReplyMarkup.InlineKeyboard {
			for _, btn := range row {
				if btn.URL != nil && *btn.URL != "" {
					allLinks = append(allLinks, extractTargetURL(*btn.URL)...)
				}
			}
		}
	}

	if len(allLinks) > 0 {
		log.Printf("从消息中提取到%d个链接", len(allLinks))
		successCount := 0
		failCount := 0
		var failMessages []string
		for _, link := range allLinks {
			log.Printf("处理链接: %s", link)
			switch {
			case strings.HasPrefix(link, "123FSLinkV1") || strings.HasPrefix(link, "123FSLinkV2") ||
				strings.HasPrefix(link, "123FLCPV1") || strings.HasPrefix(link, "123FLCPV2"):
				b.saveShareLinkMsg(msg, link)
				successCount++
			case strings.Contains(link, "123pan") || strings.Contains(link, "123865") || strings.Contains(link, "123684"):
				targetPID := b.env.GetInt("ENV_123_LINK_UPLOAD_PID", 0)
				if b.transferSharedLinkOptimize(link, targetPID) {
					log.Printf("转存成功: %s", link)
					successCount++
					b.triggerTransferAfterSave(targetPID)
				} else {
					log.Printf("转存失败: %s", link)
					failCount++
					failMessages = append(failMessages, link+": 转存失败")
				}
			}
		}
		if failCount > 0 {
			b.SubmitSend(func() {
				b.SendReply(msg, fmt.Sprintf("转存完成：成功%d个，失败%d个\n失败详情: %s", successCount, failCount, strings.Join(failMessages, "\n")))
			})
		} else {
			b.SubmitSend(func() {
				b.SendReplyDelete(msg, fmt.Sprintf("转存完成：成功%d个，失败%d个", successCount, failCount), 5)
			})
		}
		b.states.ClearState(userID)
		return
	}

	// 磁力链接
	fullText := text + "\n" + caption
	result := b.addMagnetLinks(msg, fullText, fmt.Sprintf("%d", b.env.GetInt("ENV_123_MAGNET_UPLOAD_PID", 0)))
	if result.Status == "success" {
		successCount := 0
		failCount := 0
		var failMessages []string
		for _, item := range result.Data {
			if magnetOK(item.Response) {
				successCount++
			} else {
				failCount++
				failMessages = append(failMessages, fmt.Sprintf("\n%s: %s", item.Link, magnetMessage(item.Response)))
			}
		}
		log.Printf("123磁力链接添加结果: 成功%d个, 失败%d个", successCount, failCount)
		if failCount > 0 {
			b.SubmitSend(func() {
				b.SendReply(msg, fmt.Sprintf("123磁力链接添加部分失败: 成功%d个, 失败%d个\n失败详情: %s", successCount, failCount, strings.Join(failMessages, ", ")))
			})
		} else {
			b.SubmitSend(func() {
				b.SendReply(msg, fmt.Sprintf("123磁力链接添加成功: 共添加了%d个链接", successCount))
			})
		}
		b.states.ClearState(userID)
		return
	}
	if result.Message != "未找到磁力链接" {
		b.SubmitSend(func() { b.SendReplyDelete(msg, "123磁力链接添加失败: "+result.Message, 5) })
		b.states.ClearState(userID)
		return
	}

	if strings.Contains(text, "提取码") && strings.Contains(text, "www.123") {
		b.SubmitSend(func() {
			b.SendReply(msg, "仅支持形如 https://www.123pan.com/s/abcde-fghi?pwd=ABCD 的提取码格式")
		})
		return
	}

	// 123 分享链接转存（caption 与 text 一并提取，资源卡片链接常在 caption）
	targetURLs := extractTargetURL(fullText)
	if len(targetURLs) > 0 {
		b.SubmitSend(func() {
			b.SendReplyDelete(msg, fmt.Sprintf("发现%d个123分享链接，开始转存...", len(targetURLs)), 5)
		})
		successCount := 0
		failCount := 0
		for _, url := range targetURLs {
			if b.transferSharedLinkOptimize(url, b.env.GetInt("ENV_123_LINK_UPLOAD_PID", 0)) {
				successCount++
				log.Printf("转存成功: %s", url)
				b.triggerTransferAfterSave(b.env.GetInt("ENV_123_LINK_UPLOAD_PID", 0))
			} else {
				failCount++
				log.Printf("转存失败: %s", url)
			}
		}
		if failCount == 0 {
			b.SubmitSend(func() {
				b.SendReplyDelete(msg, fmt.Sprintf("转存完成：成功%d个，失败%d个", successCount, failCount), 5)
			})
		} else {
			b.SubmitSend(func() {
				b.SendReply(msg, fmt.Sprintf("转存完成：成功%d个，失败%d个", successCount, failCount))
			})
		}
		b.states.ClearState(userID)
		return
	}

	// 夸克分享转存
	kuakeURLs := extractKuakeTargetURL(fullText)
	if len(kuakeURLs) > 0 {
		cookie := b.env.Get("ENV_KUAKE_COOKIE", "")
		if cookie == "" {
			b.SubmitSend(func() { b.SendReply(msg, "请填写夸克COOKIE") })
			return
		}
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("发现%d个夸克分享链接，开始尝试秒传到123...", len(kuakeURLs)))
		})
		successCount := 0
		failCount := 0
		var failMessages []string
		qc := quark.New(cookie)
		for _, url := range kuakeURLs {
			jsonData, err := qc.ExportShareInfo(url, cookie)
			if err != nil {
				failCount++
				failMessages = append(failMessages, url+": "+err.Error())
				continue
			}
			files, ok := jsonData["files"].([]any)
			if !ok || len(files) == 0 {
				failCount++
				failMessages = append(failMessages, url+": 未获取到文件信息")
				continue
			}
			s, f := b.saveJSONData(msg, jsonData, 0, nil, nil, "夸克")
			successCount += s
			failCount += f
			b.triggerTransferAfterSave(b.env.GetInt("ENV_123_KUAKE_UPLOAD_PID", 0))
		}
		_ = successCount
		if failCount > 0 {
			b.SubmitSend(func() { b.SendReply(msg, "夸克转存部分失败\n"+strings.Join(failMessages, "\n")) })
		}
		b.states.ClearState(userID)
		return
	}

	// 115 分享转存
	urls115 := extract115TargetURL(fullText)
	if len(urls115) > 0 {
		targetPID := b.env.GetInt("ENV_123_115_UPLOAD_PID", 0)
		if targetPID == 0 {
			b.SubmitSend(func() {
				b.SendReply(msg, "请配置115转存目标文件夹ID（ENV_123_115_UPLOAD_PID）")
			})
			return
		}
		litepanClient := b.initLitepanClient()
		if litepanClient == nil {
			b.SubmitSend(func() {
				b.SendReply(msg, "⚠️ Litepan token 不存在或已失效，本次转存中止。已自动向管理员发起认证请求，认证完成后请重新发送 115 链接。")
			})
			b.SubmitSend(func() { b.triggerOAuthFor115() })
			b.states.ClearState(userID)
			return
		}
		b.SubmitSend(func() {
			b.SendReply(msg, fmt.Sprintf("发现%d个115分享链接，开始尝试秒传到123...", len(urls115)))
		})
		successCount := 0
		failCount := 0
		var failMessages []string
		ec := elevenfive.New()
		for _, url := range urls115 {
			jsonData := ec.ExportShareInfo(url)
			if jsonData != nil {
				if files, ok := jsonData["files"].([]any); ok && len(files) > 0 {
					s, f := b.saveJSONData(msg, jsonData, targetPID, nil, litepanClient, "115")
					successCount += s
					failCount += f
					b.triggerTransferAfterSave(targetPID)
				} else {
					failCount++
					failMessages = append(failMessages, url+": 未获取到文件信息或链接无效")
				}
			} else {
				failCount++
				failMessages = append(failMessages, url+": 未获取到文件信息或链接无效")
			}
		}
		_ = successCount
		if failCount > 0 {
			b.SubmitSend(func() { b.SendReply(msg, "115转存部分失败\n"+strings.Join(failMessages, "\n")) })
		}
		b.states.ClearState(userID)
		return
	}

	// 会话状态处理（删除确认）
	state, data := b.states.GetState(userID)
	if state == "CONFIRM_DELETE" {
		b.handleDeleteConfirm(msg, data)
	}
}

// triggerTransferAfterSave 转存成功后触发自动整理（事件驱动）。
func (b *Bot) triggerTransferAfterSave(targetPID int) {
	if b.env.GetBool("ENV_TRANSFER_ENABLED", false) && targetPID > 0 {
		transfer.TriggerTransferAfterSave(targetPID)
	}
}

// triggerOAuthFor115 收到115链接但无 Litepan token 时，自动在后台发起认证并私发 URL。
func (b *Bot) triggerOAuthFor115() {
	client := getOAuthClient()
	authData, err := client.StartAuth(context.Background())
	if err != nil {
		b.SubmitSend(func() { b.SendMessage(fmt.Sprintf("❌ Litepan 自动认证失败：%v", err)) })
		return
	}
	sessionID, _ := authData["session_id"].(string)
	authURL, _ := authData["oauth_url"].(string)
	if authURL == "" {
		authURL, _ = authData["auth_url"].(string)
	}
	if sessionID == "" || authURL == "" {
		return
	}
	b.SubmitSend(func() {
		b.SendMessage(fmt.Sprintf("🔔 收到115链接但 Litepan token 失效，请完成授权以启用 115→123 秒传：\n%s", authURL))
	})
	tokenData, err := client.PollStatus(context.Background(), sessionID, 60)
	if err != nil {
		b.SubmitSend(func() { b.SendMessage(fmt.Sprintf("❌ Litepan 自动认证失败：%v", err)) })
		return
	}
	client.AccessToken = toString(tokenData["access_token"])
	client.RefreshToken = toString(tokenData["refresh_token"])
	expiresIn, _ := toInt(tokenData["expires_in"])
	client.TokenExpiresAt = time.Now().Unix() + int64(expiresIn)
	oauthSaveToken(client)
	client.ConfirmReceived(context.Background(), sessionID)
	b.SubmitSend(func() { b.SendMessage("✅ Litepan OAuth 认证成功，现在可以重新发送 115 链接秒传了") })
}

// saveShareLinkMsg 秒传链接转存（对应 Python parse_share_link 消息处理器）。
func (b *Bot) saveShareLinkMsg(msg *tgbotapi.Message, shareLink string) {
	entries := parseShareLink(shareLink)
	if len(entries) == 0 {
		return
	}
	files := make([]FileItem, 0, len(entries))
	usesV2 := false
	for _, e := range entries {
		files = append(files, FileItem{Path: e.Path, Etag: e.Etag, Size: e.Size})
		if e.IsV2Etag {
			usesV2 = true
		}
	}
	targetPID := b.env.GetInt("ENV_123_JSON_UPLOAD_PID", 0)
	b.saveFilesTo123(msg, "", files, len(files), 0, usesV2, false, targetPID, nil, nil, "秒传")
}

// saveJSONData 解析 JSON 格式秒传数据并转存（夸克/115/JSON 文件共用）。
func (b *Bot) saveJSONData(msg *tgbotapi.Message, jsonData map[string]any, targetPID int, client, uploadClient *pan123.Client, sourcePlatform string) (int, int) {
	commonPath := trimSpace(toString(jsonData["commonPath"]))
	commonPath = strings.TrimSuffix(commonPath, "/")
	rawFiles, _ := jsonData["files"].([]any)
	usesV2 := toBool(jsonData["usesBase62EtagsInExport"])
	isSHA1 := sourcePlatform == "115"
	totalFilesCount := toIntDefault(jsonData["totalFilesCount"], len(rawFiles))
	totalSizeJSON := toInt64Default(jsonData["totalSize"], 0)

	var files []FileItem
	for _, rf := range rawFiles {
		m, ok := rf.(map[string]any)
		if !ok {
			continue
		}
		files = append(files, FileItem{
			Path: toString(m["path"]),
			Etag: toString(m["etag"]),
			Size: toInt64(m["size"]),
		})
	}
	if targetPID == 0 {
		targetPID = b.env.GetInt("ENV_123_KUAKE_UPLOAD_PID", 0)
	}
	return b.saveFilesTo123(msg, commonPath, files, totalFilesCount, totalSizeJSON, usesV2, isSHA1, targetPID, client, uploadClient, sourcePlatform)
}

// processTextDocument 处理非 JSON 文档（纯文本链接转存）。
func (b *Bot) processTextDocument(msg *tgbotapi.Message) {
	linkProcessMu.Lock()
	defer linkProcessMu.Unlock()
	if !b.isAdmin(msg) {
		return
	}
	log.Printf("进入处理文档文件")
	content, err := b.downloadDocument(msg)
	if err != nil {
		b.SubmitSend(func() { b.SendReply(msg, fmt.Sprintf("处理文档文件失败: %v", err)) })
		return
	}
	log.Printf("文档内容: %s", truncateStr(string(content), 500))
	links := extractTargetURL(string(content))
	if len(links) == 0 {
		b.SubmitSend(func() { b.SendReply(msg, "文档中未找到链接") })
		return
	}
	log.Printf("从文档中提取到%d个链接", len(links))
	successCount := 0
	failCount := 0
	for _, link := range links {
		if b.transferSharedLinkOptimize(link, b.env.GetInt("ENV_123_LINK_UPLOAD_PID", 0)) {
			successCount++
		} else {
			failCount++
		}
	}
	b.SubmitSend(func() {
		b.SendReply(msg, fmt.Sprintf("文档链接转存完成：成功%d个，失败%d个", successCount, failCount))
	})
}

// processJSONFile 处理 JSON 秒传文件。
func (b *Bot) processJSONFile(msg *tgbotapi.Message) {
	linkProcessMu.Lock()
	defer linkProcessMu.Unlock()
	if !b.isAdmin(msg) {
		return
	}
	log.Printf("进入转存json")
	content, err := b.downloadDocument(msg)
	if err != nil {
		b.SubmitSend(func() { b.SendReply(msg, fmt.Sprintf("❌ 处理JSON文件失败:\n%v", err)) })
		return
	}

	// 判断并转换不同的 JSON 格式
	commonPath := ""
	var files []FileItem
	usesV2 := false
	totalFilesCount := 0
	totalSizeJSON := int64(0)

	var raw any
	if err := json.Unmarshal(content, &raw); err != nil {
		b.SubmitSend(func() { b.SendReply(msg, "❌ JSON 解析失败") })
		return
	}
	if arr, ok := raw.([]any); ok {
		// 格式2: 数组格式 [[etag, size, filename], ...]
		log.Printf("检测到数组格式的妙传文件")
		for _, item := range arr {
			row, ok := item.([]any)
			if !ok || len(row) < 3 {
				continue
			}
			etag := toString(row[0])
			size := toInt64(row[1])
			filename := toString(row[2])
			files = append(files, FileItem{Path: filename, Etag: etag, Size: size})
			totalSizeJSON += size
		}
		totalFilesCount = len(files)
	} else if m, ok := raw.(map[string]any); ok {
		// 格式1: 对象格式
		log.Printf("检测到对象格式的妙传文件")
		commonPath = strings.TrimSuffix(trimSpace(toString(m["commonPath"])), "/")
		rawFiles, _ := m["files"].([]any)
		for _, rf := range rawFiles {
			fm, ok := rf.(map[string]any)
			if !ok {
				continue
			}
			files = append(files, FileItem{
				Path: toString(fm["path"]),
				Etag: toString(fm["etag"]),
				Size: toInt64(fm["size"]),
			})
		}
		usesV2 = toBool(m["usesBase62EtagsInExport"])
		totalFilesCount = toIntDefault(m["totalFilesCount"], len(files))
		totalSizeJSON = toInt64Default(m["totalSize"], 0)
	}

	if len(files) == 0 {
		b.SubmitSend(func() { b.SendReply(msg, "JSON文件中没有找到文件信息。") })
		return
	}

	targetPID := b.env.GetInt("ENV_123_JSON_UPLOAD_PID", 0)
	b.saveFilesTo123(msg, commonPath, files, totalFilesCount, totalSizeJSON, usesV2, false, targetPID, nil, nil, "JSON")
}

// downloadDocument 从 Telegram 下载文档内容。
func (b *Bot) downloadDocument(msg *tgbotapi.Message) ([]byte, error) {
	if b.tg == nil || msg.Document == nil {
		return nil, fmt.Errorf("无法获取文档")
	}
	var filePath string
	fileRetry := 0
	for {
		file, err := b.tg.GetFile(tgbotapi.FileConfig{FileID: msg.Document.FileID})
		if err == nil {
			filePath = file.FilePath
			break
		}
		fileRetry++
		if fileRetry >= 10 {
			return nil, fmt.Errorf("从TG获取文件失败: %v", err)
		}
		log.Printf("从TG获取文件失败，尝试重试: %v", err)
		time.Sleep(30 * time.Second)
	}
	fileURL := "https://api.telegram.org/file/bot" + b.token + "/" + filePath
	data, _, err := tgFileHTTP.Get(context.Background(), fileURL, nil)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true") || b == "1"
	case float64:
		return b != 0
	}
	return false
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		var out int
		if _, err := fmt.Sscanf(n, "%d", &out); err == nil {
			return out, true
		}
	}
	return 0, false
}

func toIntDefault(v any, def int) int {
	if n, ok := toInt(v); ok {
		return n
	}
	return def
}

func toInt64(v any) int64 {
	if n, ok := toInt(v); ok {
		return int64(n)
	}
	return 0
}

func toInt64Default(v any, def int64) int64 {
	if n, ok := toInt(v); ok {
		return int64(n)
	}
	return def
}
