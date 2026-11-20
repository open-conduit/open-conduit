package observability

import (
	"testing"
	"time"
)

// An operator record is anchored by its operator and needs no scope; any
// other record without a scope, or an operator record without its
// operator, is refused.
func TestValidateOperatorRecord(t *testing.T) {
	at := time.Unix(1, 0)
	rec := Record{Name: "operator.secret_set", At: at, Correlation: Correlation{TraceID: "trace"}, Attributes: map[string]string{"operator.actor": "operator:local"}}
	if err := Validate(rec); err != nil {
		t.Fatalf("anchored operator record refused: %v", err)
	}
	unanchored := rec
	unanchored.Attributes = map[string]string{}
	if Validate(unanchored) == nil {
		t.Fatal("operator record without its operator accepted")
	}
	// A credential record is anchored by its issuer.
	auth := Record{Name: "auth.credential_refused", At: at, Correlation: Correlation{TraceID: "trace"}, Attributes: map[string]string{"auth.issuer": "corp"}}
	if err := Validate(auth); err != nil {
		t.Fatalf("anchored credential record refused: %v", err)
	}
	auth.Attributes = map[string]string{}
	if Validate(auth) == nil {
		t.Fatal("credential record without its issuer accepted")
	}
	// An audit record is anchored by its ledger sequence.
	audit := Record{Name: "audit.recorded", At: at, Correlation: Correlation{TraceID: "hash"}, Attributes: map[string]string{"audit.sequence": "1"}}
	if err := Validate(audit); err != nil {
		t.Fatalf("anchored audit record refused: %v", err)
	}
	audit.Attributes = map[string]string{"audit.action": "GET /v1/audit"}
	if Validate(audit) == nil {
		t.Fatal("audit record without its sequence accepted")
	}
	other := rec
	other.Name = "provider.timing_out"
	other.Attributes = map[string]string{"provider.name": "x", "operator.actor": "operator:local"}
	if Validate(other) == nil {
		t.Fatal("a scope-less provider record accepted")
	}
}
