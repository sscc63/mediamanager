package bot

// 123 云盘客户端初始化（对应 123bot.py 的 init_123_client / init_litepan_client / get_oauth_client）。
// token 持久化到 config/config.txt；OAuth token 持久化到 db/data/oauth_token.json。

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mmbot/internal/oauth"
	"mmbot/internal/pan123"
)

// tokenFile 123 主客户端 token 持久化文件。
func (b *Bot) tokenFilePath() string {
	if p := b.env.Get("ENV_123_TOKEN_FILE", ""); p != "" {
		return p
	}
	return filepath.Join("config", "config.txt")
}

// initClient 初始化 123 主客户端（带 token 持久化与自动刷新）。
// 优先读取 config/config.txt 中的持久化 token；无效时通过 client_id/client_secret 重新获取。
func (b *Bot) initClient() *pan123.Client {
	if b.client != nil {
		return b.client
	}
	tokenPath := b.tokenFilePath()
	token := ""
	if data, err := os.ReadFile(tokenPath); err == nil {
		token = trimSpace(string(data))
	}

	// 尝试使用持久化 token
	if token != "" {
		for {
			c := pan123.New(token)
			resp, err := c.UserInfo(context.Background())
			if err != nil {
				log.Printf("token健康检查异常，稍后重试: %v", err)
				time.Sleep(60 * time.Second)
				continue
			}
			if !resp.IsSuccess() || resp.MessageString() != "ok" {
				b.SubmitSend(func() { b.SendMessage("123 token过期，将重新获取") })
				log.Printf("检测到token过期，将重新获取")
				_ = os.Remove(tokenPath)
				break
			}
			log.Printf("123客户端初始化成功（使用持久化token）")
			b.client = c
			return c
		}
	}

	// 通过 API 获取新 token
	clientID := b.env.Get("ENV_123_CLIENT_ID", "")
	clientSecret := b.env.Get("ENV_123_CLIENT_SECRET", "")
	initWithNew := func() *pan123.Client {
		c, err := pan123.NewWithSecret(clientID, clientSecret)
		if err != nil {
			log.Printf("获取123 token失败: %v", err)
			return nil
		}
		_ = os.MkdirAll(filepath.Dir(tokenPath), 0o755)
		_ = os.WriteFile(tokenPath, []byte(c.Token), 0o644)
		log.Printf("123客户端初始化成功（使用新获取的token）")
		return c
	}
	c := initWithNew()
	if c == nil {
		// 重试一次
		log.Printf("获取token失败，尝试重试...")
		c = initWithNew()
	}
	b.client = c
	return c
}

// oauthOnce 全局 Litepan OAuth 单例（线程安全懒加载）。
var (
	oauthOnce sync.Once
	oauthInst *oauth.Client
)

// getOAuthClient 获取全局 LitePanOAuthClient 单例。
func getOAuthClient() *oauth.Client {
	oauthOnce.Do(func() {
		oauthInst = oauth.New("123云盘Open")
	})
	return oauthInst
}

// initLitepanClient 用 Litepan OAuth token 创建独立的 P123Client（仅用于 115→123 SHA1秒传）。
// token 不存在、已失效或探活失败时返回 nil。
func (b *Bot) initLitepanClient() *pan123.Client {
	token, err := getOAuthClient().GetToken(context.Background())
	if err != nil || token == "" {
		log.Printf("获取 Litepan token 失败: %v", err)
		return nil
	}
	c := pan123.New(token)
	if resp, err := c.UserInfo(context.Background()); err == nil && resp.IsAuthError() {
		log.Printf("Litepan token 已被服务端拒绝，需重新授权")
		return nil
	}
	return c
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
