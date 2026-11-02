//go:build linux

package codex

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// inspectProcess reads /proc. A zombie or vanished process is reported absent.
func inspectProcess(pid int) (procInfo, bool, error) {
	if pid <= 1 {
		return procInfo{}, false, nil
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if procGone(err) {
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
	if procGone(err) {
		return procInfo{}, false, nil
	} else if err != nil {
		return procInfo{}, false, err
	}
	return procInfo{PID: pid, PGID: pgid, Args: strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))}, true, nil
}

// procGone reports whether a /proc read failed because the process is gone:
// ENOENT when it was reaped before the open, ESRCH when it was reaped between
// the open and the read (an adopted segment exiting while it is observed).
func procGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
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
