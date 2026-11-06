package storage_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/storage"
)

// delegated is a Tx whose every method panics, so a ReadOnly method that
// reaches the inner transaction is told apart from one it refuses.
type delegated struct{ storage.Tx }

// ReadOnly refuses every write with ErrReadOnly and hands every read to the
// inner transaction. A method is a write by its name, so a write added to Tx
// without a refusal in ReadOnly fails here.
func TestReadOnlyRefusesEveryWrite(t *testing.T) {
	writes := []string{"Accept", "Mark", "Put", "Delete", "Claim", "Renew", "Release", "Fire", "Set"}
	tx := reflect.ValueOf(storage.ReadOnly(delegated{}))
	typ := reflect.TypeFor[storage.Tx]()
	refused := 0
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		write := false
		for _, w := range writes {
			write = write || strings.HasPrefix(m.Name, w)
		}
		args := make([]reflect.Value, m.Type.NumIn())
		for j := range args {
			args[j] = reflect.Zero(m.Type.In(j))
		}
		var out []reflect.Value
		reached := func() (reached bool) {
			defer func() { reached = recover() != nil }()
			out = tx.MethodByName(m.Name).Call(args)
			return false
		}()
		switch {
		case write && reached:
			t.Errorf("%s writes through ReadOnly", m.Name)
		case write:
			if err, _ := out[len(out)-1].Interface().(error); !errors.Is(err, storage.ErrReadOnly) {
				t.Errorf("%s: got %v, want ErrReadOnly", m.Name, err)
			}
			refused++
		case !reached:
			t.Errorf("%s is a read, but ReadOnly does not hand it on", m.Name)
		}
	}
	if refused != 15 {
		t.Errorf("refused %d writes, want 15", refused)
	}
}
