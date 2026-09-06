package wrapper

import (
	"runtime"
	"runtime/metrics"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// GC 看门狗。
//
// 观察到的现象（Go 1.24 wasip1、Envoy/V8 宿主、请求体流式转发、单 worker 数百个在途请求）：
// 某个 worker 的 Go 运行时会停止触发 GC——堆超过 GC 目标数倍仍不回收，线性内存一路涨到上限、
// 分配失败、请求报错；同样的负载在另一个 worker 上却正常。分配大多发生在宿主再入回调
// proxy_on_memory_allocate（宿主往 wasm 内存拷贝 body 分块）里，这是 Go 运行时在宿主驱动、
// 无独立调度线程的环境下的薄弱环节（参见 golang/go#69584、#78513 这类 wasmexport / 宿主相关的 GC 问题）。
//
// 这里在顶层 body 回调里每隔若干次用 runtime/metrics 读一次堆大小（无 STW、纳秒级），
// 超过"上次回收后存活量 × GCWatchdogFactor（下限 GCWatchdogFloor）"就主动 runtime.GC()，
// 相当于把 GOGC=100 的语义在运行时失灵时兜住。正常情况下 Go 自己的 GC 会先于它触发，看门狗不会动作。
// 实测（1MB 请求体流式转发）：400 并发时运行时自己的 GC 正常，堆 ~25MB；≥800 并发时几乎只靠看门狗，
// 堆停在下限附近——下限因此取 64MB（存活量只有十几 MB，强制 GC 的代价与存活量成正比）。
var (
	// GCWatchdogEnabled 关掉看门狗（插件可在 init 里设置）。
	GCWatchdogEnabled = true
	// GCWatchdogFloor 堆低于这个值时永不主动 GC。
	GCWatchdogFloor uint64 = 64 << 20
	// GCWatchdogFactor 主动 GC 的阈值 = 上次回收后的存活量 × Factor。
	GCWatchdogFactor = 2.0
	// GCWatchdogEvery 每这么多次 body 回调检查一次。
	GCWatchdogEvery uint64 = 64

	gcWatchCalls    uint64
	gcWatchLastLive uint64
	gcWatchSamples  = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

	// 可替换，便于测试
	gcWatchHeapBytes = func() uint64 {
		metrics.Read(gcWatchSamples)
		if gcWatchSamples[0].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return gcWatchSamples[0].Value.Uint64()
	}
	gcWatchForce = runtime.GC
	gcWatchLog   = func(format string, args ...interface{}) { proxywasm.LogWarnf(format, args...) }
)

func gcWatchdog() {
	if !GCWatchdogEnabled {
		return
	}
	gcWatchCalls++
	if gcWatchCalls%GCWatchdogEvery != 0 {
		return
	}
	heap := gcWatchHeapBytes()
	goal := uint64(float64(gcWatchLastLive) * GCWatchdogFactor)
	if goal < GCWatchdogFloor {
		goal = GCWatchdogFloor
	}
	if heap < goal {
		return
	}
	gcWatchForce()
	live := gcWatchHeapBytes()
	gcWatchLastLive = live
	gcWatchLog("gc watchdog: heap %dMB exceeded goal %dMB without a GC cycle, forced GC, live now %dMB", heap>>20, goal>>20, live>>20)
}
