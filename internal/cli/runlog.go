package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// FollowInterval is how often `runs log --follow` asks for new output.
var FollowInterval = 2 * time.Second

// liveTail bounds each live output `runs log` fetches.
const liveTail = 1 << 20

// runLog prints a run's log: its log artifact once it has ended, or what it
// has written so far while it runs. --follow keeps printing a running run's
// new output until it ends; --transcript picks the raw transcript instead.
func (o *operator) runLog(args []string) int {
	f := newFlags("log", "range")
	follow := f.fs.Bool("follow", false, "")
	f.fs.BoolVar(follow, "f", false, "")
	transcript := f.fs.Bool("transcript", false, "")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected runs log ID [--follow] [--transcript] [--range bytes=A-B]")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	id := pos[0]
	cx, cancel := ctx()
	run, err := c.Run(cx, id)
	cancel()
	if err != nil {
		return o.emit(nil, err)
	}
	printed := int64(-1)
	if runview.Running(run) {
		if f.get("range") != "" {
			return usageError(o.errOut, "--range applies to an ended run's artifact")
		}
		if printed, err = o.printLive(c, id, *follow, *transcript); err != nil {
			return o.emit(nil, err)
		}
		if printed >= 0 && !*follow {
			return 0
		}
		// The run ended: what is left is in its artifacts.
		cx, cancel := ctx()
		run, err = c.Run(cx, id)
		cancel()
		if err != nil {
			return o.emit(nil, err)
		}
	}
	pick := runview.LogArtifact
	if *transcript {
		pick = runview.TranscriptArtifact
	}
	a, ok := pick(run)
	if !ok {
		if printed >= 0 {
			return 0
		}
		if *transcript {
			fmt.Fprintln(o.errOut, "open-conduit: run has no transcript")
		} else {
			fmt.Fprintln(o.errOut, "open-conduit: run has no artifacts")
		}
		return 1
	}
	byteRange := f.get("range")
	if printed >= 0 {
		if a.Size <= printed {
			return 0
		}
		byteRange = "bytes=" + strconv.FormatInt(printed, 10) + "-"
	}
	cx, cancel = ctx()
	defer cancel()
	if err = c.ArtifactContent(cx, a.ID, byteRange, o.out); err != nil {
		return o.emit(nil, err)
	}
	return 0
}

// printLive prints a running run's log (or transcript) so far and, with
// follow, its new output until the run ends. It returns how much of the
// output it printed, -1 when the run had already ended.
func (o *operator) printLive(c *client.Client, id string, follow, transcript bool) (int64, error) {
	printed := int64(-1)
	for {
		cx, cancel := ctx()
		live, err := c.RunLive(cx, id, liveTail)
		cancel()
		if client.IsNotLive(err) {
			return printed, nil
		}
		if err != nil {
			return printed, err
		}
		pick := runview.LiveLog
		if transcript {
			pick = runview.LiveTranscript
		}
		if i, ok := pick(live.Outputs); ok {
			out := live.Outputs[i]
			start := out.Size - int64(len(out.Content))
			from := max(printed, start) - start
			if from < int64(len(out.Content)) {
				fmt.Fprint(o.out, out.Content[from:])
			}
			printed = max(printed, out.Size)
		} else if transcript {
			return printed, fmt.Errorf("run has no transcript")
		}
		printed = max(printed, 0)
		if !follow {
			return printed, nil
		}
		time.Sleep(FollowInterval)
	}
}
