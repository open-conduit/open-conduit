package host

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func loadAvg() ([3]float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{}, err
	}
	return parseLoadAvg(string(data))
}

func parseLoadAvg(s string) ([3]float64, error) {
	var out [3]float64
	f := strings.Fields(s)
	if len(f) < 3 {
		return out, fmt.Errorf("/proc/loadavg: %q", s)
	}
	for i := range out {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return out, fmt.Errorf("/proc/loadavg: %w", err)
		}
		out[i] = v
	}
	return out, nil
}

func memory() (total, avail uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseMeminfo(bufio.NewScanner(f))
}

// parseMeminfo reads MemTotal and MemAvailable (kB); a kernel too old for MemAvailable gets free plus cache.
func parseMeminfo(sc *bufio.Scanner) (total, avail uint64, err error) {
	kb := map[string]uint64{}
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			kb[name] = v
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if kb["MemTotal"] == 0 {
		return 0, 0, fmt.Errorf("/proc/meminfo: no MemTotal")
	}
	a, ok := kb["MemAvailable"]
	if !ok {
		a = kb["MemFree"] + kb["Buffers"] + kb["Cached"]
	}
	return kb["MemTotal"] * 1024, a * 1024, nil
}
