package collect_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
)

// sizedFleet is the memory coordination store, which holds each atlas record
// to its class's bound as the KV store does, recording what was written and
// counting the reads of a source's misses.
type sizedFleet struct {
	*memory.Fleet
	mu       sync.Mutex
	standing map[string][]byte
	misses   map[string][]byte
	missRead int
}

func newSizedFleet() *sizedFleet {
	return &sizedFleet{Fleet: memory.NewFleet(), standing: map[string][]byte{}, misses: map[string][]byte{}}
}

func (f *sizedFleet) PutAtlasSourceStatus(ctx context.Context, kind string, value []byte) error {
	f.mu.Lock()
	f.standing[kind] = value
	f.mu.Unlock()
	return f.Fleet.PutAtlasSourceStatus(ctx, kind, value)
}

func (f *sizedFleet) PutAtlasSourceMisses(ctx context.Context, kind string, value []byte) error {
	f.mu.Lock()
	f.misses[kind] = value
	f.mu.Unlock()
	return f.Fleet.PutAtlasSourceMisses(ctx, kind, value)
}

func (f *sizedFleet) AtlasSourceMisses(ctx context.Context, kind string) ([]byte, bool, error) {
	f.mu.Lock()
	f.missRead++
	f.mu.Unlock()
	return f.Fleet.AtlasSourceMisses(ctx, kind)
}

func (f *sizedFleet) missReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.missRead
}

// widestStanding is a standing at its widest: every scope a company may
// declare, under an id at its longest, every refused part a standing keeps,
// each of its texts at its bound written as text, an error at its bound, and
// every count at an int64's 19 digits and instant to the nanosecond.
func widestStanding(text func(n int) string) collect.Status {
	at := time.Date(2026, 11, 23, 12, 34, 56, 123456789, time.UTC)
	most := math.MaxInt64
	st := collect.Status{Kind: integration.KindGoogleAnalytics, LastAttemptAt: at, LastCompleteAt: at,
		Completed: true, Digest: strings.Repeat("f", 64), Entries: most, Calls: most, Withheld: most,
		Recorded: most, Retired: most, Purged: most, LastError: text(collect.MaxErrorLength),
		UnreadOverflow: most, Missed: most}
	for i := range collect.MaxUnread {
		st.Unread = append(st.Unread, collect.Finding{Part: fmt.Sprintf("%02d", i) + text(collect.MaxFindingLength-2),
			Scope: text(collect.MaxFindingLength), Detail: text(collect.MaxFindingLength)})
	}
	for i := range collect.MaxScopes {
		id := fmt.Sprintf("%03d", i) + strings.Repeat("h", 37)
		st.Scopes = append(st.Scopes, collect.Scope{ID: id, LastReadAt: at, Unread: most})
	}
	return st
}

// THE WIDEST STANDING ITS WRITER CAN MAKE OF PLAIN TEXT FITS ITS BOUND
// UNCUT: every scope, refused part, text and count at its bound is written
// as it is, within coord.MaxAtlasSourceStandingBytes, with room to spare.
//
// Mutation: raise MaxScopes back to 2048, and the record is cut or refused.
func TestTheWidestAtlasStandingFitsItsBound(t *testing.T) {
	t.Parallel()
	if len(strings.Repeat("h", 37))+3 != 40 || !config.SSHHostID(fmt.Sprintf("%03d", 199)+strings.Repeat("h", 37)) {
		t.Fatal("premise: a scope's id here is not a host's at its longest")
	}
	fleet := newSizedFleet()
	store, err := collect.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	want := widestStanding(func(n int) string { return strings.Repeat("p", n) })
	if err := store.Save(t.Context(), want); err != nil {
		t.Fatalf("save the widest standing: %v", err)
	}
	// THE FIGURE coord.MaxAtlasSourceStandingBytes STATES.
	if value := fleet.standing[want.Kind.String()]; len(value) != 53_043 {
		t.Errorf("the widest plain standing is %d bytes, want the 53,043 its bound states", len(value))
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	back := got[want.Kind]
	if back.LastError != want.LastError || len(back.Scopes) != collect.MaxScopes ||
		len(back.Unread) != collect.MaxUnread || back.Unread[0] != want.Unread[0] {
		t.Errorf("the widest plain standing was cut: %d scopes, %d parts, error of %d bytes", len(back.Scopes),
			len(back.Unread), len(back.LastError))
	}
}

// A STANDING PAST ITS BOUND IS CUT, NEVER REFUSED, EACH PART'S DETAIL FIRST,
// THEN THE ERROR, THEN THE PARTS, and every count and scope is kept: text a
// vendor wrote that JSON escapes six bytes a byte is what grows it.
//
// Mutation: halve the error before the details, and the first case cuts the
// error it did not need to; encode with no cut, and the store refuses both.
func TestAStandingPastItsBoundIsCutDetailFirstKeepingEveryCount(t *testing.T) {
	t.Parallel()
	plain := func(n int) string { return strings.Repeat("p", n) }
	escaped := func(n int) string { return strings.Repeat("<", n) }
	for _, c := range []struct {
		name string
		st   collect.Status
		// whole is whether the error and the parts survive whole.
		whole bool
	}{
		{"escaped details alone", func() collect.Status {
			st := widestStanding(plain)
			for i := range st.Unread {
				st.Unread[i].Detail = escaped(collect.MaxFindingLength)
			}
			return st
		}(), true},
		{"every text escaped", widestStanding(escaped), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fleet := newSizedFleet()
			store, err := collect.NewCoordStore(fleet)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), c.st); err != nil {
				t.Fatalf("a standing past its bound was refused: %v", err)
			}
			value := fleet.standing[c.st.Kind.String()]
			if len(value) > coord.MaxAtlasSourceStandingBytes {
				t.Fatalf("the cut standing is %d bytes, past its %d", len(value), coord.MaxAtlasSourceStandingBytes)
			}
			all, err := store.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got := all[c.st.Kind]
			if len(got.Scopes) != collect.MaxScopes || got.Scopes[collect.MaxScopes-1] != c.st.Scopes[collect.MaxScopes-1] ||
				got.Missed != c.st.Missed || got.Entries != c.st.Entries || got.UnreadOverflow != c.st.UnreadOverflow ||
				len(got.Unread) != collect.MaxUnread {
				t.Errorf("the cut lost a count or a scope: %d scopes, %d parts", len(got.Scopes), len(got.Unread))
			}
			if d := got.Unread[0].Detail; d == c.st.Unread[0].Detail || !strings.HasSuffix(d, "…") {
				t.Errorf("a part's detail is %d bytes, want it cut and marked", len(d))
			}
			kept := got.LastError == c.st.LastError && got.Unread[0].Part == c.st.Unread[0].Part
			if kept != c.whole {
				t.Errorf("the error and the parts are kept whole: %t, want %t", kept, c.whole)
			}
		})
	}
}

// THE WIDEST MISSES FIT THEIR BOUND: MaxMissing entry ids, each first missed
// by a read of its own at an instant to the nanosecond, the overflow at an
// int64's 19 digits, are recorded within coord.MaxAtlasSourceMissesBytes.
//
// Mutation: raise MaxMissing to 9,000, and the store refuses them.
func TestTheWidestAtlasMissesFitTheirBound(t *testing.T) {
	t.Parallel()
	fleet := newSizedFleet()
	store, err := collect.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	// EVERY INSTANT KEEPS ITS NINE DIGITS: one nanosecond in each ten.
	at := time.Date(2026, 11, 23, 12, 34, 56, 111111111, time.UTC)
	st := collect.Status{Kind: integration.KindGoogleAnalytics, MissingOverflow: math.MaxInt64}
	for i := range collect.MaxMissing {
		st.Missing = append(st.Missing, collect.Missing{Since: at.Add(time.Duration(10 * i)),
			Entries: []string{uuid.NewString()}})
	}
	if err := store.SaveMisses(t.Context(), st); err != nil {
		t.Fatalf("record the widest misses: %v", err)
	}
	// THE FIGURE coord.MaxAtlasSourceMissesBytes STATES.
	if value := fleet.misses[st.Kind.String()]; len(value) != 770_092 {
		t.Errorf("the widest misses are %d bytes, want the 770,092 their bound states", len(value))
	}
	back, err := store.Misses(t.Context(), collect.Status{Kind: st.Kind})
	if err != nil || len(back.Missing) != collect.MaxMissing || back.MissingOverflow != math.MaxInt64 {
		t.Errorf("the widest misses read back as %d groups, %d held back (%v)", len(back.Missing),
			back.MissingOverflow, err)
	}
}

// ONLY A VISIT READS ITS SOURCE'S MISSES, ONCE, BY ITS KEY: the tick lists
// the standings alone, so a tick with nothing due reads no misses at all,
// and a source's visit reads its own before it decides.
//
// Mutation: have the tick read each standing's misses, and the quiet tick
// reads them.
func TestOnlyAVisitReadsItsSourcesMissesByItsKey(t *testing.T) {
	t.Parallel()
	fleet := newSizedFleet()
	store, err := collect.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	src.set(world(t, t0, originA), nil)
	company := &config.Company{Name: "acme"}
	company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", APIToken: "${CF_TOKEN}"}}
	clock := t0
	w, err := collect.New(collect.Options{Sources: []collect.Source{src}, Store: store, Log: newFakeLog(),
		Input:    func(context.Context) (collect.Input, bool) { return collect.Input{Config: company}, true },
		Interval: func() time.Duration { return time.Hour }, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	w.Tick(t.Context())
	if n := fleet.missReads(); src.reads != 1 || n != 1 {
		t.Fatalf("the first visit read the source %d times and its misses %d, want once each", src.reads, n)
	}
	clock = clock.Add(30 * time.Minute)
	w.Tick(t.Context())
	if n := fleet.missReads(); src.reads != 1 || n != 1 {
		t.Errorf("a tick with nothing due read the source %d times and the misses %d, want no read of either",
			src.reads-1, n-1)
	}
	if _, recorded := fleet.misses["cloudflare"]; !recorded {
		t.Error("the visit recorded no misses beside its standing")
	}
}

// A SCOPE WHOSE ID IS NO HOST'S IS WITHHELD AND NEVER KEPT: a standing's
// scopes have a widest form only while each id is a host's.
//
// Mutation: let Builder.Scope keep every id, and the standing keeps it.
func TestAScopeWhoseIDIsNoHostsIsWithheld(t *testing.T) {
	t.Parallel()
	b := collect.NewBuilder(integration.KindSSH, t0)
	b.Scope("web-1")
	b.Scope(strings.Repeat("<", 41))
	read := b.Read()
	if len(read.Scopes) != 1 || read.Scopes[0] != "web-1" || len(read.Withheld) != 1 ||
		strings.Contains(read.Withheld[0].Where, "<") {
		t.Fatalf("the read kept scopes %v and withheld %+v, want web-1 alone and the other counted", read.Scopes,
			read.Withheld)
	}
	st := collect.Decide(nil, collect.Read{Scopes: []string{"web-1", "Web 2"}}, collect.Status{Kind: integration.KindSSH}, t0).Status
	if len(st.Scopes) != 1 || st.Scopes[0].ID != "web-1" {
		t.Errorf("the standing keeps the scopes %+v, want web-1's alone", st.Scopes)
	}
}
