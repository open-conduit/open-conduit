package schedule

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
)

// Delivery is a code-host webhook delivery as the composition reads it from
// the host's raw delivery Event (for GitHub, github.delivery). Such raw
// deliveries are non-re-derivable by their own ingress declaration, so a job
// triggered by them shares that declared loss; timed and manual runs do not.
type Delivery struct {
	// EventID is the accepted raw delivery Event, the trigger's causal parent.
	EventID domain.ID
	// ID is the provider's delivery identity.
	ID               string
	Event, Action    string
	Ref, Before, SHA string
	// Conclusion is how a finished check suite or run ended ("failure");
	// empty for other events.
	Conclusion string
	Files      []string
	// Truncated marks a changed-file list that may be incomplete (the
	// delivery hit its commit or file cap): path filters then treat the
	// changed files as unknown and run, rather than skip on a partial list.
	Truncated bool
}

// zeroSHA is the head a push names for a created or deleted branch.
func zeroSHA(sha string) bool { return sha != "" && strings.Trim(sha, "0") == "" }

// Matches reports whether a job runs on the delivery: one of its events
// names it ("push") or it and its action ("release.published"), its ref is on
// one of the job's branches when it has any, and a push changes a file its
// paths take (v1 engine/jobs.go matches and OnEvent), and a check suite or
// run ended one of its conclusions when it lists any. A push whose changed
// files are unknown, or possibly truncated, runs anyway, as in v1 (05-02
// decided against v1's `git diff` in the checkout: the extension reads no
// repository); a deleted branch changes nothing.
func (j Job) Matches(d Delivery) bool {
	if j.On == nil {
		return false
	}
	named := false
	for _, e := range j.On.Events {
		name, action, hasAction := strings.Cut(e, ".")
		if name == d.Event && (!hasAction || action == d.Action) {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	if len(j.On.Branches) > 0 {
		branch, ok := strings.CutPrefix(d.Ref, "refs/heads/")
		if !ok {
			return false
		}
		in := false
		for _, pat := range j.On.Branches {
			if m, _ := path.Match(pat, branch); m {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if len(j.On.Conclusions) > 0 {
		in := false
		for _, c := range j.On.Conclusions {
			if strings.EqualFold(c, d.Conclusion) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if len(j.On.Paths) > 0 && d.Event == "push" {
		if zeroSHA(d.SHA) {
			return false
		}
		if len(d.Files) > 0 && !d.Truncated && !PathsMatch(j.On.Paths, d.Files) {
			return false
		}
	}
	return true
}

func short(sha string) string { return sha[:min(7, len(sha))] }

// EventTrigger is the trigger Event for a job selected by a delivery. Its
// identity is the job plus the raw delivery Event, so reading the same
// delivery twice yields one trigger, and its parent is that delivery.
func EventTrigger(namespace string, j Job, d Delivery, received domain.Event) domain.Event {
	label := d.Event
	if d.Action != "" {
		label += "." + d.Action
	}
	if d.SHA != "" {
		label += " " + short(d.SHA)
	}
	if d.Conclusion != "" {
		label += " " + d.Conclusion
	}
	p := TriggerPayload{Job: j.Name, Kind: KindEvent, Trigger: label, Event: d.Event, Action: d.Action, Ref: d.Ref, Before: d.Before, SHA: d.SHA, Conclusion: d.Conclusion, Files: d.Files, Delivery: d.ID}
	data, _ := json.Marshal(p)
	return domain.Event{ID: domain.ID("schedule-" + digest(namespace, "event", j.Name, string(d.EventID))[:40]), Type: TypeEvent, Source: namespace,
		DeliveryID: "event/" + j.Name + "/" + string(d.EventID), OccurredAt: received.OccurredAt, CausationID: d.EventID,
		CorrelationKey: &domain.CorrelationKey{Source: namespace, ExternalID: JobKey(j.Name)},
		Data:           domain.Payload{Schema: SchemaTrigger, Data: data}}
}

// PathsMatch reports whether any file is taken by the patterns, as GitHub
// Actions reads them: a file is taken when the last pattern matching it does
// not start with ! (v1 engine/jobs.go pathsMatch).
func PathsMatch(patterns, files []string) bool {
	for _, f := range files {
		in := false
		for _, p := range patterns {
			neg := strings.HasPrefix(p, "!")
			if globRe(strings.TrimPrefix(p, "!")).MatchString(f) {
				in = !neg
			}
		}
		if in {
			return true
		}
	}
	return false
}

var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

// globRe is a path glob as a regexp: * and ? within a folder, ** across
// folders, [...] a class.
func globRe(glob string) *regexp.Regexp {
	globMu.Lock()
	defer globMu.Unlock()
	if re, ok := globCache[glob]; ok {
		return re
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			if end := strings.IndexByte(glob[i+1:], ']'); end >= 0 {
				class := glob[i+1 : i+1+end]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				b.WriteString("[" + class + "]")
				i += end + 1
				continue
			}
			b.WriteString(`\[`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		re = regexp.MustCompile("^" + regexp.QuoteMeta(glob) + "$")
	}
	globCache[glob] = re
	return re
}
