package devscope

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Process is a running process: its PID and command line, so a PID reused
// since it was seen is told apart.
type Process struct {
	PID     int
	Command string
}

// Descendants lists the running processes below root in the process tree.
// An agent's wrapper leads its own process group, so it outlives the
// daemon that started it; list a daemon's descendants before stopping it.
func Descendants(root int) []Process {
	parent, command := processTable()
	var found []Process
	for pid := range parent {
		for p, n := parent[pid], 0; p > 1 && n < 64; p, n = parent[p], n+1 {
			if p == root {
				found = append(found, Process{pid, command[pid]})
				break
			}
		}
	}
	return found
}

// Alive are those of ps still running: the same command at the same PID.
func Alive(ps []Process) []Process {
	_, running := processTable()
	var left []Process
	for _, p := range ps {
		if running[p.PID] == p.Command {
			left = append(left, p)
		}
	}
	return left
}

// Kill kills p and the process group it leads, if any.
func Kill(p Process) {
	_ = syscall.Kill(-p.PID, syscall.SIGKILL)
	_ = syscall.Kill(p.PID, syscall.SIGKILL)
}

// processTable is each running process's parent and command line.
func processTable() (parent map[int]int, command map[int]string) {
	parent, command = map[int]int{}, map[int]string{}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return parent, command
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parent[pid], command[pid] = ppid, strings.Join(f[2:], " ")
	}
	return parent, command
}
