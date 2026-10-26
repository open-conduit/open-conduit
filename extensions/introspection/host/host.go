// Package host reads how loaded the machine Open Conduit runs on is: its CPU load, memory and disk space.
// Agents, their dev servers and their worktrees all share the one machine, so a host that runs out of any of
// them stalls every project at once. It reads what the OS already keeps (/proc on Linux, sysctl on macOS), so
// nothing runs to take a sample. Ported from Open Supervisor v1 internal/host for the introspection host watch (05-03).
package host

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// ErrUnsupported is a reading this OS doesn't give.
var ErrUnsupported = errors.New("not supported on " + runtime.GOOS)

// Sample is one reading of the host. What the OS doesn't give is left out: HasLoad false, MemTotal 0, no disk.
type Sample struct {
	CPUs int
	// Load is the 1, 5 and 15-minute load averages.
	Load    [3]float64
	HasLoad bool
	// MemTotal and MemAvailable are bytes; available counts the cache the OS can give back.
	MemTotal, MemAvailable uint64
	Disks                  []Disk
}

// Disk is the filesystem a path is on.
type Disk struct {
	Path        string
	Total, Free uint64
}

// LoadPerCPU is the 1-minute load over the CPUs: about 1 when every core is busy.
func (s Sample) LoadPerCPU() float64 {
	if !s.HasLoad || s.CPUs == 0 {
		return 0
	}
	return s.Load[0] / float64(s.CPUs)
}

// MemoryUsedPct is the share of memory in use, 0–100.
func (s Sample) MemoryUsedPct() float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return 100 * float64(s.MemTotal-min(s.MemAvailable, s.MemTotal)) / float64(s.MemTotal)
}

// UsedPct is the share of the disk in use, 0–100.
func (d Disk) UsedPct() float64 {
	if d.Total == 0 {
		return 0
	}
	return 100 * float64(d.Total-min(d.Free, d.Total)) / float64(d.Total)
}

// Read takes a sample, with the disks the paths are on, once each: a path on a filesystem an earlier one is on
// is left out. A path that doesn't exist yet is measured at its nearest parent that does. It fails only when
// the OS gives none of it.
func Read(paths []string) (Sample, error) {
	s := Sample{CPUs: runtime.NumCPU()}
	var errs []error
	if l, err := loadAvg(); err == nil {
		s.Load, s.HasLoad = l, true
	} else {
		errs = append(errs, err)
	}
	if total, avail, err := memory(); err == nil {
		s.MemTotal, s.MemAvailable = total, avail
	} else {
		errs = append(errs, err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		total, free, id, err := diskSpace(existing(p))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		s.Disks = append(s.Disks, Disk{Path: p, Total: total, Free: free})
	}
	if !s.HasLoad && s.MemTotal == 0 && len(s.Disks) == 0 {
		return s, errors.Join(errs...)
	}
	return s, nil
}

// existing is path, or its nearest parent that exists.
func existing(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
