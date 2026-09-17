// Package gcguard 提供「任务边界主动回收内存」的统一入口。
//
// 为什么必须节流：debug.FreeOSMemory 是一次完整的 STW GC，随后把空闲内存归还给操作系统，
// 代价很高。若挂在轮询接口（/api/emby/status 前端每 2 秒一次）或事件驱动的整理任务上，
// 不节流等于每隔几秒强制 GC 一次，RSS 被反复打到低位再涨回来，白烧 CPU。
// 因此全进程共用同一个最小间隔：密集触发时只有第一次真正执行。
package gcguard

import (
	"runtime/debug"
	"sync"
	"time"
)

// interval 两次真正回收之间的最小间隔；测试会临时改小。
var interval = 5 * time.Minute

var (
	mu   sync.Mutex
	last time.Time
)

// due 判断此刻是否到达回收间隔；到达则记录时间并返回 true。
func due() bool {
	mu.Lock()
	defer mu.Unlock()
	if !last.IsZero() && time.Since(last) < interval {
		return false
	}
	last = time.Now()
	return true
}

// Reclaim 到达最小间隔时执行一次内存回收，否则直接返回。
//
// 只在「一批大对象刚刚全部失去引用」的任务边界调用：例如一轮频道扫描结束后、
// 一次顶层整理任务结束后。不要在循环体、单条消息处理、单个文件处理里调用 ——
// 那些位置大对象还活着，回收不到东西，只会白付一次 STW 的代价。
func Reclaim() {
	if !due() {
		return
	}
	// FreeOSMemory 内部已包含一次完整 GC，不需要再单独 runtime.GC()。
	debug.FreeOSMemory()
}
