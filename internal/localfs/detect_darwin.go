//go:build darwin

package localfs

import "syscall"

func detect(path string) (Info, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Info{}, err
	}
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return classifyDarwin(string(name)), nil
}
