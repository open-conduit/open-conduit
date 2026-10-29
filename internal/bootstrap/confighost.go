package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/storage"
)

// configHost is inproc.ConfigHost over the daemon's config service: it
// reads, validates and edits config text, and never applies it.
type configHost struct {
	configs func() *management.ConfigService
	path    string
	def     func() []byte
}

var _ inproc.ConfigHost = configHost{}

func (h configHost) Base(context.Context) (inproc.ConfigBase, error) {
	c := h.configs()
	if c == nil {
		return inproc.ConfigBase{}, errors.New("no configuration yet")
	}
	s, generation := c.Current()
	if s == nil {
		return inproc.ConfigBase{}, errors.New("no configuration yet")
	}
	text, err := config.ReadFile(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		text, err = h.def(), nil
	}
	if err != nil {
		return inproc.ConfigBase{}, errors.New("the config file could not be read")
	}
	b := inproc.ConfigBase{Generation: strconv.FormatUint(generation, 10), Hash: s.Hash, Text: text}
	for id, scope := range s.Scopes {
		if scope.Source != "" {
			b.Folders = append(b.Folders, string(id))
		}
	}
	sort.Strings(b.Folders)
	return b, nil
}

func (h configHost) Validate(_ context.Context, text []byte) (string, []inproc.ConfigDiagnostic, error) {
	c := h.configs()
	if c == nil {
		return "", nil, errors.New("no configuration yet")
	}
	s, err := c.Validate(text)
	var invalid *config.ValidationError
	switch {
	case errors.As(err, &invalid):
		out := make([]inproc.ConfigDiagnostic, len(invalid.Diagnostics))
		for i, d := range invalid.Diagnostics {
			out[i] = inproc.ConfigDiagnostic{Path: d.Path, Code: d.Code, Message: d.Message}
		}
		return "", out, nil
	case err != nil:
		return "", []inproc.ConfigDiagnostic{{Path: "", Code: "invalid", Message: "the configuration is not valid JSON"}}, nil
	}
	return s.Hash, nil, nil
}

func (configHost) Edit(text []byte, op, pointer string, value json.RawMessage) ([]byte, json.RawMessage, error) {
	doc, err := cli.ParseConfigDoc(text)
	if err != nil {
		return nil, nil, errors.New("the config is not a JSON object")
	}
	var before json.RawMessage
	if !strings.HasSuffix(pointer, "/-") {
		if b, err := doc.Raw(pointer); err == nil {
			before = b
		}
	}
	switch op {
	case "add":
		err = doc.Set(pointer, string(value))
	case "replace":
		if before == nil {
			return nil, nil, errors.New("nothing is at " + pointer + " to replace")
		}
		err = doc.Set(pointer, string(value))
	case "remove":
		if before == nil {
			return nil, nil, errors.New("nothing is at " + pointer + " to remove")
		}
		err = doc.Unset(pointer)
	default:
		return nil, nil, errors.New("op is add, replace or remove")
	}
	if err != nil {
		return nil, nil, err
	}
	return doc.Bytes(), before, nil
}

// configCandidate is the configuration a Work proposes, from the in-process
// extension that owns its kind (GET /work/{id}/config-candidate).
func (d *daemon) configCandidate(ctx context.Context, w domain.Work) (extension.Candidate, error) {
	for _, x := range d.inproc {
		if c, ok := x.(extension.ConfigCandidate); ok && (ownership{x: x}).workKind(w.Kind) {
			return c.ConfigCandidate(ctx, w)
		}
	}
	return extension.Candidate{}, storage.ErrNotFound
}
