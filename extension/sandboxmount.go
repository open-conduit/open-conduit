package extension

import (
	"path/filepath"
	"strings"
)

// parseMountAliases reads mountinfo (proc(5)): for the mount path is on, the
// path's place within that filesystem, then every other mount of the same
// filesystem whose root holds it. Each such mount point is an alias.
func parseMountAliases(mountinfo, path string) []string {
	type mount struct{ dev, root, point string }
	var mounts []mount
	for _, line := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mounts = append(mounts, mount{dev: f[2], root: unescapeMount(f[3]), point: unescapeMount(f[4])})
	}
	// The mount path is on: the longest mount point it is within.
	var on *mount
	for i := range mounts {
		m := &mounts[i]
		if Within(path, []string{m.point}) && (on == nil || len(m.point) >= len(on.point)) {
			on = m
		}
	}
	if on == nil {
		return nil
	}
	rel, err := filepath.Rel(on.point, path)
	if err != nil {
		return nil
	}
	inFS := filepath.Join(on.root, rel)
	var out []string
	for _, m := range mounts {
		if m.dev != on.dev || m.point == on.point {
			continue
		}
		if r, err := filepath.Rel(m.root, inFS); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
			out = append(out, filepath.Join(m.point, r))
		}
	}
	return out
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, ok := octal(s[i+1 : i+4]); ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octal(s string) (byte, bool) {
	if len(s) != 3 {
		return 0, false
	}
	var v int
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v*8 + int(c-'0')
	}
	return byte(v), v < 256
}
