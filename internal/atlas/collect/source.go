package collect

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Source is one integration's read of what the company runs on at its vendor.
// Registered from the vendor package's init ([Register]), as the seat access
// vendors are, so the engine learns the sources from the packages it links.
type Source interface {
	// Kind is the integration the source reads.
	Kind() integration.Kind

	// Collect reads the vendor whole, under the access the company's own
	// credential holds, and reports everything it saw as claims in the
	// integration's voice. It is never told what changed, and it writes
	// nothing anywhere: the duty records what differs from the log. It
	// answers [integration.ErrNotConfigured] where the company does not
	// connect the integration, and an error for a read that did not finish,
	// which records and retires nothing.
	Collect(ctx context.Context, in Input) (Read, error)
}

// Input is what one read is handed.
type Input struct {
	// Config is the live company document, whose credentials resolve through
	// Env. A source reads its own block's credential from it and nothing
	// else into a claim: every value it reports goes through [Builder].
	Config *config.Company
	Env    *config.Resolver

	// Now is the instant the read is made at, which every claim is seen at.
	Now time.Time

	// HTTP overrides the vendor client's transport, for tests; nil is the
	// vendor package's own.
	HTTP *http.Client
}

// Read is what one source saw: every claim, in the integration's voice, the
// values it withheld, and what the read cost.
type Read struct {
	// Claims is every claim the read holds, at most one per entry and slot.
	Claims []atlas.Claim

	// Withheld is every value the read did not take: where it sat and the
	// rule it failed, never what it was.
	Withheld []derive.Withheld

	// Calls is how many requests the read made at the vendor.
	Calls int

	// Unread is every part of the vendor the credential may not read. The
	// read is still whole: each part's earlier claims stand as they were.
	Unread []Unread
}

// Unread is one part of a vendor a read was refused: a listing the company's
// credential lacks the permission for. It is a finding on the source's
// standing, never a fault of the read.
type Unread struct {
	// Part names the listing, and Detail says what to grant so it is read.
	// Both are the source's own words, never the vendor's answer.
	Part   string
	Detail string

	// Holds reports whether a claim the voice already holds came from this
	// part. Such a claim is carried as it stands: a refusal withdraws and
	// retires nothing, since it says nothing about what the vendor holds.
	Holds func(atlas.Claim) bool
}

// registry is every registered source, by kind.
var registry = struct {
	sync.RWMutex
	sources map[integration.Kind]Source
}{sources: map[integration.Kind]Source{}}

// Register adds a vendor's source. A kind registered twice is a programming
// error, and panics as the seat access registry does: two sources for one
// vendor would record the same vendor in one voice twice.
func Register(s Source) {
	kind := s.Kind()
	if !kind.Valid() {
		panic(fmt.Sprintf("collect: %q is not an integration kind", kind))
	}
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.sources[kind]; dup {
		panic(fmt.Sprintf("collect: the %s source is registered twice", kind))
	}
	registry.sources[kind] = s
}

// Lookup is the source registered for kind, and false for a vendor nothing
// reads for the atlas.
func Lookup(kind integration.Kind) (Source, bool) {
	registry.RLock()
	defer registry.RUnlock()
	s, ok := registry.sources[kind]
	return s, ok
}

// Registered is every registered source, in the order of their kinds.
func Registered() []Source {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]Source, 0, len(registry.sources))
	for _, s := range registry.sources {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Source) int { return cmp.Compare(a.Kind(), b.Kind()) })
	return out
}
