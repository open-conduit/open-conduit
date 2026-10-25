//go:build linux

package shell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// inspectProcess reads /proc. A zombie or vanished process is reported absent.
func inspectProcess(pid int) (procInfo, bool, error) {
	if pid <= 1 {
		return procInfo{}, false, nil
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return procInfo{}, false, nil
	} else if err != nil {
		return procInfo{}, false, err
	}
	// pid (comm) state ppid pgrp ...; comm may contain spaces or parentheses.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return procInfo{}, false, errors.New("malformed process status")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 3 {
		return procInfo{}, false, errors.New("malformed process status")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return procInfo{}, false, nil
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return procInfo{}, false, err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if errors.Is(err, os.ErrNotExist) {
		return procInfo{}, false, nil
	} else if err != nil {
		return procInfo{}, false, err
	}
	return procInfo{PID: pid, PGID: pgid, Args: strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))}, true, nil
}

func scanProcesses() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ok, err := inspectProcess(pid); err == nil && ok {
			out = append(out, p)
		}
	}
	return out, nil
}
