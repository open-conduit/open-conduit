package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
)

// slUploadCall is the slack tools' operations bound to one attempt.
func slUploadCall(t *testing.T, e *slEnv, w domain.Work, attempt domain.ID) func(op, request, input string) (map[string]any, error) {
	t.Helper()
	tools, err := sl.NewTools(sl.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []sl.Source {
		return []sl.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]extension.Operation{}
	for _, op := range tools.Registration().Operations {
		ops[op.Name] = op
	}
	auth := extension.Authority{ScopeID: slScope, WorkID: w.ID, ExecutionID: attempt}
	return func(op, request, input string) (map[string]any, error) {
		ctx := extension.WithCallRequestID(context.Background(), request)
		out, err := ops[op].Handler(ctx, auth, domain.Payload{Schema: "json", Data: json.RawMessage(input)})
		if err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(out.Data, &m)
		return m, nil
	}
}

func slFile(content string) string {
	data, _ := json.Marshal(map[string]string{"name": "shot.png", "title": "The login page", "content_base64": base64.StdEncoding.EncodeToString([]byte(content))})
	return string(data)
}

const slPNG = "\x89PNG\r\n\x1a\n0000IHDR"

// TestSlackUploadIsOnceAndWithinTheEntrysLimits drives slack.upload: a
// retried call shares the file once, a share whose answer was lost is found
// again by the file's ID (and never shared twice), a file outside the
// entry's limits is refused before Slack is called, and a refusal is not
// retried.
func TestSlackUploadIsOnceAndWithinTheEntrysLimits(t *testing.T) {
	e := newSlackEnv(t, `,"uploads":{"max_bytes":64,"media_types":["image/png"]}`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "The login page looks broken")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	call := slUploadCall(t, e, w, e.startAttempt(w.ID))

	first, err := call(sl.OpUpload, "u1", slFile(slPNG+"a"))
	if err != nil || first["state"] != sl.IntentPosted || first["file"] == "" {
		t.Fatalf("upload: %v %v", first, err)
	}
	if got, ok := e.fake.FileContent(first["file"].(string)); !ok || string(got) != slPNG+"a" {
		t.Fatalf("content sent: %q", got)
	}
	again, err := call(sl.OpUpload, "u1", slFile(slPNG+"a"))
	if err != nil || again["file"] != first["file"] || len(e.fake.Uploads(slChannel, root)) != 1 || e.fake.Calls("files.getUploadURLExternal") != 1 {
		t.Fatalf("a retried call uploaded again: %v %v", again, err)
	}
	var op *extension.OperationError
	if _, err := call(sl.OpUpload, "u1", slFile(slPNG+"b")); !errors.As(err, &op) || op.Code != "intent_conflict" {
		t.Fatalf("a reused request ID with another file: %v", err)
	}

	// The share is made but its answer is lost: the call says pending, and
	// once the search delay has passed the projector finds the file.
	e.fake.FailNextComplete("lost")
	lost, err := call(sl.OpUpload, "u2", slFile(slPNG+"c"))
	if err != nil || lost["state"] != "pending" {
		t.Fatalf("lost answer: %v %v", lost, err)
	}
	if res := e.converge(); res.Recovered+res.Posted != 0 || len(e.fake.Uploads(slChannel, root)) != 2 {
		t.Fatalf("shared again before the search delay: %+v", res)
	}
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 || len(e.fake.Uploads(slChannel, root)) != 2 {
		t.Fatalf("lost share recovery: %+v", res)
	}
	if again, err := call(sl.OpUpload, "u2", slFile(slPNG+"c")); err != nil || again["state"] != sl.IntentPosted {
		t.Fatalf("after recovery: %v %v", again, err)
	}
	e.clock.Add(time.Hour)
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.Uploads(slChannel, root)) != 2 || e.fake.Calls("files.getUploadURLExternal") != 2 {
		t.Fatalf("file shared twice: %+v", res)
	}

	// The share is refused without being made: the same file is shared
	// once the search delay shows it isn't in the thread.
	e.fake.FailNextComplete("refuse:internal_error")
	if _, err := call(sl.OpUpload, "u3", slFile(slPNG+"d")); !errors.As(err, &op) || op.Code != "slack_refused" {
		t.Fatalf("refusal: %v", err)
	}
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.Uploads(slChannel, root)) != 2 {
		t.Fatalf("a refused upload was retried: %+v", res)
	}

	// The host's limits, before Slack is called.
	reserved := e.fake.Calls("files.getUploadURLExternal")
	for input, code := range map[string]string{
		slFile(slPNG + string(make([]byte, 64))): "too_large",
		slFile("plain text, not an image"):       "media_type_not_allowed",
		`{"name":"shot.png"}`:                    "missing_field",
		`{"file_path":"shot.png"}`:               "invalid_input",
	} {
		if _, err := call(sl.OpUpload, "", input); !errors.As(err, &op) || op.Code != code {
			t.Fatalf("%s: %v, want %s", input, err, code)
		}
	}
	if e.fake.Calls("files.getUploadURLExternal") != reserved {
		t.Fatal("a refused file reached Slack")
	}
	files := 0
	for _, m := range e.fake.Thread(slChannel, root) {
		files += len(m.Files)
	}
	if files != 2 {
		t.Fatalf("files in the thread: %d", files)
	}
}

// TestSlackUploadNeedsTheEntrysUploadsAndTheScope: an entry without uploads
// refuses every file, and a token without files:write is told so.
func TestSlackUploadNeedsTheEntrysUploadsAndTheScope(t *testing.T) {
	off := newSlackEnv(t, "", sl.IngressOptions{})
	root := off.fake.Post(slChannel, slAlice, "Here's what I see")
	off.sync()
	off.reconcile()
	off.route()
	w, _ := off.workFor(root)
	var op *extension.OperationError
	if _, err := slUploadCall(t, off, w, off.startAttempt(w.ID))(sl.OpUpload, "u", slFile(slPNG)); !errors.As(err, &op) || op.Code != "uploads_off" {
		t.Fatalf("uploads off: %v", err)
	}

	on := newSlackEnv(t, `,"uploads":{"media_types":["*/*"]}`, sl.IngressOptions{})
	on.fake.SetScopes(slices.DeleteFunc(slices.Clone(sl.BotScopes), func(s string) bool { return s == sl.FilesScope }))
	root = on.fake.Post(slChannel, slAlice, "Here's what I see")
	on.sync()
	on.reconcile()
	on.route()
	w, _ = on.workFor(root)
	if _, err := slUploadCall(t, on, w, on.startAttempt(w.ID))(sl.OpUpload, "u", slFile(slPNG)); !errors.As(err, &op) || op.Code != "upload_scope_missing" {
		t.Fatalf("scope missing: %v", err)
	}
}
