//go:build !linux

package srt

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type process struct {
	PID, PPID int
	Args      string
}

func ps(args ...string) ([]byte, error) {
	cmd := exec.Command("ps", args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	return cmd.Output()
}

// processes lists every process with its parent and argv.
func processes() ([]process, error) {
	out, err := ps("-A", "-o", "pid=", "-o", "ppid=", "-o", "command=")
	if err != nil {
		return nil, err
	}
	var list []process
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		list = append(list, process{PID: pid, PPID: ppid, Args: strings.Join(f[2:], " ")})
	}
	return list, nil
}

// startTime is when pid started, to the second.
func startTime(pid int) (time.Time, error) {
	out, err := ps("-p", strconv.Itoa(pid), "-o", "lstart=")
	if err != nil {
		return time.Time{}, fmt.Errorf("process %d: no start time", pid)
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return time.Time{}, errors.New("unreadable process start time")
	}
	return t, nil
}

// namespaced is Linux's; on macOS the boundary is Seatbelt's, inherited by
// every process the root starts.
func namespaced(int, []process) error { return nil }
