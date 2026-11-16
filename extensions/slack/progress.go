package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Progress messages (issue #673). With progress enabled, each run of a
// conversation gets one message in its thread that shows what the run is
// doing: a Slack plan block with a task per child Work it handed to and per
// approval it waits on, and a Stop button for the people the entry lists.
// A run is what one person's message starts or wakes: it begins with that
// message (which gets an eyes reaction) and lasts until the next one.
//
// Nothing here runs with the run. Each projection pass derives every live
// run's plan from durable state (the Work, its children, its Waits and the
// actions offered on it), so a run parked for hours, or a daemon restarted
// in between, shows the same plan. The message is posted once, as a marked
// intent (Deliver), and then changed in place with chat.update only when
// what it shows changes, at most once a pass. The core and the agent are
// not involved: what a child or a gate means stays its owner's, and the
// plan shows only their state.

// stopAction is the action_id of a progress message's Stop button.
const stopAction = "oc_stop"

// CancelFunc cancels Work of the extension's own kinds (inproc.Host.CancelWork).
type CancelFunc func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error)

// Reactions the person's message gets: while the run is live, and once it
// has handed back, failed or been stopped.
const (
	reactLive    = "eyes"
	reactDone    = "white_check_mark"
	reactFailed  = "x"
	reactStopped = "octagonal_sign"
)

// Plan task states, as Slack names them.
const (
	taskActive   = "in_progress"
	taskComplete = "complete"
	taskError    = "error"
)

// maxTasks is Slack's limit on a plan's tasks.
const maxTasks = 50

// progressRecord is one run's progress message: what it is for, and what
// was last shown, so a pass changes Slack only when the plan changes.
type progressRecord struct {
	Work domain.ID `json:"work"`
	// Run is the person's message Event the run began with; Seq its
	// sequence, from which a pass looks for the next one.
	Run     domain.ID `json:"run"`
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"at"`
	Channel string    `json:"channel"`
	Thread  string    `json:"thread"`
	// Message is the person's message's ts, which carries the reaction.
	Message string `json:"message"`
	Binding string `json:"binding,omitempty"`
	// Digest is of the blocks last shown; Reaction the reaction last set.
	Digest   string `json:"digest,omitempty"`
	Reaction string `json:"reaction,omitempty"`
	// Steps are what the run has waited on (an approval, a question, a
	// Wait that says what it is for), in the order first seen; one no
	// longer there is done.
	Steps []progressStep `json:"steps,omitempty"`
	// Ended: a later message began the next run, so this one shows its
	// last state. Done: it shows its last state and the reaction matches,
	// so no pass reads it again.
	Ended bool `json:"ended,omitempty"`
	Done  bool `json:"done,omitempty"`
	// Shown: the run has been live in a pass, so it has a message.
	Shown bool `json:"shown,omitempty"`
}

type progressStep struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Open  bool   `json:"open"`
}

func progressKey(work, run domain.ID) string {
	return "progress/" + digest(string(work), string(run))[:40]
}

// progressIntent is the intent ID of a run's progress message.
func progressIntent(work, run domain.ID) string {
	return "progress/" + string(work) + "/" + string(run)
}

// progressText is the progress message's text when posted: its
// notification, which a later update replaces with the plan's title.
const progressText = "Working on it"

// runView is what a pass reads for one run.
type runView struct {
	rec      progressRecord
	work     domain.Work
	children []domain.Work
	// latest is the person's message Event the Work's latest run began with
	// (nil when none).
	latest *domain.AcceptedEvent
}

// progressRuns reads every run whose message is live: the latest run of
// each conversation that is not finished, and every run with a record not
// yet done.
func (p *Projector) progressRuns(ctx context.Context) ([]runView, error) {
	var out []runView
	err := p.transact(ctx, func(tx storage.Tx) error {
		out = nil
		pending := map[domain.ID][]progressRecord{}
		rows, err := tx.List(p.ns, "progress/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var rec progressRecord
			if json.Unmarshal(row.Value, &rec) == nil && !rec.Done {
				pending[rec.Work] = append(pending[rec.Work], rec)
			}
		}
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		children := map[domain.ID][]domain.Work{}
		for _, w := range works {
			if w.ParentID != "" {
				children[w.ParentID] = append(children[w.ParentID], w)
			}
		}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope || w.Kind != WorkKind {
				continue
			}
			recs := pending[w.ID]
			if w.State.Terminal() && len(recs) == 0 {
				continue
			}
			channel, thread, ok := p.Thread(w)
			if !ok {
				continue
			}
			var after uint64
			for _, r := range recs {
				after = max(after, r.Seq)
			}
			latest, err := p.latestRun(tx, w, channel, thread, after)
			if err != nil {
				return err
			}
			if latest == nil && len(recs) > 0 {
				// No newer message: the newest record is the latest run.
				sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
				r := recs[len(recs)-1]
				if a, err := tx.Event(r.Run); err == nil {
					latest = &a
				}
			}
			kids := children[w.ID]
			sort.Slice(kids, func(i, j int) bool { return kids[i].CreatedAt.Before(kids[j].CreatedAt) })
			binding := ""
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			var top uint32
			for _, x := range xs {
				if x.Attempt >= top {
					top, binding = x.Attempt, x.ExecutorBinding
				}
			}
			for _, r := range recs {
				v := runView{rec: r, work: w, latest: latest}
				if latest == nil || r.Run != latest.Event.ID {
					v.rec.Ended = true
				}
				v.children = runChildren(kids, r, v.rec.Ended, latest)
				out = append(out, v)
			}
			if latest == nil || w.State.Terminal() {
				continue
			}
			if slices.ContainsFunc(recs, func(r progressRecord) bool { return r.Run == latest.Event.ID }) {
				continue
			}
			var pl MessagePayload
			_ = json.Unmarshal(latest.Event.Data.Data, &pl)
			rec := progressRecord{Work: w.ID, Run: latest.Event.ID, Seq: latest.Sequence, At: latest.Event.ReceivedAt, Channel: channel, Thread: thread, Message: pl.TS, Binding: binding}
			out = append(out, runView{rec: rec, work: w, latest: latest, children: runChildren(kids, rec, false, latest)})
		}
		return nil
	})
	return out, err
}

// runChildren are the child Work the run made: made once its message was
// accepted and, for a run a later message ended, before that one was.
func runChildren(kids []domain.Work, rec progressRecord, ended bool, latest *domain.AcceptedEvent) []domain.Work {
	var out []domain.Work
	for _, k := range kids {
		if k.CreatedAt.Before(rec.At) {
			continue
		}
		if ended && latest != nil && latest.Event.ID != rec.Run && !k.CreatedAt.Before(latest.Event.ReceivedAt) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// latestRun is the latest person's message after sequence after that was
// routed to w as input in its own thread: the message its latest run began
// with. An edit or a deletion begins no run.
func (p *Projector) latestRun(tx storage.Tx, w domain.Work, channel, thread string, after uint64) (*domain.AcceptedEvent, error) {
	var found *domain.AcceptedEvent
	for {
		page, err := tx.WorkEvents(w.ID, after, 500)
		if err != nil {
			return nil, err
		}
		for i := range page {
			a := page[i]
			after = a.Sequence
			e := a.Event
			if e.Type != TypeMessageHuman || e.Source != p.o.Settings.Namespace || !a.Routed || a.RoutedWorkID != w.ID || e.Data.Schema != SchemaMessage {
				continue
			}
			var pl MessagePayload
			if json.Unmarshal(e.Data.Data, &pl) != nil || pl.Edited != "" || pl.Deleted || pl.Channel != channel || !tsPattern.MatchString(pl.TS) {
				continue
			}
			if pl.Thread != thread && pl.TS != thread {
				continue
			}
			found = &a
		}
		if len(page) < 500 {
			return found, nil
		}
	}
}

// runPlan is what a run's progress message shows now.
type runPlan struct {
	title    string
	reaction string
	// live: the run is working or waiting on something other than the
	// person, so it shows Stop and the live reaction.
	live  bool
	tasks []map[string]any
}

// plan derives what v's message shows from the run's state, the steps it
// has waited on (rec.Steps, updated by the caller) and its live waits.
func (v runView) plan(waits []domain.Wait) runPlan {
	w := v.work
	liveKids := 0
	var tasks []map[string]any
	for _, k := range v.children {
		status := taskActive
		switch k.State {
		case domain.WorkCompleted:
			status = taskComplete
		case domain.WorkFailed, domain.WorkCancelled:
			status = taskError
		default:
			liveKids++
		}
		title := ""
		if k.Display != nil {
			title = strings.TrimSpace(k.Display.Title)
		}
		if title == "" {
			title = "Hand-off: " + k.Kind
		}
		tasks = append(tasks, planTask("c"+digest(string(k.ID))[:16], title, status))
	}
	openSteps := 0
	for _, s := range v.rec.Steps {
		status := taskComplete
		if s.Open {
			status, openSteps = taskActive, openSteps+1
		}
		tasks = append(tasks, planTask("s"+s.ID, s.Title, status))
	}
	pl := runPlan{live: true, reaction: reactLive}
	switch {
	case v.rec.Ended:
		pl.title, pl.live, pl.reaction = "Done", false, reactDone
	case w.State == domain.WorkCompleted:
		pl.title, pl.live, pl.reaction = "Done", false, reactDone
	case w.State == domain.WorkFailed:
		pl.title, pl.live, pl.reaction = "Failed", false, reactFailed
	case w.State == domain.WorkCancelled:
		pl.title, pl.live, pl.reaction = "Stopped", false, reactStopped
	case w.State == domain.WorkRunning:
		pl.title = "Working"
	case openSteps > 0 && w.State != domain.WorkRunning:
		pl.title = "Waiting"
	case liveKids > 0:
		pl.title = "Waiting for hand-offs"
	case w.State == domain.WorkReady:
		pl.title = "Starting"
	case awaitsOther(waits):
		pl.title = "Waiting"
	default:
		// Waiting for the person only: the run has handed back.
		pl.title, pl.live, pl.reaction = "Waiting for your reply", false, reactDone
	}
	if !pl.live {
		// Whatever was in flight when the run ended is not anymore.
		for _, t := range tasks {
			if t["status"] == taskActive {
				t["status"] = taskComplete
				if w.State == domain.WorkFailed || w.State == domain.WorkCancelled {
					t["status"] = taskError
				}
			}
		}
	}
	if len(tasks) == 0 {
		status := taskActive
		switch {
		case pl.reaction == reactDone:
			status = taskComplete
		case !pl.live:
			status = taskError
		}
		tasks = append(tasks, planTask("run", pl.title, status))
	}
	if len(tasks) > maxTasks {
		tasks = tasks[len(tasks)-maxTasks:]
	}
	pl.tasks = tasks
	return pl
}

// awaitsOther reports whether an active Wait is for something other than
// a person writing in the thread (a timer, another system's state).
func awaitsOther(waits []domain.Wait) bool {
	for _, wt := range waits {
		if wt.State == domain.WaitActive && !waitsOnPerson(wt) {
			return true
		}
	}
	return false
}

func waitsOnPerson(wt domain.Wait) bool {
	for _, t := range wt.Match.Types {
		if t == TypeMessageHuman {
			return true
		}
	}
	return false
}

func planTask(id, title, status string) map[string]any {
	if len(title) > 150 {
		title = strings.ToValidUTF8(title[:147], "") + "..."
	}
	return map[string]any{"task_id": id, "title": title, "status": status}
}

// progressBlocks lays the plan out: the plan block and, while the run is
// live and the entry names who may stop it, a Stop button. Each layout gets
// its own block IDs, as Slack asks of an updated plan. The digest is of
// what is shown, so an unchanged plan is never sent again.
func (p *Projector) progressBlocks(work domain.ID, pl runPlan) ([]map[string]any, string) {
	raw, _ := json.Marshal([]any{pl.title, pl.tasks, pl.live && len(p.o.Settings.StopUsers) > 0})
	d := digest(string(raw))[:16]
	blocks := []map[string]any{{"type": "plan", "block_id": "oc_plan_" + d, "title": pl.title, "tasks": pl.tasks}}
	if pl.live && len(p.o.Settings.StopUsers) > 0 {
		v, _ := json.Marshal(buttonValue{Work: work})
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "oc_stop_" + d, "elements": []map[string]any{{
			"type": "button", "action_id": stopAction, "style": "danger", "value": string(v),
			"text": map[string]any{"type": "plain_text", "text": "Stop"},
			"confirm": map[string]any{
				"title":   map[string]any{"type": "plain_text", "text": "Stop this run?"},
				"text":    map[string]any{"type": "plain_text", "text": "The agent stops, and this conversation ends."},
				"confirm": map[string]any{"type": "plain_text", "text": "Stop"},
				"deny":    map[string]any{"type": "plain_text", "text": "Keep going"},
			},
		}}})
	}
	return blocks, d
}

// stepID names an awaited action or a Wait as a step of the plan.
func stepID(parts ...string) string { return digest(parts...)[:16] }

// steps updates rec's steps from what the run waits on now: the awaited
// actions offered on its Work (an approval, a question) and its active
// Waits that say what they are for. A step no longer there is done.
func (p *Projector) steps(ctx context.Context, v *runView, waits []domain.Wait) {
	open := map[string]string{}
	var order []string
	add := func(id, title string) {
		if _, ok := open[id]; !ok && strings.TrimSpace(title) != "" {
			open[id] = title
			order = append(order, id)
		}
	}
	w := v.work
	if !v.rec.Ended && !w.State.Terminal() {
		if p.o.Actions != nil && (w.State == domain.WorkWaiting || w.State == domain.WorkReady) {
			if offers, err := p.o.Actions(ctx, domain.ID(p.o.Settings.Scope), w.ID); err == nil {
				for _, o := range offers {
					if o.Awaited {
						add(stepID("action", o.Provider, o.ID, o.Label, o.Detail), o.Label)
					}
				}
			}
		}
		for _, wt := range waits {
			if wt.State == domain.WaitActive && !waitsOnPerson(wt) && wt.Description != "" {
				title := "Waiting for " + wt.Description
				if wt.Progress != "" {
					title += " (" + wt.Progress + ")"
				}
				add(stepID("wait", string(wt.ID)), title)
			}
		}
	}
	seen := map[string]bool{}
	for i := range v.rec.Steps {
		s := &v.rec.Steps[i]
		seen[s.ID] = true
		title, ok := open[s.ID]
		s.Open = ok
		if ok {
			s.Title = title
		}
	}
	for _, id := range order {
		if !seen[id] {
			v.rec.Steps = append(v.rec.Steps, progressStep{ID: id, Title: open[id], Open: true})
		}
	}
}

// Progress syncs every live run's progress message with its run, at most
// limit Slack writes, and reports how many it changed. Rate limits and
// credential failures stop the pass; another failure is reported as a code
// and retried next pass.
func (p *Projector) Progress(ctx context.Context, limit int) (int, []string, error) {
	if !p.o.Settings.Progress {
		return 0, nil, nil
	}
	runs, err := p.progressRuns(ctx)
	if err != nil {
		return 0, nil, err
	}
	changed := 0
	var codes []string
	for i := range runs {
		if changed >= limit {
			break
		}
		n, err := p.syncRun(ctx, &runs[i])
		changed += n
		var rate *RateLimitError
		switch {
		case errors.As(err, &rate), errors.Is(err, ErrCredential):
			return changed, append(codes, "progress:"+ErrorCode(err)), err
		case err != nil:
			codes = append(codes, "progress:"+ErrorCode(err))
		}
	}
	return changed, codes, nil
}

// syncRun brings one run's message, and the reaction on the person's
// message, up to date, recording each step as it is made.
func (p *Projector) syncRun(ctx context.Context, v *runView) (int, error) {
	var waits []domain.Wait
	if err := p.transact(ctx, func(tx storage.Tx) error {
		var err error
		waits, err = tx.ListWaits(v.work.ID)
		return err
	}); err != nil {
		return 0, err
	}
	p.steps(ctx, v, waits)
	pl := v.plan(waits)
	rec := &v.rec
	if !rec.Shown && !pl.live {
		// A run that handed back before any pass saw it live needs no
		// message; it is recorded, so its events are not read again, and
		// shown if it goes on (a timer wakes it).
		rec.Done = rec.Ended || v.work.State.Terminal()
		return 0, p.saveRun(ctx, *rec)
	}
	rec.Shown = true
	blocks, d := p.progressBlocks(v.work.ID, pl)
	if err := p.saveRun(ctx, *rec); err != nil {
		return 0, err
	}
	if p.o.Settings.Projection.DryRun {
		// Nothing is written to Slack, but a run that has ended still
		// retires, so passes don't keep reading every run ever seen.
		if !pl.live && (rec.Ended || v.work.State.Terminal()) {
			rec.Done = true
			return 0, p.saveRun(ctx, *rec)
		}
		return 0, nil
	}
	id := progressIntent(rec.Work, rec.Run)
	st, err := p.Lookup(ctx, id)
	if err != nil {
		return 0, err
	}
	changed := 0
	if st.State == IntentFailed {
		// Slack refused the post: nothing to update; the reaction still
		// says how the run went.
		rec.Digest = d
	} else if st.State != IntentPosted {
		state, err := p.Deliver(ctx, Intent{ID: id, Work: rec.Work, Channel: rec.Channel, Thread: rec.Thread, Text: progressText, Blocks: blocks, Binding: rec.Binding})
		switch {
		case err != nil:
			return 0, err
		case state == IntentPosted:
			rec.Digest, changed = d, 1
		case state == IntentRecovered:
			// Found by its marker: what it shows is not known, so it is
			// updated below.
		default:
			return 0, nil
		}
		if st, err = p.Lookup(ctx, id); err != nil {
			return changed, err
		}
	}
	if st.State == IntentPosted && rec.Digest != d {
		if err := p.o.Client.UpdateMessage(ctx, Update{Channel: rec.Channel, TS: st.TS, Text: pl.title, Blocks: blocks}); err != nil {
			return changed, err
		}
		rec.Digest, changed = d, changed+1
	}
	if err := p.saveRun(ctx, *rec); err != nil {
		return changed, err
	}
	if rec.Reaction != pl.reaction && rec.Message != "" {
		if rec.Reaction != "" {
			if err := p.o.Client.RemoveReaction(ctx, rec.Channel, rec.Message, rec.Reaction); err != nil {
				return changed, err
			}
		}
		if err := p.o.Client.AddReaction(ctx, rec.Channel, rec.Message, pl.reaction); err != nil {
			return changed, err
		}
		rec.Reaction = pl.reaction
		changed++
	}
	if !pl.live && (rec.Ended || v.work.State.Terminal()) {
		rec.Done = true
	}
	return changed, p.saveRun(ctx, *rec)
}

func (p *Projector) saveRun(ctx context.Context, rec progressRecord) error {
	return p.transact(ctx, func(tx storage.Tx) error { return p.put(tx, progressKey(rec.Work, rec.Run), rec) })
}

// stop takes a press of a progress message's Stop button: Slack vouches
// for who pressed it, the entry's progress stop list says whether they may,
// and the host cancels the Work as the cancellation of a Work of this
// extension's own kind. The press is idempotent per press (its action_ts),
// and the person is told the outcome in a message only they see.
func (p *Projector) stop(ctx context.Context, in interaction, value, at string) error {
	var v buttonValue
	if json.Unmarshal([]byte(value), &v) != nil || v.Work == "" || !tsPattern.MatchString(at) {
		return nil
	}
	user := in.User.ID
	msg := ""
	var err error
	switch {
	case p.o.Cancel == nil || !p.o.Settings.Progress:
		return nil
	case !p.o.Settings.MayStop(user):
		msg = "You are not one of the people who may stop this run."
	default:
		msg, err = p.cancel(ctx, v.Work, in.Container.ChannelID, user, at)
	}
	thread := in.Container.ThreadTS
	if thread == "" {
		thread = in.Container.MessageTS
	}
	if p.o.Settings.Projection.DryRun || msg == "" {
		return err
	}
	if e := p.o.Client.PostEphemeral(ctx, Ephemeral{Channel: in.Container.ChannelID, User: user, ThreadTS: thread, Text: msg}); e != nil && err == nil {
		err = e
	}
	return err
}

// cancel cancels work for user, when it is a live conversation of this
// entry whose thread is in channel. A revision that moved under it (the
// run went on) is read again, a few times.
func (p *Projector) cancel(ctx context.Context, work domain.ID, channel, user, at string) (string, error) {
	reason := "stopped from Slack by slack:" + user
	key := "slack-stop/" + user + "/" + at
	for range 3 {
		var w domain.Work
		err := p.transact(ctx, func(tx storage.Tx) error { var e error; w, e = tx.Work(work); return e })
		if errors.Is(err, storage.ErrNotFound) {
			return "That run is no longer here.", nil
		}
		if err != nil {
			return "", err
		}
		c, _, ok := p.Thread(w)
		if !ok || c != channel || string(w.ScopeID) != p.o.Settings.Scope || w.Kind != WorkKind {
			return "", nil
		}
		if w.State.Terminal() {
			return "That run has already ended.", nil
		}
		_, err = p.o.Cancel(ctx, work, w.Revision, reason, key)
		switch {
		case err == nil:
			return "Stopped.", nil
		case errors.Is(err, storage.ErrConflict):
			continue
		}
		return "Open Conduit couldn't stop that run just now. Please press Stop again.", err
	}
	return "Open Conduit couldn't stop that run just now. Please press Stop again.", storage.ErrConflict
}
