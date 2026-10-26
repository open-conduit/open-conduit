package bootstrap

import (
	"net"
	"strings"
	"testing"
)

func TestBindErrorExplainsPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, err = net.Listen("tcp", addr)
	if err == nil {
		t.Fatal("second bind succeeded")
	}
	msg := bindError(addr, err).Error()
	for _, want := range []string{"cannot bind " + addr, "already in use", "--webhook-addr", "another free local port"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}
