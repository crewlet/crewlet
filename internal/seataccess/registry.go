package seataccess

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/words"
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
	if problem := catalogProblem(kind, v.Catalog()); problem != "" {
		panic(problem)
	}
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.vendors[kind]; dup {
		panic(fmt.Sprintf("seataccess: the %s vendor is registered twice", kind))
	}
	registry.vendors[kind] = v
}

// catalogProblem is why cat may not be registered for kind, or "": a catalog
// naming another kind or other lanes, a lane holding no credential, and a
// box variable or an issued field no seat could be handed.
func catalogProblem(kind integration.Kind, cat Catalog) string {
	if cat.Kind != kind {
		return fmt.Sprintf("seataccess: the %s vendor's catalog names %q", kind, cat.Kind)
	}
	want := LanesOf(kind)
	got := make([]Lane, 0, len(cat.Lanes))
	for _, spec := range cat.Lanes {
		got = append(got, spec.Lane)
	}
	if !slices.Equal(want, got) {
		return fmt.Sprintf("seataccess: the %s vendor's catalog lists lanes %s, want %s",
			kind, words.And(got), words.And(want))
	}
	for _, spec := range cat.Lanes {
		// EVERY LANE HOLDS A CREDENTIAL, sealed or issued at use, and an
		// issued one is never also sealed, which would show it twice.
		if len(cat.Credentials) == 0 && spec.Issued == "" {
			return fmt.Sprintf("seataccess: the %s vendor's %s lane names no credential field, "+
				"sealed or issued", kind, spec.Lane)
		}
		if spec.Issued != "" && (strings.TrimSpace(spec.Issued) != spec.Issued ||
			slices.Contains(cat.Credentials, spec.Issued)) {
			return fmt.Sprintf("seataccess: the %s vendor's %s lane issues %q, which is blank-padded "+
				"or one of its sealed fields %s", kind, spec.Lane, spec.Issued, words.And(cat.Credentials))
		}
		if spec.ScopeRequired && !spec.Scoped {
			return fmt.Sprintf("seataccess: the %s vendor's %s lane requires a scope it does not have",
				kind, spec.Lane)
		}
		envs := map[string]bool{}
		for _, b := range spec.BoxEnv {
			if !slices.Contains(cat.Credentials, b.Field) || strings.TrimSpace(b.Env) == "" || envs[b.Env] {
				return fmt.Sprintf("seataccess: the %s vendor's %s lane reads %q from %q: "+
					"each box variable is named once, from one of its credential fields %s",
					kind, spec.Lane, b.Field, b.Env, words.And(cat.Credentials))
			}
			envs[b.Env] = true
		}
	}
	return ""
}

// Lookup returns the vendor registered for kind.
func Lookup(kind integration.Kind) (Vendor, bool) {
	registry.RLock()
	defer registry.RUnlock()
	v, ok := registry.vendors[kind]
	return v, ok
}

// FixedLevels is a vendor whose levels are, at least in part, this engine's own
// fixed set rather than a list it reads from the vendor, so they can be named
// with no session: those levels as its [Session.Options] offers them, before
// any is marked unavailable. A vendor that also lists levels live (GitLab's
// custom roles) answers its fixed ones alone.
type FixedLevels interface {
	FixedLevels(lane Lane) []Level
}

// KnownLevels is lane's levels as kind's vendor fixes them, least access
// first, and false for a vendor that lists every level live or is not
// registered.
func KnownLevels(kind integration.Kind, lane Lane) ([]Level, bool) {
	v, ok := Lookup(kind)
	if !ok {
		return nil, false
	}
	fixed, ok := v.(FixedLevels)
	if !ok {
		return nil, false
	}
	return fixed.FixedLevels(lane), true
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
