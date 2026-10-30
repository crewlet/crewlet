package seataccess

import (
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/integration"
)

// The vendors this build manages per-seat access through, registered by each
// vendor package's init.
//
// A REGISTRY rather than a list in the engine, so adding a vendor is a change
// to that vendor's package alone: the setup pass and the access routes reach
// every vendor through [Lookup], and a test holds every kind in [Kinds] to a
// registered vendor, so a kind left unregistered fails the build's suite
// rather than silently managing nobody.
var registry = struct {
	sync.RWMutex
	vendors map[integration.Kind]Vendor
}{vendors: map[integration.Kind]Vendor{}}

// Register adds a vendor. Called from the vendor package's init.
//
// It PANICS on a kind outside [Kinds], on a catalog whose kind or lanes do not
// match, and on a second registration of one kind: each is a programming
// error that would otherwise surface as one vendor's seats being managed by
// another's code, or by none.
func Register(v Vendor) {
	kind := v.Kind()
	if !slices.Contains(Kinds(), kind) {
		panic(fmt.Sprintf("seataccess: %q is not a kind that manages per-seat access", kind))
	}
	cat := v.Catalog()
	if cat.Kind != kind {
		panic(fmt.Sprintf("seataccess: the %s vendor's catalog names %q", kind, cat.Kind))
	}
	want := LanesOf(kind)
	got := make([]Lane, 0, len(cat.Lanes))
	for _, spec := range cat.Lanes {
		got = append(got, spec.Lane)
	}
	if !slices.Equal(want, got) {
		panic(fmt.Sprintf("seataccess: the %s vendor's catalog lists lanes %v, want %v", kind, got, want))
	}
	if len(cat.Credentials) == 0 {
		panic(fmt.Sprintf("seataccess: the %s vendor's catalog names no credential field", kind))
	}
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.vendors[kind]; dup {
		panic(fmt.Sprintf("seataccess: the %s vendor is registered twice", kind))
	}
	registry.vendors[kind] = v
}

// Lookup returns the vendor registered for kind.
func Lookup(kind integration.Kind) (Vendor, bool) {
	registry.RLock()
	defer registry.RUnlock()
	v, ok := registry.vendors[kind]
	return v, ok
}

// Registered is every registered vendor, in [Kinds] order.
func Registered() []Vendor {
	registry.RLock()
	defer registry.RUnlock()
	var out []Vendor
	for _, kind := range Kinds() {
		if v, ok := registry.vendors[kind]; ok {
			out = append(out, v)
		}
	}
	return out
}
