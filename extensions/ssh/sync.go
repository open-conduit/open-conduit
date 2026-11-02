package ssh

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// Entry is a path as the host copied it to the remote: its type, size,
// modification time (to the second, as tar keeps it) and permissions.
type Entry struct {
	Type  byte  `json:"type"`
	Size  int64 `json:"size,omitempty"`
	Mtime int64 `json:"mtime"`
	Mode  int64 `json:"mode"`
}

// tarFile is one file of a small archive.
type tarFile struct {
	name string
	data []byte
	mode int64
}

// tarFiles is an archive of small files.
func tarFiles(files []tarFile) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), ModTime: time.Now().Truncate(time.Second), Typeflag: tar.TypeReg})
		_, _ = tw.Write(f.data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// writeTree writes the roots to w as a tar of paths relative to /, and
// returns the manifest of what it wrote beneath the write roots. It never
// follows a symbolic link, leaves out what skip names and anything but
// directories, regular files and links (a socket, the toolbelt's), and
// never writes the toolbelt socket's path.
func writeTree(w io.Writer, roots, skip, write []string, toolbelt string) (map[string]Entry, error) {
	tw := tar.NewWriter(w)
	manifest := map[string]Entry{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if extension.Within(path, skip) || toolbelt != "" && path == toolbelt {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			h := &tar.Header{Name: strings.TrimPrefix(path, "/"), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime().Truncate(time.Second)}
			switch {
			case fi.IsDir():
				h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
			case fi.Mode().IsRegular():
				h.Typeflag, h.Size = tar.TypeReg, fi.Size()
			case fi.Mode()&fs.ModeSymlink != 0:
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				h.Typeflag, h.Linkname = tar.TypeSymlink, link
			default:
				return nil
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.CopyN(tw, f, h.Size)
				f.Close()
				if err != nil {
					return fmt.Errorf("%s changed while it was copied: %v", path, err)
				}
			}
			if extension.Within(path, write) {
				manifest[path] = Entry{Type: h.Typeflag, Size: h.Size, Mtime: h.ModTime.Unix(), Mode: h.Mode}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return manifest, tw.Close()
}

// Apply brings back what a run changed beneath the write roots: listed is
// every path beneath them on the remote, and archive holds every
// directory there and every other path the run wrote. A path is written
// only beneath a write root, never through a symbolic link and never in
// keep; a file the archive holds unchanged from the manifest is left
// alone. A path the host copied and the remote no longer has is removed,
// unless it changed on the host since. Nothing but directories, regular
// files and links is written, and no set-ID bit.
func Apply(archive io.Reader, listed map[string]bool, manifest map[string]Entry, write, keep []string) error {
	allowed := func(path string) bool {
		return filepath.IsAbs(path) && filepath.Clean(path) == path && extension.Within(path, write) && !extension.Within(path, keep)
	}
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "."), "/")
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if !allowed(name) {
			continue
		}
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := ensureDir(name, write, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if m, ok := manifest[name]; ok && m.Type == tar.TypeReg && m.Size == h.Size && m.Mtime == h.ModTime.Unix() && m.Mode == int64(mode) {
				continue
			}
			if err := safeParent(name, write); err != nil {
				return err
			}
			if err := writeFile(name, tr, mode, h.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if m, ok := manifest[name]; ok && m.Type == tar.TypeSymlink {
				if link, err := os.Readlink(name); err == nil && link == h.Linkname {
					continue
				}
			}
			if err := safeParent(name, write); err != nil {
				return err
			}
			if err := removeAny(name); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, name); err != nil {
				return err
			}
		}
	}
	// What the remote no longer has goes, deepest first, unless the host
	// changed it since.
	var gone []string
	for path := range manifest {
		if !listed[path] && allowed(path) {
			gone = append(gone, path)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(gone)))
	for _, path := range gone {
		m := manifest[path]
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if err := safeParent(path, write); err != nil {
			return err
		}
		switch {
		case m.Type == tar.TypeDir && fi.IsDir():
			_ = os.Remove(path)
		case m.Type == tar.TypeReg && fi.Mode().IsRegular() && fi.Size() == m.Size && fi.ModTime().Unix() == m.Mtime:
			if err := os.Remove(path); err != nil {
				return err
			}
		case m.Type == tar.TypeSymlink && fi.Mode()&fs.ModeSymlink != 0:
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// safeParent checks every directory from path's write root down to its
// parent is a directory, not a link to one, making any that's missing.
func safeParent(path string, write []string) error {
	for _, w := range write {
		if path == w {
			return nil
		}
	}
	return ensureDir(filepath.Dir(path), write, 0o700)
}

func ensureDir(dir string, write []string, mode fs.FileMode) error {
	var root string
	for _, w := range write {
		if extension.Within(dir, []string{w}) && len(w) > len(root) {
			root = w
		}
	}
	if root == "" {
		return fmt.Errorf("%s is outside the write grant", dir)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	cur := root
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			cur = filepath.Join(cur, parts[i])
		}
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist) && i >= 0:
			if err := os.Mkdir(cur, mode); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0 && i >= 0:
			// A link the run made where a directory now is: the
			// directory replaces it.
			if err := os.Remove(cur); err != nil {
				return err
			}
			if err := os.Mkdir(cur, mode); err != nil {
				return err
			}
		case !fi.IsDir() && i >= 0:
			if err := os.Remove(cur); err != nil {
				return err
			}
			if err := os.Mkdir(cur, mode); err != nil {
				return err
			}
		case !fi.IsDir():
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

// writeFile replaces path with the archive's file, never writing through
// what was there.
func writeFile(path string, r io.Reader, mode fs.FileMode, mtime time.Time) error {
	tmp := filepath.Join(filepath.Dir(path), ".open-conduit-"+filepath.Base(path)+".tmp")
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode&0o777)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Chmod(tmp, mode&0o777)
	}
	if err == nil {
		err = os.Chtimes(tmp, mtime, mtime)
	}
	if err == nil {
		if fi, lerr := os.Lstat(path); lerr == nil && fi.IsDir() {
			err = os.RemoveAll(path)
		}
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// removeAny removes whatever is at path, a directory with its contents.
func removeAny(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// ParseList reads a NUL-separated listing of paths relative to the
// remote's root ("./<path>") as host paths.
func ParseList(raw []byte) map[string]bool {
	out := map[string]bool{}
	for _, p := range bytes.Split(raw, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		name := strings.TrimPrefix(string(p), ".")
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		out[filepath.Clean(name)] = true
	}
	return out
}
