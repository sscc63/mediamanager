package transfer

// 整理定时调度器（对应 transfer_scheduler.py）。
// 定时扫描配置为 monitor=true 的源目录，防止重叠执行。

import (
	"context"
	"log"
	"sync"
	"time"
)

// TransferScheduler 整理定时调度器。
type TransferScheduler struct {
	executor *TransferExecutor

	scanInterval time.Duration // 扫描间隔，0 表示不自动扫描

	stopCh chan struct{}
	once   sync.Once

	runningMu sync.Mutex
	running   bool // 是否正在执行整理（防止重叠）

	nextMu  sync.Mutex
	nextRun time.Time // 下次定时扫描时间（零值=未启用/未 Start）

	// OnTransferCompleted 整理完成后联动回调（例如生成 STRM），可空。
	OnTransferCompleted func(stats *TransferStats)
}

// NewTransferScheduler 创建整理定时调度器。
func NewTransferScheduler(executor *TransferExecutor, scanIntervalMin int) *TransferScheduler {
	return &TransferScheduler{
		executor:     executor,
		scanInterval: time.Duration(scanIntervalMin) * time.Minute,
		stopCh:       make(chan struct{}),
	}
}

// Start 启动定时扫描（在后台 goroutine 中运行）。
func (s *TransferScheduler) Start() {
	if s.scanInterval <= 0 {
		log.Printf("🚫 整理定时扫描未启用（scan_interval<=0）")
		return
	}
	s.once = sync.Once{}
	stop := make(chan struct{})
	s.stopCh = stop
	go s.runLoop(stop)
	s.setNextRun(time.Now().Add(s.scanInterval))
	log.Printf("✅ 整理调度器已启动，每 %v 扫描一次", s.scanInterval)
}

// Stop 停止定时扫描。
func (s *TransferScheduler) Stop() {
	if s.stopCh != nil {
		close(s.stopCh)
		s.stopCh = nil
	}
	log.Printf("🛑 整理调度器已停止")
}

func (s *TransferScheduler) runLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(s.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.safeScanAll()
			s.setNextRun(time.Now().Add(s.scanInterval))
		case <-stop:
			return
		}
	}
}

func (s *TransferScheduler) setNextRun(t time.Time) {
	s.nextMu.Lock()
	s.nextRun = t
	s.nextMu.Unlock()
}

// safeScanAll 安全的扫描所有监控目录（防止重叠执行）。
func (s *TransferScheduler) safeScanAll() {
	s.runningMu.Lock()
	if s.running {
		s.runningMu.Unlock()
		log.Printf("上一轮整理尚未完成，跳过本次扫描")
		return
	}
	s.running = true
	s.runningMu.Unlock()

	defer func() {
		s.runningMu.Lock()
		s.running = false
		s.runningMu.Unlock()
	}()
	s.ScanAll()
}

// ScanAll 扫描所有配置为 monitor=true 的源目录，返回汇总统计。
func (s *TransferScheduler) ScanAll() map[string]int {
	dirs := s.executor.DirHelper.GetMonitorDirs()
	total := map[string]int{"total_dirs": 0, "success": 0, "fail": 0, "skip": 0}
	if len(dirs) == 0 {
		log.Printf("无监控目录配置，跳过扫描")
		return total
	}
	total["total_dirs"] = len(dirs)
	log.Printf("开始扫描 %d 个监控目录", len(dirs))

	for _, d := range dirs {
		log.Printf("扫描整理: %s (source_pid=%d)", d.Name, d.SourcePID)
		ctx := context.Background()
		stats := s.executor.TransferDirectory(ctx, d.SourcePID, true, false, nil, d.TransferType, true, nil)
		total["success"] += stats.Success
		total["fail"] += stats.Fail
		total["skip"] += stats.Skip
		// 整理完成后联动生成 STRM（若 STRM 功能启用）
		if s.OnTransferCompleted != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("整理联动 STRM 生成失败: %v", r)
					}
				}()
				s.OnTransferCompleted(stats)
			}()
		}
	}
	log.Printf("全部扫描完成: %v", total)
	return total
}

// ScanNow 手动触发扫描。sourcePID<=0 时扫描所有监控目录。
func (s *TransferScheduler) ScanNow(sourcePID int) map[string]int {
	if sourcePID > 0 {
		ctx := context.Background()
		stats := s.executor.TransferDirectory(ctx, sourcePID, true, false, nil, "", false, nil)
		result := map[string]int{
			"total_dirs": 1,
			"success":    stats.Success,
			"fail":       stats.Fail,
			"skip":       stats.Skip,
		}
		return result
	}
	return s.ScanAll()
}

// IsScanning 是否正在执行整理。
func (s *TransferScheduler) IsScanning() bool {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	return s.running
}

// IsRunning 调度器是否已启动（scanInterval>0 且已 Start）。
func (s *TransferScheduler) IsRunning() bool {
	return s != nil && s.scanInterval > 0 && s.stopCh != nil
}

// ScanInterval 返回扫描间隔（分钟）。
func (s *TransferScheduler) ScanInterval() int {
	if s == nil {
		return 0
	}
	return int(s.scanInterval / time.Minute)
}

// NextRun 返回下次定时扫描时间；未启用或未 Start 时返回零值。
func (s *TransferScheduler) NextRun() time.Time {
	if s == nil || !s.IsRunning() {
		return time.Time{}
	}
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	return s.nextRun
}
