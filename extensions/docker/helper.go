package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// HelperOptions are the helper's streams and the docker CLI it runs.
type HelperOptions struct {
	Docker                string
	Stdin                 io.Reader
	Stdout, Stderr        io.Writer
	SignalRetry, Interval time.Duration
}

// RunHelper is `open-conduit container run [--records <dir>] <ref>`: what
// an executor's wrapper runs to start a created container. It stays in the
// wrapper's process group, so an Interrupt reaches it, and runs `docker
// start -a -i` in a group of its own, so a signal to the group never reaches
// the attached CLI. It turns TERM, INT and HUP into `docker kill -s`, each
// recorded first as one the host sent (retrying until the container runs),
// and exits with the container's code from `docker wait`; 125 when the
// container never ran.
func RunHelper(args []string, o HelperOptions) int {
	if o.Docker == "" {
		o.Docker = "docker"
	}
	if o.SignalRetry == 0 {
		o.SignalRetry = 30 * time.Second
	}
	if o.Interval == 0 {
		o.Interval = 100 * time.Millisecond
	}
	var records string
	if len(args) >= 2 && args[0] == "--records" {
		records, args = args[1], args[2:]
	}
	if len(args) != 1 {
		fmt.Fprintln(o.Stderr, "usage: open-conduit container run [--records <dir>] <context>/<container>")
		return 125
	}
	ref := args[0]
	dctx, id, err := splitRef(ref)
	if err != nil {
		fmt.Fprintln(o.Stderr, "open-conduit container run:", err)
		return 125
	}
	docker := func(args ...string) *exec.Cmd {
		return exec.Command(o.Docker, append([]string{"--context", dctx}, args...)...)
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	cli := docker("start", "--attach", "--interactive", id)
	cli.Stdin, cli.Stdout, cli.Stderr = o.Stdin, o.Stdout, o.Stderr
	cli.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cli.Start(); err != nil {
		fmt.Fprintln(o.Stderr, "open-conduit container run:", err)
		return 125
	}
	done := make(chan struct{})
	go func() { _ = cli.Wait(); close(done) }()
	names := map[os.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGINT: "INT", syscall.SIGHUP: "HUP"}
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		for {
			select {
			case s := <-sigs:
				name := names[s]
				_ = extension.RecordContainerSignal(records, ref, strings.ToLower(name))
				relay(docker, id, name, o.SignalRetry, o.Interval)
			case <-done:
				return
			}
		}
	}()
	<-done
	<-relayed
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	wait := exec.CommandContext(ctx, o.Docker, "--context", dctx, "wait", id)
	if err := wait.Run(); err != nil {
		fmt.Fprintln(o.Stderr, "open-conduit container run: docker wait:", err)
		return 125
	}
	out, err := exec.CommandContext(ctx, o.Docker, "--context", dctx, "inspect", "--type", "container", "--format", "{{json .State}}", id).Output()
	var st struct {
		ExitCode  int
		StartedAt time.Time
		Error     string
	}
	if err != nil || json.Unmarshal(out, &st) != nil {
		fmt.Fprintln(o.Stderr, "open-conduit container run: could not read the container's exit")
		return 125
	}
	if st.StartedAt.Year() <= 1 || st.Error != "" {
		fmt.Fprintln(o.Stderr, "open-conduit container run: the container did not start:", st.Error)
		return 125
	}
	return st.ExitCode
}

// relay sends a signal to the container once it runs; one created and not
// yet started is retried, one that has stopped needs none.
func relay(docker func(...string) *exec.Cmd, id, name string, retry, interval time.Duration) {
	deadline := time.Now().Add(retry)
	for time.Now().Before(deadline) {
		out, err := docker("inspect", "--type", "container", "--format", "{{.State.Status}}", id).Output()
		switch strings.TrimSpace(string(out)) {
		case "running":
			if docker("kill", "--signal", name, id).Run() == nil {
				return
			}
		case "created", "restarting":
		default:
			if err == nil {
				return
			}
		}
		time.Sleep(interval)
	}
}
