//go:build !unix

package claudecode

// refileSession moves nothing where descriptor-relative moves aren't
// available: the turn goes on from the handoff note.
func refileSession(string, []string, string, string) (bool, error) {
	return false, nil
}
