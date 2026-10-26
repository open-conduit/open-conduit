package runview

import (
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// Running reports a run whose attempt has not ended, so its output is live
// rather than in artifacts.
func Running(r client.Run) bool { return r.State == "pending" || r.State == "running" }

// IsTranscript reports a transcript media type: JSON lines, the raw form an
// executor's rendered log is made from.
func IsTranscript(media string) bool {
	m, _, _ := strings.Cut(media, ";")
	m = strings.TrimSpace(m)
	return m == "application/x-ndjson" || m == "application/jsonl" || m == "application/x-jsonlines"
}

// TranscriptArtifact selects the run's transcript artifact.
func TranscriptArtifact(r client.Run) (client.Artifact, bool) {
	for _, a := range r.Artifacts {
		if IsTranscript(a.MediaType) {
			return a, true
		}
	}
	return client.Artifact{}, false
}

// LiveLog selects a running run's log the way LogArtifact selects a
// finished one's: the first text output, else the first.
func LiveLog(outs []client.LiveOutput) (int, bool) {
	for i, o := range outs {
		if strings.HasPrefix(o.MediaType, "text/") {
			return i, true
		}
	}
	return 0, len(outs) > 0
}

// LiveTranscript selects a running run's transcript output.
func LiveTranscript(outs []client.LiveOutput) (int, bool) {
	for i, o := range outs {
		if IsTranscript(o.MediaType) {
			return i, true
		}
	}
	return 0, false
}
