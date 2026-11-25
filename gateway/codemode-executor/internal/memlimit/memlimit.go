// Package memlimit fits the executor to its container's memory limit. Each running script can
// hold up to sandbox.max_memory_bytes of WebAssembly memory, and growing that memory briefly
// needs more, so the executor sets the Go memory limit below the container's and refuses to
// start when its configured concurrency cannot fit: an executor killed for memory fails every
// caller's run, which would let one caller's scripts take it down for everyone.
package memlimit

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Base is the memory the executor needs before any script runs.
const Base = 128 << 20

// GoLimitPercent is the share of the container limit given to the Go runtime as GOMEMLIMIT.
const GoLimitPercent = 85

// cgroup files holding the container's memory limit: v2, then v1.
var files = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}

// Container returns the container's memory limit, or false when there is none or it cannot
// be read.
func Container() (int64, bool) {
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			return parse(string(b))
		}
	}
	return 0, false
}

func parse(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	// cgroup v1 reports "no limit" as a huge page-rounded number.
	if err != nil || n <= 0 || n >= 1<<60 {
		return 0, false
	}
	return n, true
}

// Check reports whether runs scripts of up to perRun bytes each fit in limit.
func Check(limit int64, runs int, perRun int64) error {
	need := Base + int64(runs)*perRun
	if need > limit {
		return fmt.Errorf("server.max_concurrent_runs (%d) × sandbox.max_memory_bytes (%d MiB) + %d MiB needs %d MiB, above the container's %d MiB memory limit; lower one of them or raise the limit",
			runs, perRun>>20, Base>>20, need>>20, limit>>20)
	}
	return nil
}

// GoLimit is the GOMEMLIMIT to use under a container limit.
func GoLimit(limit int64) int64 { return limit / 100 * GoLimitPercent }
