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
	live := F * 3 / 4 // 回收后存活 = 0.75 × 下限
	gcWatchForce = func() { forced++; heap = live }
	gcWatchLog = func(string, ...interface{}) {} // 测试里没有宿主
	gcWatchLastLive, gcWatchCalls = 0, 0
	tick := func(n int) {
		for i := 0; i < n; i++ {
			gcWatchdog()
		}
	}
	// 堆在下限之下：不动作
	heap = F - 1<<20
	tick(int(GCWatchdogEvery) * 3)
	if forced != 0 {
		t.Fatalf("低于下限不该主动 GC，forced=%d", forced)
	}
	// 超过下限：动作一次，之后阈值 = 存活 × 2 = 1.5 × 下限
	heap = F + 1<<20
	tick(int(GCWatchdogEvery))
	if forced != 1 || gcWatchLastLive != live {
		t.Fatalf("应主动 GC 一次并记录存活量，forced=%d live=%d", forced, gcWatchLastLive)
	}
	heap = live*2 - 1<<20
	tick(int(GCWatchdogEvery) * 2)
	if forced != 1 {
		t.Fatalf("低于 2×存活 不该动作，forced=%d", forced)
	}
	heap = live*2 + 1<<20
	tick(int(GCWatchdogEvery))
	if forced != 2 {
		t.Fatalf("超过 2×存活 应动作，forced=%d", forced)
	}
	// 关掉
	GCWatchdogEnabled = false
	heap = F * 10
	tick(int(GCWatchdogEvery) * 2)
	GCWatchdogEnabled = true
	if forced != 2 {
		t.Fatalf("关闭后不该动作，forced=%d", forced)
	}
}
