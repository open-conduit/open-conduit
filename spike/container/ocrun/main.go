// Command ocrun stands in for a host helper the broker's Launch argv would
// run instead of a bare `docker start -a`: it keeps the attached CLI out of
// the wrapper's process group, turns TERM/INT/HUP into `docker kill -s` on
// the container (retrying until the container has started), and exits with
// the container's own exit code once it has exited.
package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	id := os.Args[1]
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	cli := exec.Command("docker", "start", "-a", "-i", id)
	cli.Stdin, cli.Stdout, cli.Stderr = os.Stdin, os.Stdout, os.Stderr
	cli.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cli.Start(); err != nil {
		os.Exit(125)
	}
	done := make(chan struct{})
	go func() { cli.Wait(); close(done) }()
	go func() {
		for s := range sigs {
			name := map[os.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGINT: "INT", syscall.SIGHUP: "HUP"}[s]
			for i := 0; i < 100; i++ {
				out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", id).Output()
				st := strings.TrimSpace(string(out))
				if st == "running" {
					exec.Command("docker", "kill", "-s", name, id).Run()
					break
				}
				if st != "created" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	<-done
	out, err := exec.Command("docker", "wait", id).Output()
	if err != nil {
		os.Exit(125)
	}
	c, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	os.Exit(c)
}
