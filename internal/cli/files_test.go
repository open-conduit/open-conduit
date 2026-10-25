package cli

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alsiola/open-conduit/client"
)

func TestCallInputWrapsTheOperationInput(t *testing.T) {
	got, err := callInput(json.RawMessage(`{"body":"hi","number":3}`))
	if err != nil || string(got) != `{"data":{"body":"hi","number":3}}` {
		t.Fatalf("wrap: %s %v", got, err)
	}
	got, err = callInput(json.RawMessage(`{"schema":"s.v1","data":{"x":1}}`))
	if err != nil || string(got) != `{"data":{"x":1},"schema":"s.v1"}` {
		t.Fatalf("explicit payload: %s %v", got, err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("png!"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = callInput(json.RawMessage(`{"file_path":"` + path + `"}`))
	var p struct {
		Data map[string]string `json:"data"`
	}
	if err != nil || json.Unmarshal(got, &p) != nil || p.Data["name"] != "shot.png" || p.Data["content_base64"] != base64.StdEncoding.EncodeToString([]byte("png!")) || p.Data["file_path"] != "" {
		t.Fatalf("file input: %s %v", got, err)
	}
	if _, err = callInput(json.RawMessage(`{"file_path":"` + path + `","content_base64":"x"}`)); err == nil {
		t.Fatal("both file_path and content accepted")
	}
	if _, err = callInput(json.RawMessage(`{"file_path":"` + filepath.Join(dir, "missing") + `"}`)); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestFileOutputSavesDownloads(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".local", "img.png")
	data, _ := json.Marshal(map[string]any{"name": "img.png", "content_type": "image/png", "size": 4, "base64": base64.StdEncoding.EncodeToString([]byte("png!")), "save_to": target})
	shown, err := fileOutput(&client.Payload{Schema: FileSchema, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "png!" || shown.(map[string]any)["saved"] != target {
		t.Fatalf("saved: %q %v %v", b, err, shown)
	}
	data, _ = json.Marshal(map[string]any{"name": "img.png", "base64": "cG5nIQ=="})
	shown, err = fileOutput(&client.Payload{Schema: FileSchema, Data: data})
	if err != nil || shown.(map[string]any)["note"] == nil {
		t.Fatalf("no save_to: %v %v", shown, err)
	}
	other := &client.Payload{Schema: "x", Data: json.RawMessage(`{}`)}
	if shown, _ := fileOutput(other); shown != other {
		t.Fatal("other output changed")
	}
}
