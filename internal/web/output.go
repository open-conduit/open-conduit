package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// Output picks: the log, the transcript, or the output at an index.
const (
	pickLog        = -1
	pickTranscript = -2
)

// output is one of a run's outputs as shown: its live output while it runs,
// else its artifact.
type output struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Index int    `json:"index"`
	Count int    `json:"count"`
	Live  bool   `json:"live"`
	Err   string `json:"error,omitempty"`
	// Size is the whole output's size, of which Text may be the tail.
	Size int64 `json:"size"`
	// Artifact is the output's artifact, once the run has ended.
	Artifact string `json:"-"`
}

func pick(n, want, log, transcript int, hasTranscript bool) (int, error) {
	switch {
	case want == pickTranscript && !hasTranscript:
		return 0, fmt.Errorf("run has no transcript")
	case want == pickTranscript:
		return transcript, nil
	case want == pickLog:
		return log, nil
	}
	return want % n, nil
}

func parsePick(v string) int {
	switch v {
	case "", "log":
		return pickLog
	case "transcript":
		return pickTranscript
	}
	if i, err := strconv.Atoi(v); err == nil && i >= 0 {
		return i
	}
	return pickLog
}

// fetchOutput reads at most the last tail bytes of the output want of run
// id: the live output while it runs, else its artifact, settling on the
// artifacts once it ends.
func (s *Server) fetchOutput(ctx context.Context, id string, want int, tail int64) (output, error) {
	live, err := s.src.RunLive(ctx, id, tail)
	var e *client.Error
	switch {
	case errors.As(err, &e) && (e.Code == "not_live" || e.Code == "live_unsupported"):
	case err != nil:
		return output{}, err
	case len(live.Outputs) == 0:
		return output{Title: id + " (running)", Live: true}, nil
	default:
		log, _ := runview.LiveLog(live.Outputs)
		transcript, has := runview.LiveTranscript(live.Outputs)
		i, err := pick(len(live.Outputs), want, log, transcript, has)
		if err != nil {
			return output{}, err
		}
		o := live.Outputs[i]
		return output{Title: o.Name + " (running)", Text: o.Content, Index: i, Count: len(live.Outputs), Live: true, Size: max(o.Size, int64(len(o.Content)))}, nil
	}
	run, err := s.src.Run(ctx, id)
	if err != nil {
		return output{}, err
	}
	if len(run.Artifacts) == 0 {
		return output{}, fmt.Errorf("run has no artifacts")
	}
	logIdx, transcript, has := 0, 0, false
	for i, a := range run.Artifacts {
		if l, ok := runview.LogArtifact(run); ok && l.ID == a.ID {
			logIdx = i
		}
		if t, ok := runview.TranscriptArtifact(run); ok && t.ID == a.ID {
			transcript, has = i, true
		}
	}
	i, err := pick(len(run.Artifacts), want, logIdx, transcript, has)
	if err != nil {
		return output{}, err
	}
	a := run.Artifacts[i]
	if run.Retention.State == "expired" {
		return output{}, fmt.Errorf("artifact content expired under the retention policy")
	}
	out := output{Title: a.ID + " " + a.MediaType, Index: i, Count: len(run.Artifacts), Size: a.Size, Artifact: a.ID}
	if a.Size == 0 {
		return out, nil
	}
	var b strings.Builder
	byteRange := ""
	if a.Size > tail {
		byteRange = "bytes=-" + strconv.FormatInt(tail, 10)
	}
	if err := s.src.ArtifactContent(ctx, a.ID, byteRange, &b); err != nil {
		return output{}, err
	}
	out.Text = b.String()
	return out, nil
}

type outputData struct {
	RunID string
	Pick  string
	Out   output
	Next  int
}

func (s *Server) outputPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	id, want := r.URL.Query().Get("run"), r.URL.Query().Get("pick")
	out, err := s.fetchOutput(ctx, id, parsePick(want), LogTail)
	if err != nil {
		out.Err = errText(err)
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if want == "" {
		want = "log"
	}
	next := 0
	if out.Count > 0 {
		next = (out.Index + 1) % out.Count
	}
	s.render(w, r, "output", &page{Title: "Output " + id, Nav: "runs", Err: out.Err}, outputData{RunID: id, Pick: want, Out: out, Next: next})
}
