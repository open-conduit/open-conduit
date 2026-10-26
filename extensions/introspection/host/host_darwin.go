package host

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

func loadAvg() ([3]float64, error) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return [3]float64{}, err
	}
	return parseLoadAvg(raw)
}

// parseLoadAvg reads struct loadavg: three fixed-point uint32s, then the scale as a long, aligned to 8.
func parseLoadAvg(raw []byte) ([3]float64, error) {
	var out [3]float64
	if len(raw) < 24 {
		return out, fmt.Errorf("vm.loadavg: %d bytes", len(raw))
	}
	scale := float64(binary.LittleEndian.Uint64(raw[16:24]))
	if scale == 0 {
		return out, fmt.Errorf("vm.loadavg: no scale")
	}
	for i := range out {
		out[i] = float64(binary.LittleEndian.Uint32(raw[i*4:])) / scale
	}
	return out, nil
}

// memory counts as available what Activity Monitor doesn't count as used: free pages, file cache and what can
// be purged or is held speculatively.
func memory() (total, avail uint64, err error) {
	total, err = sysctlUint("hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	page, err := sysctlUint("hw.pagesize")
	if err != nil {
		return 0, 0, err
	}
	var pages uint64
	for _, name := range []string{"vm.page_free_count", "vm.page_pageable_external_count", "vm.page_speculative_count", "vm.page_purgeable_count"} {
		n, err := sysctlUint(name)
		if err != nil {
			return 0, 0, err
		}
		pages += n
	}
	return total, min(pages*page, total), nil
}

// sysctlUint reads an unsigned sysctl of either width: some of these are 32 bits on one macOS and 64 on another.
func sysctlUint(name string) (uint64, error) {
	raw, err := unix.SysctlRaw(name)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return decodeUint(name, raw)
}

func decodeUint(name string, raw []byte) (uint64, error) {
	switch len(raw) {
	case 4:
		return uint64(binary.LittleEndian.Uint32(raw)), nil
	case 8:
		return binary.LittleEndian.Uint64(raw), nil
	}
	return 0, fmt.Errorf("%s: %d bytes", name, len(raw))
}
