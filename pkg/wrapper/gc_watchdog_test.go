package wrapper

import "testing"

func TestGCWatchdog(t *testing.T) {
	origHeap, origForce, origLog := gcWatchHeapBytes, gcWatchForce, gcWatchLog
	origLive, origCalls := gcWatchLastLive, gcWatchCalls
	defer func() {
		gcWatchHeapBytes, gcWatchForce, gcWatchLog = origHeap, origForce, origLog
		gcWatchLastLive, gcWatchCalls = origLive, origCalls
	}()
	var heap uint64
	forced := 0
	gcWatchHeapBytes = func() uint64 { return heap }
	F := GCWatchdogFloor
	live := F * 3 / 4 // live after collection = 0.75 × floor
	gcWatchForce = func() { forced++; heap = live }
	gcWatchLog = func(string, ...interface{}) {} // no host in tests
	gcWatchLastLive, gcWatchCalls = 0, 0
	tick := func(n int) {
		for i := 0; i < n; i++ {
			gcWatchdog(0)
		}
	}
	// heap below the floor: no action
	heap = F - 1<<20
	tick(int(GCWatchdogEvery) * 3)
	if forced != 0 {
		t.Fatalf("no forced GC expected below the floor, forced=%d", forced)
	}
	// above the floor: act once, then the threshold is live × 2 = 1.5 × floor
	heap = F + 1<<20
	tick(int(GCWatchdogEvery))
	if forced != 1 || gcWatchLastLive != live {
		t.Fatalf("expected one forced GC and the live size recorded, forced=%d live=%d", forced, gcWatchLastLive)
	}
	heap = live*2 - 1<<20
	tick(int(GCWatchdogEvery) * 2)
	if forced != 1 {
		t.Fatalf("no action expected below 2×live, forced=%d", forced)
	}
	heap = live*2 + 1<<20
	tick(int(GCWatchdogEvery))
	if forced != 2 {
		t.Fatalf("action expected above 2×live, forced=%d", forced)
	}
	// switched off
	GCWatchdogEnabled = false
	heap = F * 10
	tick(int(GCWatchdogEvery) * 2)
	GCWatchdogEnabled = true
	if forced != 2 {
		t.Fatalf("no action expected when disabled, forced=%d", forced)
	}
}

// A few large chunks trigger a check before the callback count does.
func TestGCWatchdogChecksByBytes(t *testing.T) {
	origHeap, origForce, origLog := gcWatchHeapBytes, gcWatchForce, gcWatchLog
	origLive, origCalls, origBytes := gcWatchLastLive, gcWatchCalls, gcWatchBytes
	origEvery, origEveryBytes, origFloor := GCWatchdogEvery, GCWatchdogEveryBytes, GCWatchdogFloor
	t.Cleanup(func() {
		gcWatchHeapBytes, gcWatchForce, gcWatchLog = origHeap, origForce, origLog
		gcWatchLastLive, gcWatchCalls, gcWatchBytes = origLive, origCalls, origBytes
		GCWatchdogEvery, GCWatchdogEveryBytes, GCWatchdogFloor = origEvery, origEveryBytes, origFloor
	})
	heap, forced := uint64(8<<20), 0
	gcWatchHeapBytes = func() uint64 { return heap }
	gcWatchForce = func() { forced++; heap = 1 << 20 }
	gcWatchLog = func(string, ...interface{}) {}
	gcWatchLastLive, gcWatchCalls, gcWatchBytes = 0, 0, 0
	GCWatchdogEvery, GCWatchdogEveryBytes, GCWatchdogFloor = 1000, 1<<20, 4<<20

	gcWatchdog(600 << 10)
	if forced != 0 {
		t.Fatalf("checked after 600KB of a 1MB byte budget")
	}
	gcWatchdog(600 << 10)
	if forced != 1 {
		t.Fatalf("expected a forced GC once 1.2MB came in, forced=%d", forced)
	}
	if gcWatchBytes != 0 {
		t.Fatalf("byte count not reset after the check: %d", gcWatchBytes)
	}
}
