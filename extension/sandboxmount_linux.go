//go:build linux

package extension

import "os"

// mountAliases are the other paths a directory is reachable by through bind
// mounts, from /proc/self/mountinfo.
func mountAliases(path string) []string {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseMountAliases(string(raw), path)
}
