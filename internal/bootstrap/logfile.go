package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The daemon's JSON-lines logs (lifecycle, Claude Code turns, telemetry
// events) rotate by size: once a write would take the file past logMaxBytes,
// it is renamed to PATH.1 (PATH.1 to PATH.2, and so on, the oldest beyond
// logKept removed) and a fresh PATH started. Every writer hands a whole line
// to one Write, so a line is never split across files.
const (
	logMaxBytes = 10 << 20
	logKept     = 5
)

// rotatingLog is an append-only log file that rotates by size.
type rotatingLog struct {
	path     string
	maxBytes int64
	kept     int
	mu       sync.Mutex
	file     *os.File
	size     int64
}

func openLog(path string) (*rotatingLog, error) {
	return openRotatingLog(path, logMaxBytes, logKept)
}

func openRotatingLog(path string, maxBytes int64, kept int) (*rotatingLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &rotatingLog{path: path, maxBytes: maxBytes, kept: kept}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(p)) > l.maxBytes {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate shifts the kept files up one and starts a fresh one. A failure
// leaves the current file open, so logging goes on in it.
func (l *rotatingLog) rotate() error {
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.kept))
	for i := l.kept - 1; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	old := l.file
	if err := l.open(); err != nil {
		// The current file is now PATH.1: keep writing to it, and try
		// again only once it has grown by another maxBytes.
		l.file, l.size = old, 0
		return nil
	}
	return old.Close()
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
