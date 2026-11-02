package claudecode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Live implements extension.LiveExecutor: the running segment's activity so
// far, its raw transcript and its stderr, the outputs its result will
// publish as artifacts. A segment that has recorded its result is not live.
func (e *Executor) Live(ctx context.Context, id domain.ID, tail int64) ([]extension.LiveOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := e.liveSegment(id)
	if err != nil {
		return nil, err
	}
	transcript := filepath.Join(dir, "out.jsonl")
	activity := []byte(RenderActivity(readText(transcript)))
	out := []extension.LiveOutput{{Name: "activity", MediaType: ActivityMediaType, Size: int64(len(activity)), Content: tailBytes(activity, tail)}}
	for _, f := range []struct{ name, file, media string }{{"transcript", "out.jsonl", TranscriptMediaType}, {"stderr", "err.log", LogMediaType}} {
		size, content, err := readTail(filepath.Join(dir, f.file), tail)
		if err != nil {
			return nil, err
		}
		out = append(out, extension.LiveOutput{Name: f.name, MediaType: f.media, Size: size, Content: content})
	}
	return out, nil
}

// liveSegment is the execution's newest segment directory with no recorded
// result.
func (e *Executor) liveSegment(id domain.ID) (string, error) {
	base := filepath.Join(e.o.StateDir, "runs", hashKey(string(id)))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return "", extension.ErrNotLive
	} else if err != nil {
		return "", err
	}
	var dir string
	var newest int64
	for _, entry := range entries {
		if !entry.IsDir() || !hexName.MatchString(entry.Name()) {
			continue
		}
		d := filepath.Join(base, entry.Name())
		if exists(filepath.Join(d, "result.json")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if t := info.ModTime().UnixNano(); dir == "" || t > newest {
			dir, newest = d, t
		}
	}
	if dir == "" {
		return "", extension.ErrNotLive
	}
	return dir, nil
}

func tailBytes(b []byte, tail int64) []byte {
	if tail > 0 && int64(len(b)) > tail {
		return b[int64(len(b))-tail:]
	}
	return b
}

// readTail returns a file's size and at most its last tail bytes; a missing
// file is empty.
func readTail(path string, tail int64) (int64, []byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, nil
	} else if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	size, start := info.Size(), int64(0)
	if tail > 0 && size > tail {
		start = size - tail
	}
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return 0, nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, size-start))
	return start + int64(len(b)), b, err
}
