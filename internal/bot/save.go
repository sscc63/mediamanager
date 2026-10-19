package bot

// 核心转存逻辑（对应 123bot.py 的 save_json_file_quark / process_json_file / parse_share_link
// 中的文件秒传循环，供 JSON 文件、夸克、115、秒传链接共用）。

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"mmbot/internal/pan123"
	"mmbot/internal/transfer"
)

// FileItem 秒传文件条目（path/etag/size）。
type FileItem struct {
	Path string
	Etag string
	Size int64
}

// SaveBatchMsg 转存进度消息条目。
type SaveBatchMsg struct {
	Status string // ✅ ❌ 🔄
	Dir    string
	File   string
}

// SaveStats 转存统计。
type SaveStats struct {
	Success   int
	Fail      int
	Skip      int
	TotalSize int64
	FailList  []string
}

// saveFilesTo123 秒传一组文件到目标目录（夸克/115/JSON/秒传链接共用）。
//
//	client:      建目录用客户端（一般为主程序 client）
//	uploadClient: 秒传上传客户端（115→123 时为 Litepan client，其他场景同 client）
//
// 返回 (成功数, 失败数)。
func (b *Bot) saveFilesTo123(msg *tgbotapi.Message, commonPath string, files []FileItem, totalFilesCount int, totalSizeJSON int64, usesV2Etag, isSHA1 bool, targetPID int, client, uploadClient *pan123.Client, sourcePlatform string) (int, int) {
	if len(files) == 0 {
		b.SubmitSend(func() { b.SendReply(msg, "分享中没有找到文件信息。") })
		return 0, 0
	}
	b.SubmitSend(func() {
		b.SendReplyDelete(msg, fmt.Sprintf("开始123转存文件中的%d个文件...", len(files)), 5)
	})

	if client == nil {
		client = b.initClient()
	}
	if uploadClient == nil {
		uploadClient = client
	}

	ctx := context.Background()
	stats := &SaveStats{}
	var msgBatch []SaveBatchMsg
	var lastEtag string
	folderCache := map[string]int64{}
	targetDirName := commonPath
	if targetDirName == "" {
		targetDirName = "JSON转存"
	}
	targetDirID := int64(targetPID)
	if targetDirID == 0 {
		targetDirID = int64(b.env.GetInt("ENV_123_KUAKE_UPLOAD_PID", 0))
	}

	filePerSec := b.env.GetFloat("ENV_FILE_PER_SECOND", 5)

	for _, fi := range files {
		filePath := fi.Path
		if commonPath != "" {
			filePath = commonPath + "/" + filePath
		}
		etag := fi.Etag
		size := fi.Size
		if filePath == "" || etag == "" || size == 0 {
			stats.Fail++
			stats.FailList = append(stats.FailList, orStr(filePath, "未知文件")+"：文件信息不完整")
			continue
		}

		pathParts := strings.Split(filePath, "/")
		fileName := pathParts[len(pathParts)-1]
		pathParts = pathParts[:len(pathParts)-1]
		parentID := targetDirID

		// 创建目录结构（带缓存）
		currentPath := ""
		for _, part := range pathParts {
			if part == "" {
				continue
			}
			currentPath = currentPath + "/" + part
			if currentPath[0] == '/' {
				currentPath = currentPath[1:]
			}
			cacheKey := fmt.Sprintf("%d/%s", parentID, currentPath)
			if id, ok := folderCache[cacheKey]; ok {
				parentID = id
				continue
			}
			folderID, err := mkdirWithRetry(ctx, client, part, parentID)
			if err != nil {
				log.Printf("创建文件夹失败: %s，将使用当前目录: %v", part, err)
			} else {
				folderCache[cacheKey] = folderID
				parentID = folderID
			}
		}

		// 处理 ETag
		if usesV2Etag {
			etag = optimizedEtagToHex(etag, true)
		}

		// 秒传文件（带重试）
		var rapidResp *pan123.Response
		rapidErr := ""
		retryCount := 3
		for retryCount > 0 {
			// 跳过同一目录下的重复文件
			if lastEtag == etag {
				stats.Skip++
				log.Printf("跳过重复文件: %s", filePath)
				rapidResp = skipMarker()
				break
			}
			var err error
			if isSHA1 {
				rapidResp, err = uploadClient.UploadSha1Reuse(ctx, map[string]any{
					"sha1":         etag,
					"size":         size,
					"filename":     fileName,
					"parentFileId": parentID,
					"duplicate":    1,
				})
			} else {
				rapidResp, err = uploadClient.UploadRequest(ctx, map[string]any{
					"etag":         robustNormalizeMD5(etag),
					"fileName":     fileName,
					"size":         size,
					"parentFileId": parentID,
					"duplicate":    1,
				})
			}
			if err != nil {
				rapidErr = err.Error()
				retryCount--
				log.Printf("转存文件 %s 失败 (剩余重试: %d): %v", fileName, retryCount, err)
				time.Sleep(31 * time.Second)
				continue
			}
			if rapidResp.Code != 0 {
				rapidErr = rapidResp.MessageString()
				retryCount--
				log.Printf("转存文件 %s 失败 (剩余重试: %d): %s", fileName, retryCount, rapidErr)
				// 同名文件 / Etag 相关错误不再重试
				if strings.Contains(rapidErr, "同名文件") || strings.Contains(rapidErr, "Etag") {
					break
				}
				time.Sleep(31 * time.Second)
				continue
			}
			break
		}

		dirPath := dirOf(filePath)
		switch classifySave(rapidResp) {
		case saveSkip:
			msgBatch = append(msgBatch, SaveBatchMsg{Status: "🔄", Dir: dirPath, File: fileName + " (重复跳过)"})
		case saveOK:
			lastEtag = etag
			stats.Success++
			stats.TotalSize += size
			msgBatch = append(msgBatch, SaveBatchMsg{Status: "✅", Dir: dirPath, File: fileName})
		default:
			stats.Fail++
			errMsg := rapidErr
			if errMsg == "" {
				if rapidResp != nil && rapidResp.Code != 0 {
					errMsg = rapidResp.MessageString()
				} else {
					errMsg = "此文件在123服务器不存在，无法秒传"
				}
			}
			stats.FailList = append(stats.FailList, fmt.Sprintf("• %s（失败原因：%s）", filePath, errMsg))
			msgBatch = append(msgBatch, SaveBatchMsg{Status: "❌", Dir: dirPath, File: fmt.Sprintf("%s (%s)", fileName, errMsg)})
		}

		// 每 10 条进度消息发送一次
		if len(msgBatch) >= 10 {
			b.SubmitSend(func() { b.SendReplyDelete(msg, batchProgressMsg(len(msgBatch), totalFilesCount, msgBatch), 5) })
			msgBatch = nil
		}
		time.Sleep(time.Duration(float64(time.Second) / filePerSec))
	}

	// 发送剩余进度
	if len(msgBatch) > 0 {
		b.SubmitSend(func() { b.SendReplyDelete(msg, batchProgressMsg(len(msgBatch), totalFilesCount, msgBatch), 5) })
	}

	// 结果汇总
	successCount := stats.Success
	failCount := stats.Fail
	sizeStr := FormatSize(stats.TotalSize, "GB")
	avgSize := int64(0)
	if successCount > 0 {
		avgSize = stats.TotalSize / int64(successCount)
	}
	avgStr := FormatSize(avgSize, "B")
	totalSizeJSONStr := FormatSize(totalSizeJSON, "GB")
	targetName := "夸克"
	if sourcePlatform != "" {
		targetName = sourcePlatform
	}
	resultMsg := fmt.Sprintf("✅ 123转存%s完成！\n✅成功: %d个\n❌失败: %d个\n🔄跳过同一目录下的重复文件: %d个\n📊成功转存体积: %s\n📊平均文件大小: %s\n📝%s分享理论文件数: %d个\n⏱️耗时: 已统计",
		targetName, successCount, failCount, stats.Skip, sizeStr, avgStr, targetName, totalFilesCount)
	_ = totalSizeJSONStr
	b.SubmitSend(func() { b.SendReply(msg, resultMsg) })

	// 转存成功后触发自动整理（事件驱动）
	if successCount > 0 && targetPID > 0 && b.env.GetBool("ENV_TRANSFER_ENABLED", false) {
		transfer.TriggerTransferAfterSave(targetPID)
	}

	// 失败文件详情（每批 10 条）
	if failCount > 0 {
		for i := 0; i < len(stats.FailList); i += 10 {
			end := i + 10
			if end > len(stats.FailList) {
				end = len(stats.FailList)
			}
			batch := stats.FailList[i:end]
			batchMsg := fmt.Sprintf("❌ 失败文件 (批次 %d/%d):\n%s", (i/10)+1, (len(stats.FailList)+9)/10, strings.Join(batch, "\n"))
			b.SubmitSend(func() { b.SendReply(msg, batchMsg) })
			time.Sleep(500 * time.Millisecond)
		}
	}

	return successCount, failCount
}

// saveStatus 转存结果分类。
type saveStatus int

const (
	saveFail saveStatus = iota
	saveOK
	saveSkip
)

func skipMarker() *pan123.Response {
	return &pan123.Response{Code: 0, Data: []byte(`{"Reuse":true,"Skip":true}`)}
}

func classifySave(resp *pan123.Response) saveStatus {
	if resp == nil {
		return saveFail
	}
	if !resp.IsSuccess() {
		return saveFail
	}
	// 兼容 Reuse/reuse 大小写两种字段（不同秒传接口返回键名不同）
	var m map[string]any
	if len(resp.Data) > 0 {
		_ = jsonUnmarshal(resp.Data, &m)
	}
	reuse := toBool(m["Reuse"]) || toBool(m["reuse"])
	if !reuse {
		return saveFail
	}
	if toBool(m["Skip"]) {
		return saveSkip
	}
	return saveOK
}

// batchProgressMsg 生成批次进度消息（树状结构）。
func batchProgressMsg(batchSize, total int, batch []SaveBatchMsg) string {
	percent := 0
	if total > 0 {
		percent = batchSize * 100 / total
	}
	return fmt.Sprintf("📊 %d/%d (%d%%) 个文件已处理\n\n%s", batchSize, total, percent, buildTreeMsg(batch))
}

// buildTreeMsg 将 [{status, dir, file}] 组装为树状结构消息文本。
func buildTreeMsg(batch []SaveBatchMsg) string {
	type group struct {
		ok  []string
		bad []string
		rot []string
	}
	m := map[string]*group{}
	var order []string
	for _, e := range batch {
		g, ok := m[e.Dir]
		if !ok {
			g = &group{}
			m[e.Dir] = g
			order = append(order, e.Dir)
		}
		switch e.Status {
		case "✅":
			g.ok = append(g.ok, e.File)
		case "❌":
			g.bad = append(g.bad, e.File)
		default:
			g.rot = append(g.rot, e.File)
		}
	}
	sort.Strings(order)
	var lines []string
	for _, dir := range order {
		g := m[dir]
		for _, item := range [][2]string{{"✅", ""}, {"❌", ""}, {"🔄", ""}} {
			var files []string
			switch item[0] {
			case "✅":
				files = g.ok
			case "❌":
				files = g.bad
			default:
				files = g.rot
			}
			if len(files) == 0 {
				continue
			}
			lines = append(lines, fmt.Sprintf("--- %s %s", item[0], dir))
			for i, f := range files {
				prefix := "      └──"
				if i < len(files)-1 {
					prefix = "      ├──"
				}
				lines = append(lines, fmt.Sprintf("%s %s", prefix, f))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// mkdirWithRetry 创建文件夹（3 次重试，失败 sleep 31），全部失败返回错误。
func mkdirWithRetry(ctx context.Context, client *pan123.Client, part string, parentID any) (int64, error) {
	retryCount := 3
	for retryCount > 0 {
		resp, err := client.FSMkdir(ctx, part, parentID, 1)
		time.Sleep(200 * time.Millisecond)
		if err == nil && resp.Code == 0 {
			var data struct {
				Info struct {
					FileID int64 `json:"FileId"`
				} `json:"Info"`
			}
			if len(resp.Data) > 0 {
				_ = jsonUnmarshal(resp.Data, &data)
			}
			if data.Info.FileID != 0 {
				return data.Info.FileID, nil
			}
		}
		retryCount--
		if err == nil && resp != nil {
			log.Printf("创建文件夹 %s 失败 (剩余重试: %d): %s", part, retryCount, resp.MessageString())
		} else if err != nil {
			log.Printf("创建文件夹 %s 失败 (剩余重试: %d): %v", part, retryCount, err)
		}
		time.Sleep(31 * time.Second)
	}
	return 0, fmt.Errorf("创建文件夹 %s 失败（重试耗尽）", part)
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// dirOf 返回路径的目录部分（不含文件名）。
func dirOf(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		if i == 0 {
			return "/"
		}
		return p[:i]
	}
	return ""
}
