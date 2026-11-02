package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// HelperOptions are the helper's streams and timings.
type HelperOptions struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// StopGrace is how long a run left behind by a lost connection gets
	// after TERM before its group is killed (10s when zero).
	StopGrace time.Duration
	// SignalRetry is how long a signal waits for the run to start (30s
	// when zero), checked every Interval (100ms when zero).
	SignalRetry, Interval time.Duration
	Now                   func() time.Time
}

// RunHelper is `open-conduit remote run --records <dir> <ref>`: what an
// executor's wrapper runs to start a created run. It stays in the
// wrapper's process group, so an Interrupt reaches it, and runs ssh in a
// group of its own, with the execution's toolbelt socket forwarded to the
// same path on the remote. It turns TERM, INT and HUP into the same
// signal to the run's stub, each recorded first as one the host sent.
// Once ssh ends it reads the run's status from the remote (a run a lost
// connection left running is stopped), copies back what the run changed
// in its write grants and exits with the run's status; 125 when the run
// never started, its status can't be read or its changes can't be copied
// back.
func RunHelper(args []string, o HelperOptions) int {
	if o.StopGrace == 0 {
		o.StopGrace = 10 * time.Second
	}
	if o.SignalRetry == 0 {
		o.SignalRetry = 30 * time.Second
	}
	if o.Interval == 0 {
		o.Interval = 100 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(o.Stderr, "open-conduit remote run: "+format+"\n", a...)
		return 125
	}
	if len(args) != 3 || args[0] != "--records" {
		return fail("usage: open-conduit remote run --records <dir> <remote>/<run>")
	}
	records, ref := args[1], args[2]
	i := strings.LastIndex(ref, "/")
	if i < 0 || !idPattern.MatchString(ref[i+1:]) {
		return fail("%q is not an ssh run reference", ref)
	}
	id := ref[i+1:]
	var run Run
	raw, err := os.ReadFile(RunFile(records, id))
	if err == nil {
		err = json.Unmarshal(raw, &run)
	}
	if err != nil || run.Ref != ref || run.ID != id || len(run.SSH) == 0 || run.Host == "" {
		return fail("no record of run %s", ref)
	}
	h := &helper{run: run, records: records, o: o}

	start := append([]string{}, run.SSH...)
	if run.Toolbelt != "" {
		start = append(start, "-R", run.Root+run.Toolbelt+":"+run.Toolbelt, "-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes")
	}
	tty := "-T"
	if run.TTY {
		tty = "-t"
	}
	start = append(start, tty, run.Host, command(run.Root, scriptRun, id, strconv.FormatInt(o.Now().UnixNano(), 10)))
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	cli := exec.Command(start[0], start[1:]...)
	cli.Stdin, cli.Stdout, cli.Stderr = o.Stdin, o.Stdout, o.Stderr
	// An operator's attach runs ssh on their terminal, which carries their
	// keys and signals itself; a segment's runs off the group.
	if !run.TTY {
		cli.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cli.Start(); err != nil {
		return fail("%v", err)
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
				if run.TTY {
					continue
				}
				_ = extension.RecordContainerSignal(records, ref, strings.ToLower(names[s]))
				h.relay(names[s])
			case <-done:
				return
			}
		}
	}()
	<-done
	<-relayed

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	st, err := h.state(ctx)
	if err != nil {
		return fail("could not read the run's status: %v", err)
	}
	code := st.ExitCode
	lost := false
	if st.Running {
		// The connection went with the run still running: it is stopped,
		// as a lost container's would be, rather than left unwatched.
		lost = true
		_ = extension.RecordContainerSignal(records, ref, "stop")
		if _, err := h.remote(ctx, nil, scriptStop, id, tenths(o.StopGrace)); err != nil {
			return fail("the connection was lost and the run could not be stopped: %v", err)
		}
	}
	if st.StartedAt.IsZero() {
		return fail("the run did not start")
	}
	if err := h.copyBack(ctx); err != nil {
		return fail("could not copy the run's changes back: %v", err)
	}
	if lost {
		return fail("the connection to %s was lost while the run ran", run.Host)
	}
	return code
}

type helper struct {
	run     Run
	records string
	o       HelperOptions
}

// remote runs a script on the run's remote.
func (h *helper) remote(ctx context.Context, stdin io.Reader, script string, args ...string) ([]byte, error) {
	argv := append(append([]string{}, h.run.SSH...), "-T", h.run.Host, command(h.run.Root, script, args...))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &sshError{endpoint: h.run.Host, stderr: stderr.String(), err: err}
	}
	return out, nil
}

// state is the run's state on the remote.
func (h *helper) state(ctx context.Context) (extension.ContainerState, error) {
	out, err := h.remote(ctx, nil, scriptInspect, h.run.ID)
	if err != nil {
		return extension.ContainerState{}, err
	}
	states, err := parseStates(Settings{}, out)
	if err != nil {
		return extension.ContainerState{}, err
	}
	st, ok := states[h.run.ID]
	if !ok || st.Gone {
		return extension.ContainerState{}, errors.New("the run's records are gone")
	}
	return st, nil
}

// relay sends a signal to the run once it runs; one not yet started is
// retried, one that has ended (or is gone) needs none.
func (h *helper) relay(name string) {
	id := []byte(" " + h.run.ID)
	deadline := time.Now().Add(h.o.SignalRetry)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := h.remote(ctx, nil, scriptInspect, h.run.ID)
		if err == nil {
			has := func(fact string) bool { return bytes.Contains(out, append([]byte(fact), id...)) }
			switch {
			case has("running"):
				if _, err := h.remote(ctx, nil, scriptSignal, h.run.ID, name); err == nil {
					cancel()
					return
				}
			case has("gone"), has("exit"), has("pid"):
				cancel()
				return
			}
		}
		cancel()
		time.Sleep(h.o.Interval)
	}
}

// copyBack brings back what the run changed in its write grants.
func (h *helper) copyBack(ctx context.Context) error {
	listing, err := h.remote(ctx, nil, scriptList0, h.run.Write...)
	if err != nil {
		return err
	}
	argv := append(append([]string{}, h.run.SSH...), "-T", h.run.Host, command(h.run.Root, scriptDownload, append([]string{h.run.ID}, h.run.Write...)...))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// What the remote has and the host didn't copy comes back whatever its
	// times: a renamed file keeps its old mtime.
	listed := ParseList(listing)
	var fresh []string
	for path := range listed {
		if _, ok := h.run.Manifest[path]; !ok {
			fresh = append(fresh, path)
		}
	}
	sort.Strings(fresh)
	var extra bytes.Buffer
	for _, path := range fresh {
		extra.WriteString("." + path + "\x00")
	}
	cmd.Stdin = &extra
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	applyErr := Apply(out, listed, h.run.Manifest, h.run.Write, h.run.Keep)
	if applyErr != nil {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, out)
	waitErr := cmd.Wait()
	if applyErr != nil {
		return applyErr
	}
	if waitErr != nil {
		return &sshError{endpoint: h.run.Host, stderr: stderr.String(), err: waitErr}
	}
	return nil
}
