package sdk

import (
	"reflect"

	"github.com/open-conduit/open-conduit/extension"
)

// SchemaOf returns the JSON Schema of t's encoding/json form, as an
// operation's input declares it (extension.SchemaOf).
func SchemaOf(t reflect.Type) map[string]any { return extension.SchemaOf(t) }
