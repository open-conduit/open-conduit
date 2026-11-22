package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
)

// OpChecksLog reads a failed GitHub Actions job's log on a PR's head.
const OpChecksLog = "github.checks.log"

const (
	// maxLogExcerpt bounds the log text a call returns inline.
	maxLogExcerpt = 64 << 10
	// logLinesBefore and logLinesAfter are the lines kept around each line
	// the runner marked as an error: the marker usually only says the step
	// exited, and what failed is above it.
	logLinesBefore = 100
	logLinesAfter  = 5
	// logTailLines is what an excerpt keeps of a log with no error marker.
	logTailLines = 200
)

type checksLogInput struct {
	Check  string `json:"check,omitempty" doc:"the failed check's name as github.checks shows it; may be left out when exactly one GitHub Actions job failed"`
	Number int    `json:"number,omitempty" doc:"the PR whose check to read; defaults to the open PR of this Work's issue, and is required for any other"`
	Repo   string `json:"repo,omitempty" doc:"the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one"`
	SaveTo string `json:"save_to,omitempty" doc:"a path the os client saves the whole log to (its last 50 MB), instead of returning the failing part"`
}

// checksLog returns the failing part of an Actions job's log on the PR's
// current head, or saves the whole log. The job is found on that head, in the
// repository the Work may read, so a call can't name another repository's or
// another commit's job. The log is CI output: the agent reads it as data.
func (t *Tools) checksLog(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in checksLogInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	p, err := t.checksPull(ctx, b, src, in.Number)
	if err != nil {
		return nil, err
	}
	repo, c := src.Settings.Repo, src.Client
	jobs, err := c.HeadJobs(ctx, repo, p.Head.SHA)
	if err != nil {
		return nil, actionsRefusal(repo, err)
	}
	job, err := pickJob(jobs, strings.TrimSpace(in.Check), p.Number)
	if err != nil {
		return nil, err
	}
	if job.Status != "completed" {
		return nil, refuse("job_running", "%q on PR #%d is still %s, and GitHub has no log for it until it finishes", job.Name, p.Number, job.Status)
	}
	log, size, err := c.JobLog(ctx, repo, job.ID)
	if err != nil {
		return nil, actionsRefusal(repo, err)
	}
	if in.SaveTo != "" {
		file, _ := json.Marshal(map[string]any{"name": SafeName(job.Name) + ".log", "content_type": "text/plain; charset=utf-8", "size": len(log),
			"base64": base64.StdEncoding.EncodeToString(log), "save_to": in.SaveTo})
		return domain.Payload{Schema: FileSchema, Data: file}, nil
	}
	excerpt, cut := logExcerpt(log, maxLogExcerpt)
	out := map[string]any{"pr": p.Number, "sha": p.Head.SHA, "check": job.Name, "conclusion": job.Conclusion, "url": job.HTMLURL, "size": size, "truncated": cut, "log": excerpt}
	if cut {
		out["note"] = "this is the part around the job's errors; pass save_to to save the whole log and search it"
	}
	return out, nil
}

// actionsRefusal says what a refused Actions read needs. Minting a token for
// a permission the App wasn't granted is a 422, the read itself a 403.
func actionsRefusal(repo string, err error) error {
	if IsStatus(err, http.StatusForbidden) || IsStatus(err, http.StatusUnprocessableEntity) {
		return refuse("github_forbidden", "this connection can't read GitHub Actions logs on %s: it needs Actions read; ask the owner to grant it to the connection", repo)
	}
	return err
}

// failedJob reports whether a job's conclusion is one github.checks counts
// as a failure of the job itself.
func failedJob(j ActionsJob) bool {
	return j.Conclusion == "failure" || j.Conclusion == "timed_out" || j.Conclusion == "cancelled"
}

// pickJob is the job a call means. A name can recur on one head (two
// workflows, or a push and a pull_request run): a failed one is what the
// caller is after, and the newest of those.
func pickJob(jobs []ActionsJob, check string, pr int) (ActionsJob, error) {
	if check == "" {
		var names []string
		for _, j := range jobs {
			if failedJob(j) && !slices.Contains(names, j.Name) {
				names = append(names, j.Name)
			}
		}
		switch len(names) {
		case 0:
			return ActionsJob{}, refuse("no_failed_job", "no GitHub Actions job has failed on PR #%d's head; a check from another CI keeps its log at the link github.checks gives", pr)
		case 1:
			check = names[0]
		default:
			slices.Sort(names)
			return ActionsJob{}, refuse("missing_field", "several jobs failed on PR #%d; name one as check: %s", pr, strings.Join(names, ", "))
		}
	}
	var best ActionsJob
	for _, j := range jobs {
		if j.Name != check {
			continue
		}
		if best.ID == 0 || failedJob(j) && !failedJob(best) || failedJob(j) == failedJob(best) && j.ID > best.ID {
			best = j
		}
	}
	if best.ID == 0 {
		return ActionsJob{}, refuse("not_found", "PR #%d's head has no GitHub Actions job named %q; a check from another CI keeps its log at the link github.checks gives", pr, check)
	}
	return best, nil
}

// logStamp is the timestamp GitHub starts each log line with.
var logStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)

// logExcerpt is the part of a job's log worth reading, within limit bytes,
// and whether anything was left out: all of a log that fits, else the lines
// around each line the runner marked ##[error], else the log's end. When
// that is still over the limit its end is kept, where a failure's summary is.
func logExcerpt(log []byte, limit int) (string, bool) {
	text := strings.ToValidUTF8(string(bytes.TrimPrefix(log, []byte("\xef\xbb\xbf"))), "�")
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	for i, l := range lines {
		lines[i] = logStamp.ReplaceAllString(strings.TrimRight(l, "\r"), "")
	}
	whole := strings.Join(lines, "\n")
	if len(whole) <= limit {
		return whole, false
	}
	keep := make([]bool, len(lines))
	marked := false
	for i, l := range lines {
		if !strings.Contains(l, "##[error]") {
			continue
		}
		marked = true
		for k := max(0, i-logLinesBefore); k <= min(len(lines)-1, i+logLinesAfter); k++ {
			keep[k] = true
		}
	}
	if !marked {
		for k := max(0, len(lines)-logTailLines); k < len(lines); k++ {
			keep[k] = true
		}
	}
	var out strings.Builder
	for i := 0; i < len(lines); {
		if keep[i] {
			out.WriteString(lines[i])
			out.WriteByte('\n')
			i++
			continue
		}
		from := i
		for i < len(lines) && !keep[i] {
			i++
		}
		fmt.Fprintf(&out, "[... %d lines left out ...]\n", i-from)
	}
	excerpt := strings.TrimRight(out.String(), "\n")
	if len(excerpt) > limit {
		const mark = "[... earlier output left out ...]\n"
		rest := excerpt[len(excerpt)-(limit-len(mark)):]
		// Start on a whole line when the kept part holds a line break.
		if i := strings.IndexByte(rest, '\n'); i >= 0 && i < len(rest)-1 {
			rest = rest[i+1:]
		}
		excerpt = mark + strings.ToValidUTF8(rest, "")
	}
	return excerpt, true
}

// readTail reads r to its end and returns its last limit bytes and its whole
// size, holding at most twice limit however long r is.
func readTail(r io.Reader, limit int) ([]byte, int64, error) {
	var kept []byte
	var size int64
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		kept = append(kept, chunk[:n]...)
		size += int64(n)
		if len(kept) >= 2*limit {
			kept = append(kept[:0], kept[len(kept)-limit:]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}
	return kept, size, nil
}
