package protocol_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
)

// A contract type that crosses to an out-of-process extension must arrive
// whole: a field its wire form leaves out reaches a package as its zero
// value, which an in-process extension never sees. Each case fills every
// data field of a contract value by reflection, sends it through its wire
// form and JSON and back, and names every field that didn't survive, so a
// field added to a contract later fails here until the wire carries it.
// Brokers and callbacks (interfaces and funcs) never cross; the receiving
// side installs them.

// notCarried are the fields the wire leaves out on purpose, each with why.
var notCarried = map[string]string{
	"domain.Scope.SecretRefs": "secret references never cross to an extension",
}

type carriedCase struct {
	name string
	in   any
	trip func(any) any
}

func carried[T, W any](from func(T) W, back func(W) T) carriedCase {
	var zero T
	return carriedCase{name: reflect.TypeOf(&zero).Elem().String(), in: zero, trip: func(v any) any {
		w := from(v.(T))
		raw, err := json.Marshal(w)
		if err != nil {
			panic(err)
		}
		var out W
		if err := json.Unmarshal(raw, &out); err != nil {
			panic(fmt.Sprintf("%s: %v", raw, err))
		}
		return back(out)
	}}
}

func TestWireCarriesEveryContractField(t *testing.T) {
	cases := []carriedCase{
		carried(protocol.FromAccepted, protocol.AcceptedEvent.Domain),
		carried(protocol.FromActionTarget, protocol.ActionTarget.Extension),
		carried(protocol.FromAuthority, protocol.Authority.Extension),
		carried(protocol.FromContainerCreated, protocol.ContainerCreated.Extension),
		carried(protocol.FromContainerPlan, protocol.ContainerPlan.Extension),
		carried(protocol.FromContainerRef, protocol.ContainerRef.Extension),
		carried(protocol.FromContainerState, protocol.ContainerState.Extension),
		carried(protocol.FromDelivery, protocol.Delivery.Extension),
		carried(protocol.FromEmitCommit, protocol.EmitCommit.Extension),
		carried(protocol.FromEmitted, protocol.Emitted.Extension),
		carried(protocol.FromEvent, protocol.Event.Domain),
		carried(protocol.FromExecution, protocol.Execution.Domain),
		carried(protocol.FromKey, protocol.Key.Domain),
		carried(protocol.FromLeaseRef, protocol.LeaseRef.Domain),
		carried(protocol.FromOutcome, protocol.Outcome.Domain),
		carried(protocol.FromOwner, protocol.ResourceOwner.Extension),
		carried(protocol.FromOwnerStatus, protocol.OwnerStatus.Extension),
		carried(protocol.FromPayload, protocol.Payload.Domain),
		carried(protocol.FromPushResult, protocol.PushResult.Extension),
		carried(protocol.FromReport, protocol.ResourceReport.Extension),
		carried(protocol.FromResource, protocol.Resource.Extension),
		carried(protocol.FromResult, protocol.Result.Extension),
		carried(protocol.FromRoute, protocol.Route.Domain),
		carried(protocol.FromRouteDecision, protocol.RouteDecision.Extension),
		carried(protocol.FromRouteInput, protocol.RouteInput.Extension),
		carried(protocol.FromRouteSample, protocol.RouteSample.Extension),
		carried(protocol.FromRunRequest, protocol.RunRequest.Extension),
		carried(protocol.FromSandboxLaunch, protocol.SandboxLaunch.Extension),
		carried(protocol.FromSandboxNeeds, protocol.SandboxNeeds.Extension),
		carried(protocol.FromSandboxPaths, protocol.SandboxPaths.Extension),
		carried(protocol.FromSandboxPlan, protocol.SandboxPlan.Extension),
		carried(protocol.FromScan, protocol.Scan.Extension),
		carried(protocol.FromScope, protocol.Scope.Domain),
		carried(protocol.FromSnapshot, protocol.Snapshot.Extension),
		carried(protocol.FromSource, protocol.Source.Extension),
		carried(protocol.FromSweepReport, protocol.ResourceSweepReport.Extension),
		carried(protocol.FromTimer, protocol.Timer.Domain),
		carried(protocol.FromWait, protocol.Wait.Domain),
		carried(protocol.FromWaitPlan, protocol.WaitPlan.Extension),
		carried(protocol.FromWork, protocol.Work.Domain),
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := reflect.New(reflect.TypeOf(c.in)).Elem()
			fill(in, 0)
			out := reflect.ValueOf(c.trip(in.Interface()))
			var lost []string
			diff(in, out, typeName(in.Type()), &lost)
			sort.Strings(lost)
			for _, f := range lost {
				t.Errorf("%s doesn't cross the wire", f)
			}
		})
	}
}

var (
	timeType    = reflect.TypeOf(time.Time{})
	rawType     = reflect.TypeOf(json.RawMessage(nil))
	durType     = reflect.TypeOf(time.Duration(0))
	payloadType = reflect.TypeOf(domain.Payload{})
	when        = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
)

func typeName(t reflect.Type) string { return t.String() }

// fill sets every data field of v to a value no field has by default.
// Strings are their field's name, so a field read into another shows up.
func fill(v reflect.Value, depth int) {
	if depth > 6 {
		return
	}
	switch {
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(when))
		return
	case v.Type() == durType:
		v.SetInt(int64(time.Second))
		return
	case v.Type() == rawType:
		v.SetBytes([]byte(`{"k":"v"}`))
		return
	case v.Type() == payloadType:
		v.Set(reflect.ValueOf(domain.Payload{Schema: "x.thing.v1", Data: []byte(`{"k":"v"}`)}))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				fv := v.Field(i)
				if fv.Kind() == reflect.String {
					fv.SetString(f.Name)
					continue
				}
				fill(fv, depth+1)
			}
		}
	case reflect.String:
		v.SetString("s")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(3)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(3)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(0.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, depth+1)
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	}
}

// diff appends the path of each data field of want that got doesn't hold.
func diff(want, got reflect.Value, path string, lost *[]string) {
	if !want.IsValid() || isZero(want) {
		return
	}
	switch want.Kind() {
	case reflect.Interface, reflect.Func, reflect.Chan:
		return
	}
	if notCarriedPath(path) {
		return
	}
	if !got.IsValid() || isZero(got) {
		*lost = append(*lost, path)
		return
	}
	switch {
	case want.Type() == timeType:
		if !want.Interface().(time.Time).Equal(got.Interface().(time.Time)) {
			*lost = append(*lost, path)
		}
		return
	case want.Type() == rawType || want.Type() == payloadType:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			*lost = append(*lost, path)
		}
		return
	}
	switch want.Kind() {
	case reflect.Struct:
		for i := range want.NumField() {
			if f := want.Type().Field(i); f.IsExported() {
				diff(want.Field(i), got.Field(i), want.Type().String()+"."+f.Name, lost)
			}
		}
	case reflect.Pointer:
		diff(want.Elem(), got.Elem(), path, lost)
	case reflect.Slice:
		if got.Len() != want.Len() {
			*lost = append(*lost, path)
			return
		}
		for i := range want.Len() {
			diff(want.Index(i), got.Index(i), path, lost)
		}
	case reflect.Map:
		for _, k := range want.MapKeys() {
			diff(want.MapIndex(k), got.MapIndex(k), path, lost)
		}
	default:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			*lost = append(*lost, path)
		}
	}
}

func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.IsNil() || v.Len() == 0
	}
	return v.IsZero()
}

func notCarriedPath(path string) bool {
	for p := range notCarried {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}
