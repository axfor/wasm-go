//go:build gcprofile

package wrapper

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// Only with the gcprofile tag does the program reference runtime.MemProfile, and so keep the runtime's memory profiler
// on (see gcWatchTopSites).
func init() { gcWatchTopSites = gcWatchTopSitesFromProfile }

// gcWatchTopSitesFromProfile logs the allocation sites holding the most memory. Only what the profiler sampled is
// visible (one object in every MemProfileRate bytes), which is enough to tell which of a handful of places is holding on.
func gcWatchTopSitesFromProfile(live uint64) {
	if !GCWatchdogProfile || live < GCWatchdogProfileFloor {
		return
	}
	var recs []runtime.MemProfileRecord
	n, ok := runtime.MemProfile(nil, false)
	for !ok {
		recs = make([]runtime.MemProfileRecord, n+64)
		n, ok = runtime.MemProfile(recs, false)
	}
	recs = recs[:n]
	sort.Slice(recs, func(i, j int) bool { return recs[i].InUseBytes() > recs[j].InUseBytes() })
	for i, r := range recs {
		if i == 5 || r.InUseBytes() == 0 {
			break
		}
		frames := runtime.CallersFrames(r.Stack())
		var where []string
		for len(where) < 4 {
			f, more := frames.Next()
			if f.Function == "" {
				break
			}
			where = append(where, fmt.Sprintf("%s:%d", f.Function, f.Line))
			if !more {
				break
			}
		}
		gcWatchLog("gc watchdog: holding %dMB in %d objects at %s", r.InUseBytes()>>20, r.InUseObjects(), strings.Join(where, " <- "))
	}
}
