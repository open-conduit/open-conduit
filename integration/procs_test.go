package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// proc is a host process: its ID, its group and its argument vector.
type proc struct {
	pid, pgid int
	args      string
}

// procsUnder lists the processes whose arguments name a path under root.
// Every shell or claude-code run wrapper is given its run directory, so a
// drill's runs all name its root.
func procsUnder(root string) ([]proc, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=", "-o", "pgid=", "-o", "stat=", "-o", "args=")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var list []proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || strings.Contains(f[2], "Z") {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		args := strings.Join(f[3:], " ")
		if err1 != nil || err2 != nil || pid == os.Getpid() || !strings.Contains(args, root+"/") {
			continue
		}
		list = append(list, proc{pid: pid, pgid: pgid, args: args})
	}
	return list, nil
}

// reapUnder SIGKILLs every process under root, and the whole group of each
// that leads one (a detached run wrapper and whatever it started), until
// none is left. A run's lifetime is bounded by a daemon's sweep, and a test's
// daemon never comes back for it, so the test must (#654).
func reapUnder(root string) error {
	var left []proc
	var err error
	if eventually(10*time.Second, func() bool {
		if left, err = procsUnder(root); err != nil || len(left) == 0 {
			return true
		}
		for _, p := range left {
			if p.pgid == p.pid && p.pgid != syscall.Getpgrp() {
				_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
			}
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
		return false
	}) {
		return err
	}
	return fmt.Errorf("processes survived SIGKILL under %s: %v", root, left)
}
