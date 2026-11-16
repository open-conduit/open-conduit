package slack_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestMaxUploadFitsOneProtocolMessage: the largest upload, as base64 in a
// call to the out-of-process extension, stays within one protocol message.
func TestMaxUploadFitsOneProtocolMessage(t *testing.T) {
	if encoded := (slack.MaxUpload + 2) / 3 * 4; encoded+(64<<10) >= protocol.MaxMessageBytes {
		t.Fatalf("a %d-byte upload is %d bytes of base64, past the %d-byte protocol message", slack.MaxUpload, encoded, protocol.MaxMessageBytes)
	}
}

func TestUploadsConfig(t *testing.T) {
	base := `{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"]`
	off, err := slack.ParseExtension([]byte(base+`}`), nil)
	if err != nil || off.Uploads || off.MayUpload(1, "image/png") {
		t.Fatalf("uploads are off without the setting: %+v %v", off, err)
	}
	on, err := slack.ParseExtension([]byte(base+`,"uploads":{"max_bytes":1000,"media_types":["image/*","application/pdf"]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		size int
		typ  string
		want bool
	}{
		{10, "image/png", true},
		{1000, "image/jpeg", true},
		{1001, "image/png", false},
		{0, "image/png", false},
		{10, "application/pdf", true},
		{10, "text/plain; charset=utf-8", false},
		{10, "application/zip", false},
	} {
		if got := on.MayUpload(c.size, c.typ); got != c.want {
			t.Fatalf("MayUpload(%d, %q) = %v", c.size, c.typ, got)
		}
	}
	anything, err := slack.ParseExtension([]byte(base+`,"uploads":{"media_types":["*/*"]}}`), nil)
	if err != nil || anything.UploadMax != slack.MaxUpload || !anything.MayUpload(slack.MaxUpload, "text/plain; charset=utf-8") || anything.MayUpload(slack.MaxUpload+1, "image/png") {
		t.Fatalf("defaults: %+v %v", anything, err)
	}
	for _, bad := range []string{
		`,"uploads":{}}`,
		`,"uploads":{"media_types":[]}}`,
		`,"uploads":{"media_types":["image"]}}`,
		`,"uploads":{"media_types":["Image/PNG"]}}`,
		`,"uploads":{"media_types":["*/png"]}}`,
		`,"uploads":{"media_types":["image/png","image/png"]}}`,
		`,"uploads":{"max_bytes":-1,"media_types":["image/png"]}}`,
		`,"uploads":{"max_bytes":10485761,"media_types":["image/png"]}}`,
		`,"uploads":{"media_types":["image/png"],"types":["x"]}}`,
	} {
		if _, err := slack.ParseExtension([]byte(base+bad), nil); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestDoctorSaysWhatAMissingFilesScopeCosts(t *testing.T) {
	f := newFixture(t, "", slack.IngressOptions{})
	ctx := context.Background()
	without := slices.DeleteFunc(slices.Clone(slack.BotScopes), func(s string) bool { return s == slack.FilesScope })
	f.fake.SetScopes(without)
	r := slack.CheckAuth(ctx, f.client, settings(t, ""))
	if r.Severity != slack.DoctorWarn || !strings.Contains(r.Item, slack.FilesScope) || !strings.Contains(r.Item, "slack.upload is refused") {
		t.Fatalf("an entry without uploads warns: %+v", r)
	}
	r = slack.CheckAuth(ctx, f.client, settings(t, `,"uploads":{"media_types":["image/png"]}`))
	if r.Severity != slack.DoctorFail || !strings.Contains(r.Item, "every slack.upload is refused") {
		t.Fatalf("an entry with uploads fails: %+v", r)
	}
	f.fake.SetScopes(slack.BotScopes)
	if r := slack.CheckAuth(ctx, f.client, settings(t, `,"uploads":{"media_types":["image/png"]}`)); r.Severity != slack.DoctorOK {
		t.Fatalf("granted: %+v", r)
	}
}
