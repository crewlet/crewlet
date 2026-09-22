package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
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
	// has stopped being available.
	for name, want := range map[string]bool{
		tracker.Domain{}.Name(): true,
		pages.Domain{}.Name():   true,
		search.Domain{}.Name():  false,
	} {
		if got, held := stated[name]; !held || got != want {
			t.Errorf("%s: barrier declared %v, want %v (held %v)", name, got, want, held)
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

func (unregisteredDomain) Tables() map[string]statelog.TableClass { return nil }

func (unregisteredDomain) DeferredTable() string { return "" }

func (unregisteredDomain) ScopeIndex() string { return "" }

func (unregisteredDomain) OpsTable() string { return "" }

func (unregisteredDomain) ReadinessInput() bool { return false }

func (unregisteredDomain) ClaimsIdentity() bool { return false }

// TestEveryRegisteredDomainSaysWhichNodesRunIt, because a nil predicate is
// read as running NOWHERE and nothing else in the boot would notice: every
// node would strip that domain's rows out of an artefact it adopted and apply
// none of its records, on a company whose board simply stayed empty.
func TestEveryRegisteredDomainSaysWhichNodesRunIt(t *testing.T) {
	for _, entry := range register() {
		if entry.Participates == nil {
			t.Errorf("%s declares no participation", entry.Domain.Name())
		}
	}

	// THE CONTROL: an entry that omits it fails the boot check, and the
	// refusal names the domain rather than the field.
	entries := register()
	entries[0].Participates = nil
	err := checkRegister(entries)
	if err == nil {
		t.Fatal("an entry that says nothing about which nodes run it passed the " +
			"boot check")
	}
	if !strings.Contains(err.Error(), entries[0].Domain.Name()) {
		t.Errorf("the refusal does not name the domain: %v", err)
	}
}

// TestEveryRoleCombinationRunsTheDomainsItsEntriesName.
//
// FOUR OF THE FIVE RUN EVERYWHERE, and that is a DECISION rather than an
// absence: an ingress node serves the tracker, the vectors, the knowledge base
// and the chart over the API, a seats node reads and writes all four inside a
// turn, and a workers node sweeps them and runs the embedding duty.
//
// THE FIFTH NARROWS, which is what this case is now two-sided about. The
// identity estate runs on ingress and workers and NOT on a seats-only
// satellite: no turn reads it, so such a node would pay for the disk, the
// applier and a share of the stream budget to hold a directory of people it
// authenticates nobody against.
//
// Both directions, because each failure is silent in its own way. A domain
// that quietly stopped narrowing puts personal data on every satellite in the
// fleet. One that quietly started narrowing leaves a node applying none of its
// records while reporting itself healthy — which nothing above it can tell
// from a node that is merely behind.
func TestEveryRoleCombinationRunsTheDomainsItsEntriesName(t *testing.T) {
	// The domains that narrow, and where each of them runs. Spelled out
	// rather than derived from the predicate, because deriving it would be
	// asserting the predicate against itself.
	narrowing := map[string][]placement.NodeRole{
		iamdomain.Domain{}.Name(): {placement.RoleIngress, placement.RoleWorkers},
	}
	all := []string{string(placement.RoleIngress), string(placement.RoleSeats),
		string(placement.RoleWorkers)}
	for _, combo := range subsetsOf(all) {
		roles, err := placement.ParseRoles(combo)
		if err != nil {
			t.Fatalf("ParseRoles(%v): %v", combo, err)
		}
		part := participationOf(roles)
		run := map[string]bool{}
		for _, entry := range part.Run {
			run[entry.Domain.Name()] = true
		}
		for _, entry := range register() {
			name := entry.Domain.Name()
			want := true
			if where, narrows := narrowing[name]; narrows {
				want = false
				for _, role := range where {
					if roles.Has(role) {
						want = true
					}
				}
			}
			if run[name] != want {
				t.Errorf("roles %v %s %s, want the opposite — a domain that "+
					"quietly stopped narrowing puts its rows on every node, "+
					"and one that quietly started leaves a node applying none "+
					"of its records while reporting itself healthy",
					combo, ranOrNot(run[name]), name)
			}
		}
		if len(part.Run)+len(part.Unrun) != len(register()) {
			t.Errorf("roles %v run %d and decline %d of %d domains", combo,
				len(part.Run), len(part.Unrun), len(register()))
		}
	}
}

// ranOrNot renders a participation for the message above.
func ranOrNot(ran bool) string {
	if ran {
		return "run"
	}
	return "decline"
}

// TestADeclinedDomainIsUnrunAndNeverBoth, which is the property every reader
// of the split rests on: the applier set and the strip set partition the
// register, so no domain is applied and stripped, and none is neither.
func TestADeclinedDomainIsUnrunAndNeverBoth(t *testing.T) {
	roles, err := placement.ParseRoles([]string{string(placement.RoleSeats)})
	if err != nil {
		t.Fatal(err)
	}
	part := participationOf(roles)
	seen := map[string]int{}
	for _, entry := range part.Run {
		seen[entry.Domain.Name()]++
	}
	for _, domain := range part.Unrun {
		seen[domain.Name()]++
	}
	for _, domain := range registeredDomains() {
		if seen[domain.Name()] != 1 {
			t.Errorf("%s appears %d times across the run and unrun sets, want once "+
				"— a domain in both is stripped out of the artefact its own "+
				"applier is about to read", domain.Name(), seen[domain.Name()])
		}
		if part.Runs(domain.Name()) == slices.ContainsFunc(part.Unrun,
			func(d statelog.Domain) bool { return d.Name() == domain.Name() }) {

			t.Errorf("Runs(%q) and the unrun set disagree", domain.Name())
		}
	}
}

// subsetsOf is every non-empty combination of the roles the parser accepts.
func subsetsOf(all []string) [][]string {
	var out [][]string
	for mask := 1; mask < 1<<len(all); mask++ {
		var combo []string
		for i, role := range all {
			if mask&(1<<i) != 0 {
				combo = append(combo, role)
			}
		}
		out = append(out, combo)
	}
	return out
}

// A PEER THAT DECLARES NO ROLES RUNS EVERY DOMAIN.
//
// A presence row with no roles on it is what a build predating the field
// writes, and it is exactly what a rolling upgrade puts in front of the new
// nodes. [placement.RoleSet] settles that "declared nothing" and "does
// nothing" must never be the same answer, and a domain's own predicate is
// where that would be forgotten: a peer read as running nothing is a peer left
// out of that domain's counted set, so the fleet trims past a node that is
// still applying.
//
// IT IS EXERCISED THROUGH A NARROWING PREDICATE, because every shipped domain
// runs everywhere and a case over the real register could not tell the
// resolution from the answer.
func TestAPeerThatDeclaresNoRolesRunsEveryDomain(t *testing.T) {
	onlyIngress := register()
	onlyIngress[0].Participates = func(r placement.RoleSet) bool {
		_, holds := r[placement.RoleIngress]
		return holds
	}
	narrowed := onlyIngress[0].Domain.Name()

	for name, roles := range map[string]placement.RoleSet{
		"a nil set":            nil,
		"an empty non-nil set": {},
	} {
		t.Run(name, func(t *testing.T) {
			if got := participationIn(onlyIngress, roles); !got.Runs(narrowed) {
				t.Errorf("a peer declaring no roles declines %q — it would be left "+
					"out of that domain's counted set, and the fleet would trim "+
					"past a node that is still applying", narrowed)
			}
		})
	}

	// THE CONTROL: the predicate really does narrow, so the assertions
	// above are the resolution working rather than a predicate that says
	// yes to everything.
	seatsOnly, err := placement.ParseRoles([]string{string(placement.RoleSeats)})
	if err != nil {
		t.Fatal(err)
	}
	if participationIn(onlyIngress, seatsOnly).Runs(narrowed) {
		t.Fatalf("the fixture predicate admits %q on a seats-only node, so the "+
			"cases above prove nothing", narrowed)
	}
}

// EVERY REGISTERED DOMAIN'S SEAMS ARE COMPLETE, and the boot check is where a
// gap has to be found.
//
// [statelog.NewPublisher] refuses a nil Rows, Fence or Gates BY NAME, so a
// half-built write authority is a boot failure with an explanation rather than
// a node that appends records the fleet drops one at a time. But it only
// refuses at the moment a publisher is built, and `NewSeams` is a closure —
// so a domain whose constructor returned two of three would boot every other
// domain first and fail on one line deep inside the fifth.
//
// This is that check, run over the register directly. A domain that appends
// AND declares an eviction reader needs all four, and the entries that leave
// Evicted nil say why at the field.
func TestEveryRegisteredDomainsSeamsAreComplete(t *testing.T) {
	for _, entry := range register() {
		name := entry.Domain.Name()
		if entry.NewSeams == nil {
			t.Errorf("%s declares no write authority at all", name)
			continue
		}
		// A NIL STATELOG AND A NIL RUNNER ARE WHAT THIS CAN PASS, and
		// what it therefore checks is the SHAPE: a constructor that
		// reaches for a store handle answers an error, which is itself
		// the signal that the seams are built from the engine rather
		// than from constants. Either outcome is legitimate; a
		// constructor that returned a PARTIAL set is not.
		seams, err := entry.NewSeams(&stateLog{nodeID: "boot-check"}, nil)
		if err != nil {
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
					"on one line inside the fifth constructor rather than here",
					name, field)
			}
		}
	}
}
