//go:build unix && !linux

package codex

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// psList runs ps with a fixed locale and parses pid, pgid, state and the full
// argument vector (unlimited width when not writing to a terminal).
func psList(args ...string) ([]procInfo, error) {
	cmd := exec.Command("ps", append(args, "-o", "pid=", "-o", "pgid=", "-o", "stat=", "-o", "command=")...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(out) == 0 {
		return nil, nil // no matching process
	} else if err != nil {
		return nil, err
	}
	var list []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		var fields [3]string
		rest := line
		ok := true
		for i := range fields {
			rest = strings.TrimLeft(rest, " \t")
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				ok = false
				break
			}
			fields[i], rest = rest[:end], rest[end:]
		}
		if !ok {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pgid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || strings.Contains(fields[2], "Z") {
			continue
		}
		list = append(list, procInfo{PID: pid, PGID: pgid, Args: strings.TrimSpace(rest)})
	}
	return list, nil
}

func inspectProcess(pid int) (procInfo, bool, error) {
	if pid <= 1 {
		return procInfo{}, false, nil
	}
	list, err := psList("-p", strconv.Itoa(pid))
	if err != nil {
		return procInfo{}, false, err
	}
	for _, p := range list {
		if p.PID == pid {
			return p, true, nil
		}
	}
	return procInfo{}, false, nil
}

func scanProcesses() ([]procInfo, error) { return psList("-A") }
