package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/client"
)

// Generic toolbelt file conventions. Extension operations run in the daemon,
// not in the agent's directory, so the `os` client moves file content for
// them: an input object's `file_path` is read here and sent as
// `content_base64` (with `name`), and an output with FileSchema is written
// to its `save_to` path here instead of being shown to the agent.

// FileSchema is the toolbelt file result schema.
const FileSchema = "open-conduit.toolbelt.file.v1"

// maxToolbeltFile bounds a file moved through the toolbelt.
const maxToolbeltFile = 50 << 20

// fileInput fills content_base64 from file_path in an operation's input
// object. Anything else passes through unchanged.
func fileInput(data json.RawMessage) (json.RawMessage, error) {
	var in map[string]json.RawMessage
	if len(bytes.TrimSpace(data)) == 0 || json.Unmarshal(data, &in) != nil {
		return data, nil
	}
	raw, ok := in["file_path"]
	if !ok {
		return data, nil
	}
	var path string
	if json.Unmarshal(raw, &path) != nil || path == "" {
		return nil, errors.New("file_path must be a path")
	}
	if _, has := in["content_base64"]; has {
		return nil, errors.New("give file_path or content_base64, not both")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file_path %s is not a readable file", path)
	}
	if info.Size() > maxToolbeltFile {
		return nil, fmt.Errorf("file_path %s is over %d MB", path, maxToolbeltFile>>20)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("file_path %s is not readable", path)
	}
	delete(in, "file_path")
	in["content_base64"], _ = json.Marshal(base64.StdEncoding.EncodeToString(content))
	if _, has := in["name"]; !has {
		in["name"], _ = json.Marshal(filepath.Base(path))
	}
	return json.Marshal(in)
}

// fileOutput writes a FileSchema result to its save_to path and returns what
// to show instead of the content.
func fileOutput(out *client.Payload) (any, error) {
	if out == nil || out.Schema != FileSchema {
		return out, nil
	}
	var f struct {
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		Size        int    `json:"size"`
		Base64      string `json:"base64"`
		SaveTo      string `json:"save_to"`
	}
	if err := json.Unmarshal(out.Data, &f); err != nil {
		return nil, errors.New("malformed file result")
	}
	shown := map[string]any{"name": f.Name, "content_type": f.ContentType, "size": f.Size}
	if f.SaveTo == "" {
		shown["note"] = "pass save_to to save the file"
		return shown, nil
	}
	content, err := base64.StdEncoding.DecodeString(f.Base64)
	if err != nil {
		return nil, errors.New("malformed file content")
	}
	if dir := filepath.Dir(f.SaveTo); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cannot create %s", dir)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.SaveTo), ".os-download-*")
	if err != nil {
		return nil, fmt.Errorf("cannot write %s", f.SaveTo)
	}
	_, err = tmp.Write(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), f.SaveTo)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("cannot write %s", f.SaveTo)
	}
	shown["saved"] = f.SaveTo
	return shown, nil
}
