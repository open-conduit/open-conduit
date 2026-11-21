package storage

import "github.com/open-conduit/open-conduit/domain"

// HeldForAdmission reports whether Work is held beyond its scope's admission
// limit, from the control Events in its history: the latest of a hold, a
// release as a place freed and an operator's release decides. It reads only
// Events, so it answers the same in process and over the protocol, where an
// extension cannot read the runtime's own records. Admission holds Work only
// before its first attempt, so a caller that has seen an Execution need not
// ask. Work cancelled while held still answers true: it was never admitted.
func HeldForAdmission(tx Tx, work domain.ID) (bool, error) {
	events, err := tx.WorkEvents(work, 0, 0)
	if err != nil {
		return false, err
	}
	held := false
	for _, a := range events {
		switch a.Event.Type {
		case domain.EventAdmissionHeld:
			held = true
		case domain.EventAdmissionReleased, domain.EventManualRelease:
			held = false
		}
	}
	return held, nil
}
