package derive

import (
	"errors"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

// Derived is the claims one source makes, and every value it did not read
// into the atlas, each named by where it sits and why, never by what it is.
type Derived struct {
	Claims   []atlas.Claim `json:"claims"`
	Withheld []Withheld    `json:"withheld,omitempty"`
}

// Withheld is one value the atlas did not take: a setting's path or a record's
// name, and the rule it failed. A refused value is counted and placed so a
// reader can find and fix it, and is never shown.
type Withheld struct {
	Where  string `json:"where"`
	Reason string `json:"reason"`
}

// errReference is a value that is a ${VAR} reference: the atlas reads the
// settings as they are written and resolves no variable, so the value it names
// is never in reach.
var errReference = errors.New("a ${VAR} reference, which the atlas does not resolve")

// builder gathers one source's claims, withholding what its grammar refuses.
type builder struct {
	src atlas.Source
	at  time.Time
	out Derived
}

func newBuilder(src atlas.Source, at time.Time) *builder {
	return &builder{src: src, at: at, out: Derived{Claims: []atlas.Claim{}}}
}

func (b *builder) withhold(where string, err error) {
	b.out.Withheld = append(b.out.Withheld, Withheld{Where: where, Reason: err.Error()})
}

// reference reports a ${VAR} reference, withheld as one.
func (b *builder) reference(where, raw string) bool {
	if strings.Contains(raw, "${") {
		b.withhold(where, errReference)
		return true
	}
	return false
}

func (b *builder) add(where string, c atlas.Claim, err error) {
	if err != nil {
		b.withhold(where, err)
		return
	}
	b.out.Claims = append(b.out.Claims, c)
}

// ref is the entry key names, and its exists claim; false where the key is
// withheld.
func (b *builder) ref(where string, k atlas.Kind, key string) (atlas.Ref, bool) {
	if b.reference(where, key) {
		return atlas.Ref{}, false
	}
	r, err := atlas.NewRef(k, key)
	if err != nil {
		b.withhold(where, err)
		return atlas.Ref{}, false
	}
	c, err := atlas.Exists(r, b.src, b.at)
	b.add(where, c, err)
	return r, err == nil
}

// says claims r's fact f holds raw. An empty raw says nothing.
func (b *builder) says(where string, r atlas.Ref, f atlas.Fact, raw string) {
	if raw = strings.TrimSpace(raw); raw == "" || b.reference(where, raw) {
		return
	}
	c, err := atlas.Says(r, f, raw, b.src, b.at)
	b.add(where, c, err)
}

func (b *builder) relates(where string, from atlas.Ref, rel atlas.Relation, to atlas.Ref) {
	c, err := atlas.Relates(from, rel, to, b.src, b.at)
	b.add(where, c, err)
}

func (b *builder) names(where string, from atlas.Ref, rel atlas.Relation, ext atlas.External, name string) {
	if name = strings.TrimSpace(name); name == "" || b.reference(where, name) {
		return
	}
	c, err := atlas.Names(from, rel, ext, name, b.src, b.at)
	b.add(where, c, err)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
