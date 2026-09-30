package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The register is the one place a domain is declared, so these are the tests
// that make "one place" true: every entry is complete, every decision it holds
// is stated rather than inferred from an omission, and the order every
// operator surface reports in is the order written down here.
//
// Each carries the control that makes it fail, because a completeness walk
// that cannot go red is a claim rather than a check.

// TestEveryRegisteredDomainHasEveryConstructor is the walk itself: the
// register this build ships passes its own boot check.
func TestEveryRegisteredDomainHasEveryConstructor(t *testing.T) {
	if err := checkRegister(register()); err != nil {
		t.Fatalf("the register this build ships does not pass its own boot check: %v", err)
	}
	for _, entry := range register() {
		name := entry.Domain.Name()
		if entry.NewApplier == nil || entry.NewSeams == nil || entry.Ceiling == nil {
			t.Errorf("%s: an entry reached the walk with a nil constructor", name)
		}
	}
}

// TestANilApplierFailsTheBootCheck is that walk's control: the entry a domain
// would land with before its applier exists is refused, naming the domain, at
// the check rather than at the moment some switch happened to be consulted.
func TestANilApplierFailsTheBootCheck(t *testing.T) {
	entries := register()
	entries[0].NewApplier = nil
	err := checkRegister(entries)
	if err == nil {
		t.Fatal("a register entry with no applier passed the boot check, so a node would " +
			"consume that domain's records and produce no rows")
	}
	if !strings.Contains(err.Error(), entries[0].Domain.Name()) {
		t.Errorf("the refusal does not name the domain: %v", err)
	}
}

// TestACeilingWithNoFloorFailsTheBootCheck is the same control for the floor a
// ceiling's field accepts: without it, a boot the broker refuses cannot say how
// small a ceiling to set, and read as zero it would offer a smaller one for
// every log, the org chart's at its floor included.
func TestACeilingWithNoFloorFailsTheBootCheck(t *testing.T) {
	entries := register()
	last := len(entries) - 1
	declared := entries[last].Ceiling
	entries[last].Ceiling = func(stream config.Stream, free int64) domainCeiling {
		ceiling := declared(stream, free)
		ceiling.Floor = 0
		return ceiling
	}
	err := checkRegister(entries)
	if err == nil {
		t.Fatal("a register entry whose ceiling declares no floor passed the boot check")
	}
	if !strings.Contains(err.Error(), entries[last].Domain.Name()) {
		t.Errorf("the refusal does not name the domain: %v", err)
	}
}

// TestNoWriteAuthorityFailsTheBootCheck is the same control for the seams.
func TestNoWriteAuthorityFailsTheBootCheck(t *testing.T) {
	entries := register()
	entries[1].NewSeams = nil
	err := checkRegister(entries)
	if err == nil {
		t.Fatal("a register entry with no write authority passed the boot check")
	}
	if !strings.Contains(err.Error(), "append") {
		t.Errorf("the refusal does not say what the missing seam costs: %v", err)
	}
}

// TestABarrierDecisionIsExplicit is the reason the table exists.
//
// A domain that grants no linearizable read is a legitimate declaration — the
// vectors are one — and used to be indistinguishable from a domain whose
// author never thought about it, because both are "absent from the switch".
// Stating neither is now a boot failure, and so is stating both.
func TestABarrierDecisionIsExplicit(t *testing.T) {
	stated := map[string]bool{}
	for _, entry := range register() {
		if (entry.Barrier != nil) == entry.NoBarrier {
			t.Fatalf("%s states %s about its barrier", entry.Domain.Name(), statedBarrier(entry))
		}
		stated[entry.Domain.Name()] = entry.Barrier != nil
	}
	// The shipped answers, so that a domain silently losing its read index
	// is a failure here rather than a strongest read level nobody notices
	// has stopped being available — for EVERY registered domain, since a
	// table naming three of five pinned nothing about the org chart's or
	// the identity estate's, and docs/guides/consistency.md publishes all
	// five.
	shipped := map[string]bool{
		tracker.Domain{}.Name():   true,
		pages.Domain{}.Name():     true,
		chart.Domain{}.Name():     true,
		iamdomain.Domain{}.Name(): true,
		search.Domain{}.Name():    false,
	}
	for name, want := range shipped {
		if got, held := stated[name]; !held || got != want {
			t.Errorf("%s: barrier declared %v, want %v (held %v)", name, got, want, held)
		}
	}
	for name := range stated {
		if _, pinned := shipped[name]; !pinned {
			t.Errorf("%s is registered and its barrier decision is pinned nowhere "+
				"— say here, and in docs/guides/consistency.md, whether it grants "+
				"linearizable", name)
		}
	}

	// CONTROL, both ways round: neither is refused, and so is both.
	neither := register()
	neither[0].Barrier, neither[0].NoBarrier = nil, false
	if err := checkRegister(neither); err == nil {
		t.Error("an entry that states neither a barrier nor NoBarrier passed the boot check, " +
			"so a domain could lose its linearizable read with nothing to read off the source")
	}
	both := register()
	both[1].Barrier = tracker.EncodeBarrier
	if err := checkRegister(both); err == nil {
		t.Error("an entry that states both a barrier and NoBarrier passed the boot check")
	}
}

// A REGISTERED DOMAIN WITH NO GENERATION RECORD FAILS THE BOOT CHECK.
//
// A reanchor opens a generation with the domain's own record, and the engine
// once refused every log but the tracker's for want of one — so the org chart's
// and the identity estate's logs, which a restored broker can leave behind as
// readily as the tracker's, could never be re-anchored, and nobody learned it
// until an incident asked. A domain that keeps none says so through its own
// encoder, as the vectors do.
func TestADomainWithNoGenerationRecordFailsTheBootCheck(t *testing.T) {
	for _, entry := range register() {
		if entry.Generation == nil {
			t.Errorf("%s declares no generation record, so its log could never "+
				"be re-anchored", entry.Domain.Name())
		}
	}
	entries := register()
	last := len(entries) - 1
	entries[last].Generation = nil
	err := checkRegister(entries)
	if err == nil {
		t.Fatal("a register entry with no generation record passed the boot check")
	}
	if !strings.Contains(err.Error(), entries[last].Domain.Name()) ||
		!strings.Contains(err.Error(), "re-anchored") {
		t.Errorf("the refusal does not name the domain and what it costs: %v", err)
	}
}

// EVERY IDENTITY-CLAIMING LOG CAN BE WRITTEN BY THE NODE GATE, AND NO OTHER.
//
// The trim counts nodes on a log that claims identity, so a log the gate could
// not write kept an evicted node counted there for ever — which is what the
// tracker-only gesture did to the pages log. The org chart and the identity
// estate joined the register after the gate did, and a gate that walked a
// switch of its own reached neither.
func TestEveryIdentityLogHasANodeGateWriter(t *testing.T) {
	claims := 0
	for _, entry := range register() {
		identity := entry.Domain.ClaimsIdentity()
		switch {
		case identity && entry.NewGate == nil:
			t.Errorf("%s claims identity and declares no node-gate writer", entry.Domain.Name())
		case !identity && entry.NewGate != nil:
			t.Errorf("%s claims no identity and declares a node-gate writer", entry.Domain.Name())
		}
		if identity {
			claims++
		}
	}
	if claims < 4 {
		t.Fatalf("the register holds %d identity-claiming domain(s), want the "+
			"tracker, the pages, the org chart and the identity estate", claims)
	}

	// CONTROL, both ways round.
	missing := register()
	for i := range missing {
		if missing[i].Domain.Name() == (iamdomain.Domain{}).Name() {
			missing[i].NewGate = nil
		}
	}
	if err := checkRegister(missing); err == nil ||
		!strings.Contains(err.Error(), iamdomain.Domain{}.Name()) {
		t.Errorf("an identity log with no gate writer answered %v, want a refusal naming it", err)
	}
	extra := register()
	for i := range extra {
		if extra[i].Domain.Name() == (search.Domain{}).Name() {
			extra[i].NewGate = register()[0].NewGate
		}
	}
	if err := checkRegister(extra); err == nil ||
		!strings.Contains(err.Error(), search.Domain{}.Name()) {
		t.Errorf("a gate writer on a log that counts nobody answered %v, want a refusal", err)
	}
}

// A WAKE FEED THAT DISAGREES WITH ITS DOMAIN'S DECLARATION FAILS THE BOOT CHECK
// — on every node and before anything is provisioned, where it used to be
// asked only as a feed started, which a node that publishes nothing never
// does.
//
// The trim waits on the group a domain DECLARES, and the wakes advance the one
// its feed RUNS AS; each way of making the two disagree is a trim that silently
// waits for ever or silently waits for nothing. Every arm is a register this
// build could ship if the feed were a switch beside the declaration, which is
// what it was.
func TestAFeedThatDisagreesWithItsDeclarationFailsTheBootCheck(t *testing.T) {
	trackerFeed := func() func(*Engine, domainFeed) (changefeed.Translator, changefeed.Opener) {
		for _, entry := range register() {
			if entry.Domain.Name() == (tracker.Domain{}).Name() {
				return entry.Feed
			}
		}
		t.Fatal("the register holds no tracker entry")
		return nil
	}()
	for _, c := range []struct {
		name   string
		domain string
		edit   func(*registration)
		names  string
	}{
		{"a declared feed nobody runs", pages.Domain{}.Name(),
			func(r *registration) { r.Feed = nil }, "runs none"},
		{"a feed nobody declared", search.Domain{}.Name(),
			func(r *registration) { r.Feed = trackerFeed }, "declares no wake feed"},
		{"a feed under another domain's group", pages.Domain{}.Name(),
			func(r *registration) { r.Feed = trackerFeed }, "its feed runs as"},
	} {
		t.Run(c.name, func(t *testing.T) {
			entries := register()
			for i := range entries {
				if entries[i].Domain.Name() == c.domain {
					c.edit(&entries[i])
				}
			}
			err := checkRegister(entries)
			if err == nil || !strings.Contains(err.Error(), c.names) ||
				!strings.Contains(err.Error(), c.domain) {
				t.Fatalf("%s answered %v, want a refusal naming %s and saying %q",
					c.name, err, c.domain, c.names)
			}
		})
	}
}

// AN IDENTITY-CLAIMING DOMAIN THAT CANNOT SAY WHO IS EVICTED ON IT FAILS THE
// BOOT CHECK — before anything is provisioned, where it used to be asked only
// once the logs were up.
func TestAnIdentityDomainThatCannotSayWhoIsEvictedFailsTheBootCheck(t *testing.T) {
	for _, c := range []struct {
		name   string
		domain statelog.Domain
		names  string
	}{
		{"no eviction listing", identityWithoutListing{}, "list the evictions"},
		{"no log probe", identityWithoutProbe{}, "without applying it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			entries := register()
			entries[0].Domain = c.domain
			err := checkRegister(entries)
			if err == nil || !strings.Contains(err.Error(), c.names) {
				t.Fatalf("an identity domain with %s answered %v, want a refusal "+
					"saying so (%q)", c.name, err, c.names)
			}
		})
	}
}

// identityWithoutListing is the tracker's declaration with no eviction
// listing: the method set of a domain that embeds nothing but the probe.
type identityWithoutListing struct{ unregisteredDomain }

func (identityWithoutListing) Name() string         { return "listless" }
func (identityWithoutListing) ClaimsIdentity() bool { return true }
func (identityWithoutListing) EvictionSubject(node string) statelog.Subject {
	return tracker.Domain{}.EvictionSubject(node)
}
func (identityWithoutListing) Evicts(payload []byte) (bool, error) {
	return tracker.Domain{}.Evicts(payload)
}

// identityWithoutProbe lists its evictions and cannot be asked of its log.
type identityWithoutProbe struct{ unregisteredDomain }

func (identityWithoutProbe) Name() string         { return "probeless" }
func (identityWithoutProbe) ClaimsIdentity() bool { return true }
func (identityWithoutProbe) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	return tracker.Domain{}.Evictions(ctx, db)
}

// TestEveryRegisteredDomainHasACeilingCase walks the ceiling the way the boot
// sizes it, through the same seam, so a domain added to the register with no
// Tier A ceiling cannot reach the broker.
func TestEveryRegisteredDomainHasACeilingCase(t *testing.T) {
	const free = 64 << 30
	for _, domain := range registeredDomains() {
		ceiling, err := tierACeiling(config.Stream{}, domain, free)
		if err != nil {
			t.Errorf("%s: %v", domain.Name(), err)
			continue
		}
		if ceiling.Bytes <= 0 {
			t.Errorf("%s: sized at %d bytes", domain.Name(), ceiling.Bytes)
		}
		if ceiling.Field == "" {
			t.Errorf("%s: names no Tier A field, so a refusal to reserve it could not "+
				"say what to change", domain.Name())
		}
	}

	// CONTROL: a domain that is not in the register has no ceiling case,
	// and the refusal names it.
	_, err := tierACeiling(config.Stream{}, unregisteredDomain{}, free)
	if err == nil {
		t.Fatal("a domain with no register entry was given a ceiling")
	}
	if !strings.Contains(err.Error(), "unregistered") {
		t.Errorf("the refusal does not name the domain: %v", err)
	}
}

// TestTheRegisteredOrderIsTheDeclaredOrder pins the order, which is
// load-bearing: it is the order streams are provisioned in, the order an
// offer's terms are compared in and the order every operator surface reports.
func TestTheRegisteredOrderIsTheDeclaredOrder(t *testing.T) {
	want := []string{
		tracker.Domain{}.Name(),
		search.Domain{}.Name(),
		pages.Domain{}.Name(),
		chart.Domain{}.Name(),
		iamdomain.Domain{}.Name(),
	}
	got := []string{}
	for _, domain := range registeredDomains() {
		got = append(got, domain.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("the register holds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the register holds %v, want %v", got, want)
		}
	}
}

// TestADomainDeclaredTwiceFailsTheBootCheck: which applier, write authority
// and ceiling such a node ran would depend on which entry a lookup reached.
func TestADomainDeclaredTwiceFailsTheBootCheck(t *testing.T) {
	entries := register()
	entries = append(entries, entries[0])
	if err := checkRegister(entries); err == nil {
		t.Fatal("a register declaring one domain twice passed the boot check")
	}
}

// TestRegistrationForAnUnknownDomainIsNotFound: the lookup every switch now
// goes through answers honestly for a name the register does not hold, which
// is what turns each of those switches' default arms into one answer.
func TestRegistrationForAnUnknownDomainIsNotFound(t *testing.T) {
	if _, found := registrationFor("no-such-domain"); found {
		t.Fatal("the register answered for a domain it does not hold")
	}
	for _, domain := range registeredDomains() {
		if _, found := registrationFor(domain.Name()); !found {
			t.Errorf("the register does not answer for %s, which it holds", domain.Name())
		}
	}
}

// unregisteredDomain is a domain no register entry names, for the controls.
type unregisteredDomain struct{}

func (unregisteredDomain) Name() string { return "unregistered" }

func (unregisteredDomain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{Name: "CREWLET_UNREGISTERED", Subjects: []string{"crewlet.unregistered.>"}}
}

func (unregisteredDomain) RecordVersion() int { return 1 }

func (unregisteredDomain) Envelope([]byte) (statelog.Envelope, error) {
	return statelog.Envelope{}, nil
}

func (unregisteredDomain) InstallsGate(statelog.Envelope) bool { return false }

func (unregisteredDomain) NodeGate(statelog.Envelope) bool { return false }

func (unregisteredDomain) FeedGroup() string { return "" }

func (unregisteredDomain) Tables() map[string]statelog.TableClass { return nil }

func (unregisteredDomain) DeferredTable() string { return "" }

func (unregisteredDomain) ScopeIndex() string { return "" }

func (unregisteredDomain) OpsTable() string { return "" }

func (unregisteredDomain) ReadinessInput() bool { return false }

func (unregisteredDomain) ClaimsIdentity() bool { return false }

// EVERY REGISTERED DOMAIN'S WRITE AUTHORITY BUILDS, over a real store.
//
// [statelog.NewPublisher] refuses a nil Rows, Fence or Gates BY NAME, so a
// half-built write authority is a boot failure with an explanation rather than
// a node that appends records the fleet drops one at a time. But `NewSeams` is
// a closure the boot reaches one domain at a time, so a gap in the fifth entry
// is found only after four domains have started.
//
// So every entry is built HERE the way [stateLog.start] builds it: over a
// migrated store, beside a real runner, through [stateLog.publisherFrom].
// A case with no store could not tell a constructor that needs one from a
// broken one, and an error it skipped past certified nothing — which is what
// this case did while it handed every constructor a nil database. And each
// fence is ASKED, because one built over the wrong handle constructs fine and
// fails on the first append.
//
// A STRICT domain needs its eviction reader too: readiness asks it whether
// this node was evicted, and one without it serves reads from rows the fleet
// has abandoned. Only a compacted log may leave it nil — its values are derived
// and a re-embed replaces anything an evicted node held.
func TestEveryRegisteredDomainsSeamsAreComplete(t *testing.T) {
	t.Parallel()
	s := seamsStateLog(t)
	for _, entry := range register() {
		name := entry.Domain.Name()
		runner := seamsRunner(t, s, entry)
		seams, err := entry.NewSeams(s, nil, runner)
		if err != nil {
			t.Errorf("%s's write authority could not be built over a migrated "+
				"store: %v", name, err)
			continue
		}
		for field, absent := range map[string]bool{
			"Rows":  seams.Rows == nil,
			"Fence": seams.Fence == nil,
			"Gates": seams.Gates == nil,
		} {
			if absent {
				t.Errorf("%s's write authority has no %s — statelog.NewPublisher "+
					"refuses that by name, so this domain would fail the boot "+
					"after every domain before it had started", name, field)
			}
		}
		if seams.Fence != nil {
			if evicted, err := seams.Fence.Evicted(t.Context()); err != nil || evicted {
				t.Errorf("%s's fence over a fresh store answers evicted=%v (%v), "+
					"want a definite no", name, evicted, err)
			}
		}
		strict := entry.Domain.Stream().Replay == statelog.ReplayStrict
		if strict && seams.Evicted == nil {
			t.Errorf("%s is a strict log and declares no eviction reader, so "+
				"readiness never asks whether this node was evicted and its "+
				"reads go on serving rows the fleet has abandoned", name)
		}
		if _, _, err := s.publisherFrom(entry, noLog, nil, runner,
			seamsReserve(t, entry)); err != nil {
			t.Errorf("%s's write authority is refused at boot: %v", name, err)
		}
	}
}

// A REGISTER ENTRY SHORT OF A SEAM FAILS ITS DOMAIN'S START, BEFORE ANY APPEND.
//
// The register's own entries are complete, so they cannot reach this arm; an
// entry whose constructor drops one seam is handed to the same path a boot
// takes. The log it is given refuses every append, so a publisher that got far
// enough to append would fail rather than pass — the refusal has to come first.
func TestAnEntryShortOfASeamFailsItsDomainsStart(t *testing.T) {
	t.Parallel()
	s := seamsStateLog(t)
	entry, ok := registrationFor(tracker.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no tracker domain")
	}
	for _, c := range []struct {
		field string
		drop  func(*writeSeams)
		names string
	}{
		{"Rows", func(w *writeSeams) { w.Rows = nil }, "no rows"},
		{"Fence", func(w *writeSeams) { w.Fence = nil }, "no fence"},
		{"Gates", func(w *writeSeams) { w.Gates = nil }, "no gates"},
	} {
		short := entry
		build := entry.NewSeams
		short.NewSeams = func(s *stateLog, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			seams, err := build(s, appendTo, runner)
			c.drop(&seams)
			return seams, err
		}
		_, _, err := s.publisherFrom(short, noLog, nil, seamsRunner(t, s, short),
			seamsReserve(t, short))
		if err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("an entry whose seams have no %s starts as %v, want a refusal "+
				"naming it (%q)", c.field, err, c.names)
		}
	}
	// THE CONTROL: the same entry, unmodified, starts — so the refusals
	// above are about the dropped seam and not about the rig.
	if _, _, err := s.publisherFrom(entry, noLog, nil, seamsRunner(t, s, entry),
		seamsReserve(t, entry)); err != nil {
		t.Fatalf("the unmodified tracker entry is refused on this rig: %v", err)
	}
}

// seamsStateLog is a state log over a migrated store, holding what a domain's
// constructors read: this node's id, the store and a keyring to sign under.
func seamsStateLog(t *testing.T) *stateLog {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: len(register())})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &stateLog{nodeID: "boot-check", db: db,
		ring: statelog.OneKey("k1", "boot-check-material")}
}

// seamsRunner is the runner [stateLog.start] builds beside a domain's write
// authority, over the same estate, with a consumer that delivers nothing.
func seamsRunner(t *testing.T, s *stateLog, entry registration) *statelog.Runner {
	t.Helper()
	applier, err := entry.NewApplier(s)
	if err != nil {
		t.Fatalf("build %s's applier: %v", entry.Domain.Name(), err)
	}
	verifier, err := s.verifierFor(entry.Domain)
	if err != nil {
		t.Fatalf("build %s's verifier: %v", entry.Domain.Name(), err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: entry.Domain, Applier: applier, Fetch: nothingToFetch{},
		Log: nothingOnLog{}, Node: s.db,
		DB: replicatedEstate{node: s.db}, Verifier: verifier,
		Checkpoint: statelog.Position{Generation: 1},
	})
	if err != nil {
		t.Fatalf("build %s's runner: %v", entry.Domain.Name(), err)
	}
	return runner
}

// seamsReserve is the gate reserve [stateLog.start] builds beside a domain's
// write authority — nil for a log that keeps none — over a log it never reads:
// the reserve asks the log for its usage only at an append, which these cases
// never make.
func seamsReserve(t *testing.T, entry registration) *statelog.Reserve {
	t.Helper()
	reserve, err := reserveFor(entry.Domain, nil)
	if err != nil {
		t.Fatalf("build %s's gate reserve: %v", entry.Domain.Name(), err)
	}
	return reserve
}

// noLog is the appender these cases hand a write authority: one no case
// appends through, so a publisher that got far enough to append would fail
// the case rather than pass it.
var noLog statelog.Appender = refusingAppender{}

type refusingAppender struct{}

func (refusingAppender) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return 0, false, errors.New("this case appends nothing")
}

func (refusingAppender) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, errors.New("this case reads nothing")
}

// nothingOnLog is the log read by position, holding nothing: the runner is
// built, and never run.
type nothingOnLog struct{}

func (nothingOnLog) At(context.Context, uint64) (string, []byte, time.Time, bool, error) {
	return "", nil, time.Time{}, false, nil
}

func (nothingOnLog) Bounds(context.Context) (uint64, uint64, error) { return 0, 0, nil }

// nothingToFetch is a consumer with nothing waiting: the runner is built, and
// never run.
type nothingToFetch struct{}

func (nothingToFetch) Fetch(context.Context, int, int, time.Duration) ([]statelog.Message, error) {
	return nil, nil
}

func (nothingToFetch) Pending(context.Context) (uint64, error) { return 0, nil }

// THE IDENTITY LEDGER'S SESSION ROWS GO IN AN HOUR, and only a SHORTER kind
// horizon is a declaration the boot accepts.
//
// The shipped answer first: the identity entry declares its session subjects'
// own horizon, and it is an hour — the design's number, a resolve budget plus
// a margin — with every other domain keeping one horizon for the whole ledger.
// Then the controls: a kind horizon at or past the domain's would never be what
// sweeps a row, and one of zero sweeps a row before its writer could re-ask.
func TestTheIdentityLedgersSessionRowsGoInAnHour(t *testing.T) {
	for _, entry := range register() {
		name := entry.Domain.Name()
		if name != (iamdomain.Domain{}).Name() {
			if len(entry.OpsKindRetention) != 0 {
				t.Errorf("%s declares kind horizons %v, and no writer of it is "+
					"known to spare its op ids", name, entry.OpsKindRetention)
			}
			continue
		}
		got := entry.OpsKindRetention[string(iamdomain.KindSession)]
		if got != time.Hour {
			t.Errorf("the identity ledger keeps session rows for %s, want %s",
				got, time.Hour)
		}
	}
	for _, horizon := range []time.Duration{0, statelog.OpsRetention,
		2 * statelog.OpsRetention} {
		entries := register()
		for i := range entries {
			if entries[i].Domain.Name() == (iamdomain.Domain{}).Name() {
				entries[i].OpsKindRetention = map[string]time.Duration{
					string(iamdomain.KindSession): horizon,
				}
			}
		}
		if err := checkRegister(entries); err == nil {
			t.Errorf("a %s session horizon beside the domain's %s passed the "+
				"boot check", horizon, statelog.OpsRetention)
		}
	}
}
