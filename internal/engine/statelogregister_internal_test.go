package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
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

// TestEveryRoleCombinationRunsEveryShippedDomain.
//
// All three shipped domains run everywhere, and that is a DECISION rather than
// an absence — an ingress node serves all three over the API, a seats node
// reads and writes all three inside a turn, and a workers node sweeps them and
// runs the embedding duty. This is what says the decision is still the one in
// the register, over every role set the parser accepts, so a fourth domain
// that narrows it has to change this case on purpose.
func TestEveryRoleCombinationRunsEveryShippedDomain(t *testing.T) {
	all := []string{string(placement.RoleIngress), string(placement.RoleSeats),
		string(placement.RoleWorkers)}
	for _, combo := range subsetsOf(all) {
		roles, err := placement.ParseRoles(combo)
		if err != nil {
			t.Fatalf("ParseRoles(%v): %v", combo, err)
		}
		part := participationOf(roles)
		if len(part.Unrun) != 0 {
			t.Errorf("roles %v decline %v — no shipped domain narrows on roles, so "+
				"this is a register entry that changed without this case",
				combo, domainNames(part.Unrun))
		}
		if len(part.Run) != len(register()) {
			t.Errorf("roles %v run %d of %d domains", combo, len(part.Run), len(register()))
		}
	}
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
