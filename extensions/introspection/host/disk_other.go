//go:build !linux && !darwin

package host

func diskSpace(path string) (total, free uint64, id string, err error) {
	return 0, 0, "", ErrUnsupported
}
