package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"

	"github.com/crewlet/crewlet/internal/envref"
)

// Fingerprint answers one question and no other: has what this payload's
// ${VAR} references RESOLVE TO changed since the last time this process
// asked?
//
// # Why a revision comparison is not enough
//
// A config revision and the values its references resolve to are two
// different things, and the engine only ever compared the first. That gap is
// the whole of secret rotation: the documented gesture for picking up a
// rotated credential is to RE-ACTIVATE THE UNCHANGED REVISION, which means
// the payload is byte-identical, which means apply's no-op early-out fires
// and nothing rebuilds. The secret snapshot is swapped and every subsystem
// holding a resolved value keeps the old one — MCP subprocesses baked it
// into their spawn env, LLM providers hold it in a client, transports hold
// it in a header.
//
// Equal payload AND equal fingerprint is a true no-op. Equal payload with a
// changed fingerprint is a rotation, and has to rebuild.
//
// # It is keyed, per process, and cannot be printed
//
// A bare hash of a short credential is offline-brute-forceable, so a
// fingerprint in a log line or a stored row would turn a fix into a leak.
// The key is random per process, which makes a fingerprint meaningful only
// as "has this changed since the last apply IN THIS PROCESS" — which is
// exactly, and only, what it is asked.
//
// Go lets that be structural rather than a rule someone remembers: the type
// is an opaque array that compares with ==, and its String and MarshalText
// render a placeholder. Logging one, or letting it reach JSON, produces
// nothing an attacker can work with.
type Fingerprint [16]byte

// String renders a placeholder. The digest is never meant to be displayed,
// and a Stringer that returned it would put one in the first log line
// someone wrote with %v.
func (f Fingerprint) String() string { return "fingerprint(redacted)" }

// MarshalText keeps the digest out of any JSON or YAML a caller builds,
// for the same reason as String.
func (f Fingerprint) MarshalText() ([]byte, error) { return []byte("redacted"), nil }

// IsZero reports the zero fingerprint — what a caller holds before its
// first apply, and therefore never equal to a computed one in practice.
func (f Fingerprint) IsZero() bool { return f == Fingerprint{} }

// Fingerprinter computes fingerprints under one process-local key.
//
// Hold ONE per process (or per engine). Two Fingerprinters produce
// incomparable values for identical inputs, which is the point: a
// fingerprint that travelled between processes would be meaningless, and
// making that structurally impossible is cheaper than documenting it.
type Fingerprinter struct {
	key []byte
}

// NewFingerprinter mints a fingerprinter with a fresh random key.
func NewFingerprinter() *Fingerprinter {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand does not fail on any supported platform, and a
		// process that cannot read randomness cannot safely run a company
		// — it has no session ids, no incarnations and no nonces either.
		panic("config: reading randomness for the resolution fingerprint: " + err.Error())
	}
	return &Fingerprinter{key: key}
}

// Of digests what every reference in payload currently resolves to.
//
// Resolution goes through the same chain a real build uses, so a rotation
// written to the secret store registers here exactly as it will there. An
// UNSET variable is digested as the empty string, deliberately: that is
// what the config layer resolves it to and what every builder consumes, so
// distinguishing the two here would make the fingerprint disagree with what
// the engine actually sees — a difference nothing could act on.
func (f *Fingerprinter) Of(payload any, r *Resolver) Fingerprint {
	mac := hmac.New(sha256.New, f.key)
	for _, name := range ReferencedNames(payload) {
		value := r.Lookup(name)
		// Length-prefixed so ("ab", "c") and ("a", "bc") cannot digest
		// alike — otherwise a rotation that moved a character from one
		// credential to the next would read as a no-op.
		fmt.Fprintf(mac, "%d:%s=%d:%s\n", len(name), name, len(value), value)
	}
	var out Fingerprint
	copy(out[:], mac.Sum(nil))
	return out
}

// ReferencedNames is every ${VAR} a payload mentions anywhere, sorted and
// de-duplicated.
//
// It walks the value rather than a serialised form so it works on a decoded
// [Company], on the map a store row decodes to, and on a raw yaml.Node
// alike — the three shapes a payload actually arrives in.
func ReferencedNames(payload any) []string {
	seen := map[string]struct{}{}
	walkStrings(reflect.ValueOf(payload), func(s string) {
		for _, name := range envref.Names(s) {
			seen[name] = struct{}{}
		}
	})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// walkStrings visits every string reachable from v.
//
// Unexported fields are skipped: reflection cannot read them, and nothing
// in a config payload hides a reference behind one — the two types that
// have unexported state (org.Toggle, this package's Fingerprint) hold no
// strings at all.
func walkStrings(v reflect.Value, visit func(string)) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		visit(v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkStrings(v.Elem(), visit)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkStrings(v.Index(i), visit)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			// Keys too: an mcp_env server name or a skill variable is a
			// key, and while neither should carry a reference, missing one
			// that did would make the fingerprint blind to it.
			walkStrings(iter.Key(), visit)
			walkStrings(iter.Value(), visit)
		}
	case reflect.Struct:
		// A yaml.Node's own Value carries the scalar text; its Content
		// carries the children. Walking the struct generically reaches
		// both, so there is no special case to keep in sync.
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).PkgPath != "" {
				continue // unexported
			}
			walkStrings(v.Field(i), visit)
		}
	}
}
