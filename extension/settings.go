package extension

import "encoding/json"

// Setting is one config field a type declares safe to show an operator, with
// its effective value (defaults applied): a string, bool, number or list of
// strings. The operator's view of the config (GET /v1/config) shows only
// declared settings; every other field is shown by name alone, as set.
type Setting struct {
	Name  string
	Value any
}

// SettingsDescriber is optionally implemented by an executor registration:
// Settings lists the binding config fields it is safe to show, such as a
// model. It returns nil for a config it would reject. It must never return a
// command, argument list, URL, header, environment value or anything else
// that can carry a credential.
type SettingsDescriber interface {
	Settings(config json.RawMessage) []Setting
}
