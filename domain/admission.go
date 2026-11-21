package domain

// The control Events that record admission in a Work's history. The runtime
// accepts them addressed to the Work; an extension reads them through
// storage.HeldForAdmission to decide what held Work means in its domain.
const (
	// EventAdmissionHeld records Work held beyond its scope's admission limit.
	EventAdmissionHeld = "core.admission.held"
	// EventAdmissionReleased records held Work let in as a place freed.
	EventAdmissionReleased = "core.admission.released"
	// EventManualRelease records an operator releasing a Work's hold, which
	// admits Work held for admission at once.
	EventManualRelease = "core.manual.release"
	// EventCapacityHeld records Work held because its container provider
	// had no room for the launch.
	EventCapacityHeld = "core.capacity.held"
	// EventCapacityReleased records a capacity hold ending at its time, so
	// the Work's launch is tried again.
	EventCapacityReleased = "core.capacity.released"
)
