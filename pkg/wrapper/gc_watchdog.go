package wrapper

import (
	"fmt"
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
	// GCWatchdogEveryBytes: also check once this many body bytes have come in since the last check (0 = callbacks
	// only). Every body callback copies its chunk into the module, and a chunk is whatever the host hands over -- 16KB
	// or a whole request -- so a count of callbacks alone lets the heap overshoot by Every chunks of any size.
	GCWatchdogEveryBytes uint64 = 512 << 10
	// GCWatchdogProfile: log where the memory in use was allocated, after a forced collection whose live set is above
	// GCWatchdogProfileFloor. For finding out what a busy worker is holding; off by default, it walks the heap profile.
	// Only a build with the gcprofile tag has the profile to walk (see gcWatchTopSites).
	GCWatchdogProfile             = false
	GCWatchdogProfileFloor uint64 = 32 << 20

	gcWatchCalls    uint64
	gcWatchBytes    uint64
	gcWatchLastLive uint64
	gcWatchSamples  = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	// What the module has taken from the host and cannot give back: in wasm the linear memory only ever grows, so
	// where it went is worth saying when a collection is forced. Live objects are one part of it; spans that are free
	// but still mapped, the room lost inside spans, stacks and the runtime's own tables are the rest.
	// The true live set, as of the last completed collection. It is not the same as the heap class below, which also
	// counts objects that are dead but not yet swept -- and the sweeper lags exactly when allocation is heaviest.
	gcWatchLiveSample = []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	gcWatchBreakdown  = []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/stacks:bytes"},
		{Name: "/memory/classes/metadata/mspan/inuse:bytes"},
		{Name: "/memory/classes/metadata/mcache/inuse:bytes"},
		{Name: "/memory/classes/metadata/other:bytes"},
	}

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

// GCWatchdogCheckNow runs the check straight away instead of on the next scheduled one. For a callback that has just
// allocated far more than a body chunk -- a request released in one piece after waiting, say -- where the allocation
// of the callbacks in between would carry the heap well past its goal before the next check, and in wasm the linear
// memory never gives back what it grew to.
func GCWatchdogCheckNow() {
	if !GCWatchdogEnabled {
		return
	}
	gcWatchdogCheck()
}

// gcWatchTopSites logs the allocation sites holding the most memory when GCWatchdogProfile is on. It does nothing
// unless the build has the gcprofile tag (gc_watchdog_profile.go): a reference to runtime.MemProfile anywhere in a
// program keeps the runtime's memory profiler on, and its bucket table alone is 1.4MB of linear memory, which a wasm
// module never gives back.
var gcWatchTopSites = func(live uint64) {}

// gcWatchMarkedLive is what the last collection actually marked as live.
func gcWatchMarkedLive() uint64 {
	metrics.Read(gcWatchLiveSample)
	if gcWatchLiveSample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return gcWatchLiveSample[0].Value.Uint64()
}

// gcWatchClasses reports where the memory the module holds has gone, in MB.
func gcWatchClasses() string {
	metrics.Read(gcWatchBreakdown)
	v := func(i int) uint64 {
		if gcWatchBreakdown[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return gcWatchBreakdown[i].Value.Uint64() >> 20
	}
	return fmt.Sprintf("total %dMB (free %dMB, unused %dMB, stacks %dMB, metadata %dMB)",
		v(0), v(1), v(2), v(3), v(4)+v(5)+v(6))
}

// gcWatchdog runs at the top of every body callback; bodySize is the size the host reports for it.
func gcWatchdog(bodySize int) {
	if !GCWatchdogEnabled {
		return
	}
	gcWatchCalls++
	if bodySize > 0 {
		gcWatchBytes += uint64(bodySize)
	}
	if gcWatchCalls%GCWatchdogEvery != 0 && (GCWatchdogEveryBytes == 0 || gcWatchBytes < GCWatchdogEveryBytes) {
		return
	}
	gcWatchBytes = 0
	gcWatchdogCheck()
}

func gcWatchdogCheck() {
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
	gcWatchLog("gc watchdog: heap %dMB exceeded goal %dMB without a GC cycle, forced GC, live now %dMB (marked %dMB); %s",
		heap>>20, goal>>20, live>>20, gcWatchMarkedLive()>>20, gcWatchClasses())
	gcWatchTopSites(live)
}
