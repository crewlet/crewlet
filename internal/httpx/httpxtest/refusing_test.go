package httpxtest

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

// A REFUSING ADDRESS REFUSES AND IS NOBODY ELSE'S until its test ends: a dial
// is refused, a listener asking for its port is refused the port, and a
// request to a gone server fails on that refusal.
//
// Mutation: close the socket before returning the address, and the listener
// takes the port.
func TestARefusingAddressRefusesAndIsNobodyElses(t *testing.T) {
	t.Parallel()
	addr := Refusing(t)
	if c, err := net.Dial("tcp", addr); err == nil {
		_ = c.Close()
		t.Fatalf("a dial of %s connected", addr)
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a dial of %s = %v, want it refused", addr, err)
	}
	if l, err := net.Listen("tcp", addr); err == nil {
		_ = l.Close()
		t.Fatalf("a listener was given %s while it was held", addr)
	}
	srv := Gone(t)
	t.Cleanup(srv.Close)
	if res, err := srv.Client().Get(srv.URL); err == nil {
		_ = res.Body.Close()
		t.Fatalf("a request to a gone server at %s was answered %s", srv.URL, res.Status)
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a request to a gone server = %v, want it refused", err)
	}
}
