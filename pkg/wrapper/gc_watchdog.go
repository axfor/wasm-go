package wrapper

import (
	"runtime"
	"runtime/metrics"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// GC watchdog.
//
// Observed behaviour (Go 1.24 wasip1, Envoy/V8 host, streaming request body forwarding, hundreds of in-flight requests on one worker):
// the Go runtime on one worker stops triggering GC: the heap grows to several times the GC target without a collection,
// linear memory climbs to its limit, allocations fail and requests error out, while the same load is fine on another worker.
// Most allocations happen inside the host re-entrant callback proxy_on_memory_allocate (the host copying body chunks into
// wasm memory), a weak spot of the Go runtime when it is host-driven with no scheduler thread of its own (see
// wasmexport / host related GC issues such as golang/go#69584 and #78513).
//
// Every few top-level body callbacks this reads the heap size through runtime/metrics (no STW, nanoseconds) and calls
// runtime.GC() itself once the heap exceeds "live bytes after the last collection × GCWatchdogFactor" (never below
// GCWatchdogFloor), which restores the GOGC=100 semantics when the runtime's own trigger fails. Normally Go's GC
// fires first and the watchdog never acts.
// Measured (1MB request bodies forwarded chunk by chunk): at 400 concurrent requests the runtime's own GC works and the heap
// stays around 25MB; at 800 and above almost every collection comes from the watchdog and the heap settles near
// the floor, hence a floor of 64MB (live data is a dozen MB, and a forced GC costs in proportion to live data).
var (
	// GCWatchdogEnabled turns the watchdog off (a plugin may set it in init).
	GCWatchdogEnabled = true
	// GCWatchdogFloor: no forced GC while the heap is below this size.
	GCWatchdogFloor uint64 = 64 << 20
	// GCWatchdogFactor: forced GC threshold = live bytes after the last collection × Factor.
	GCWatchdogFactor = 2.0
	// GCWatchdogEvery: check once every this many body callbacks.
	GCWatchdogEvery uint64 = 64

	gcWatchCalls    uint64
	gcWatchLastLive uint64
	gcWatchSamples  = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

	// replaceable for tests
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
