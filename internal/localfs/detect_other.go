//go:build !linux && !darwin

package localfs

import "errors"

// detect fails on a platform Open Conduit doesn't support, so Require
// refuses rather than assumes the disk is local.
func detect(string) (Info, error) {
	return Info{}, errors.New("filesystem detection is not supported on this platform")
}
