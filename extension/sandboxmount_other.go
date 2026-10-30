//go:build !linux

package extension

// mountAliases: only Linux has bind mounts a process can make unseen.
func mountAliases(string) []string { return nil }
