package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SegmentCommand is the subcommand the executor's own binary runs a
// segment under: `<binary> segment <run dir>`. A package's main hands it to
// RunSegment before anything else.
const SegmentCommand = "segment"

// segmentSpec is what the executor writes for the driver before launch.
type segmentSpec struct {
	// Binary is the Codex CLI and Args the options every turn passes.
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
	// Thread is the session to resume; empty starts a new one.
	Thread string `json:"thread,omitempty"`
	// Schema is the --output-schema file; empty for an unstructured turn.
	Schema string `json:"schema,omitempty"`
}

// Files in a segment's run directory.
const (
	specFile    = "segment.json"
	promptFile  = "prompt.md"
	eventsFile  = "events.jsonl"
	errFile     = "err.log"
	lastFile    = "last.json"
	maxFollowUp = 20
)

// RunSegment is the detached segment driver: it runs one `codex exec`
// turn on the segment's prompt, then a `codex exec resume` turn for
// whatever was injected meanwhile, until nothing is pending. It exits with
// the last turn's status. args are the run directory.
func RunSegment(args []string) int {
	if len(args) != 1 || !filepath.IsAbs(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: segment RUN_DIR")
		return 2
	}
	dir := args[0]
	var spec segmentSpec
	if err := readJSON(filepath.Join(dir, specFile), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "segment: unreadable spec")
		return 2
	}
	text, err := os.ReadFile(filepath.Join(dir, promptFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, "segment: unreadable prompt")
		return 2
	}
	thread := spec.Thread
	code := runTurn(dir, spec, thread, string(text))
	for followUps := 0; ; followUps++ {
		if thread == "" {
			thread = threadOf(filepath.Join(dir, eventsFile))
		}
		if code != 0 || thread == "" || followUps >= maxFollowUp {
			return code
		}
		inbox := filepath.Join(dir, inboxDir)
		pending, digests := pendingInbox(inbox)
		if len(pending) == 0 {
			return 0
		}
		code = runTurn(dir, spec, thread, interjection(pending))
		// The follow-up turn has had the events: acknowledge them, so the
		// result proves consumption, whatever that turn's outcome.
		for _, d := range digests {
			_ = writeFile(filepath.Join(inbox, "consumed", d), nil)
		}
	}
}

// runTurn runs one codex exec turn with message on stdin, appending its
// JSONL events and stderr to the segment's files.
func runTurn(dir string, spec segmentSpec, thread, message string) int {
	argv := []string{"exec"}
	if thread != "" {
		argv = append(argv, "resume")
	}
	argv = append(argv, spec.Args...)
	argv = append(argv, "--json", "--output-last-message", filepath.Join(dir, lastFile))
	if spec.Schema != "" {
		argv = append(argv, "--output-schema", spec.Schema)
	}
	if thread != "" {
		argv = append(argv, thread)
	}
	argv = append(argv, "-")
	events, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	defer events.Close()
	errs, err := os.OpenFile(filepath.Join(dir, errFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	defer errs.Close()
	_ = os.Remove(filepath.Join(dir, lastFile))
	cmd := exec.Command(spec.Binary, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(message), events, errs
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return exit.ExitCode()
		}
		fmt.Fprintf(errs, "segment: could not run codex: %v\n", err)
		return 127
	}
	return 0
}

// threadOf is the session `codex exec --json` started: its first
// thread.started event.
func threadOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "thread.started" && ev.ThreadID != "" {
			return ev.ThreadID
		}
		if err == io.EOF || err != nil {
			return ""
		}
	}
}
