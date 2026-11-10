// Package cpuload reads how busy the CPUs of the machine running the harness
// were during a phase, from /proc/stat. Inside a Docker Desktop container that
// is the whole Docker VM (client and database servers together), which shows
// whether a phase was CPU-bound inside the VM. It cannot see load from Windows
// processes outside the VM (WSL2 reports no steal time), and it is unavailable
// on Windows itself.
package cpuload

import (
	"os"
	"strconv"
	"strings"
)

// Sample is one reading of the aggregate CPU counters.
type Sample struct {
	busy, total uint64
	ok          bool
}

// Read takes a sample; ok is false where /proc/stat does not exist.
func Read() Sample {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return Sample{}
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return Sample{}
	}
	var s Sample
	for i, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return Sample{}
		}
		// guest and guest_nice (fields 9 and 10) are already counted in user and nice.
		if i >= 8 {
			continue
		}
		s.total += v
		if i != 3 && i != 4 { // idle, iowait
			s.busy += v
		}
	}
	s.ok = true
	return s
}

// BusyPercent is the share of CPU time spent busy between two samples, or nil
// when it cannot be computed.
func BusyPercent(start, end Sample) *float64 {
	if !start.ok || !end.ok || end.total <= start.total {
		return nil
	}
	pct := 100 * float64(end.busy-start.busy) / float64(end.total-start.total)
	return &pct
}
