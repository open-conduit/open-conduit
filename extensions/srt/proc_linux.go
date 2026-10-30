//go:build linux

package srt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type process struct {
	PID, PPID int
	Args      string
}

// processes reads /proc: every live process with its parent and argv.
func processes() ([]process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fields, err := stat(pid)
		if err != nil || len(fields) < 2 || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		out = append(out, process{PID: pid, PPID: ppid, Args: strings.TrimSpace(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))})
	}
	return out, nil
}

// stat is /proc/<pid>/stat after the command name: state, ppid, …
func stat(pid int) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, err
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return nil, errors.New("malformed process status")
	}
	return strings.Fields(string(raw[end+1:])), nil
}

// startTime is when pid started: its start in clock ticks since boot
// (field 22), from the boot time in /proc/stat. USER_HZ is 100 on every
// Linux this runs on.
func startTime(pid int) (time.Time, error) {
	fields, err := stat(pid)
	if err != nil || len(fields) < 20 {
		return time.Time{}, fmt.Errorf("process %d: no start time", pid)
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / 100), nil
		}
	}
	return time.Time{}, errors.New("no boot time")
}

// namespaced requires a descendant of root in mount, network, pid and user
// namespaces that all differ from the host's: the bubblewrap sandbox.
func namespaced(root int, procs []process) error {
	host := map[string]string{}
	for _, ns := range []string{"mnt", "net", "pid", "user"} {
		link, err := os.Readlink(filepath.Join("/proc/self/ns", ns))
		if err != nil {
			return err
		}
		host[ns] = link
	}
	children := map[int][]int{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	queue := append([]int(nil), children[root]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = append(queue[1:], children[pid]...)
		apart := true
		for ns, link := range host {
			l, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", ns))
			if err != nil || l == link {
				apart = false
				break
			}
		}
		if apart {
			return nil
		}
	}
	return fmt.Errorf("no process under the sandbox root %d is in namespaces of its own", root)
}
