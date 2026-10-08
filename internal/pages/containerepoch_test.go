package pages_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/pages"
)

// activation is the instant of the nth configuration activation of a case, in
// the order they were made.
func activation(n int) time.Time {
	return time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// ensure writes one container's settings from one activation and applies what
// it wrote, reporting whether it wrote anything.
func (r *roundTrip) ensure(at time.Time, key, name, purpose string) bool {
	r.t.Helper()
	_, changed, err := r.store.EnsureContainer(r.t.Context(), at, key, name, purpose)
	if err != nil {
		r.t.Fatalf("EnsureContainer %s: %v", key, err)
	}
	r.drain()
	return changed
}

// container reads one container back as a listing serves it.
func (r *roundTrip) container(key string) pages.Container {
	r.t.Helper()
	for _, c := range r.containers() {
		if c.Key == key {
			return c.Container
		}
	}
	r.t.Fatalf("container %s is not listed", key)
	return pages.Container{}
}

// A CONTAINER IS STAMPED WITH THE ACTIVATION ITS SETTINGS CAME FROM, and an
// older activation applied late writes nothing.
//
// A node that boots on a revision the fleet has since replaced used to rewrite
// every container's name and purpose back to its own old ones — the same
// walk-back the chart's projects had — because nothing on the row said which
// configuration had written it.
func TestAnOlderActivationDoesNotWalkAContainerBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	newer := activation(0).Add(300 * time.Millisecond)

	if !r.ensure(newer, "ENG", "Platform", "ships it") {
		t.Fatal("the first write wrote nothing — want a create")
	}
	if got, want := r.container("ENG").ChartEpoch, configplane.ActivationStamp(newer); got != want {
		t.Fatalf("the container is stamped %d, want the activation's own %d", got, want)
	}
	end := r.logEnd()

	// THE EARLIER ACTIVATION, arriving second, inside the same second.
	if r.ensure(activation(0), "ENG", "Engineering", "builds it") {
		t.Error("an activation 300ms older than the one applied wrote — it " +
			"walks the newer names back")
	}
	if got := r.logEnd(); got != end {
		t.Errorf("the older activation put %d record(s) on the log", got-end)
	}
	if c := r.container("ENG"); c.Name != "Platform" || c.Purpose != "ships it" {
		t.Errorf("the container reads (%q, %q), want the newer activation's", c.Name, c.Purpose)
	}

	// AND A NEWER ONE STILL WRITES, even with nothing but the epoch to say:
	// a container not re-stamped would let an activation between the two
	// walk it back.
	if !r.ensure(activation(1), "ENG", "Platform", "ships it") {
		t.Error("a later activation with the same settings wrote nothing — the " +
			"stamp would stay at the older activation")
	}
	if got, want := r.container("ENG").ChartEpoch, configplane.ActivationStamp(activation(1)); got != want {
		t.Errorf("the container is stamped %d after the later activation, want %d", got, want)
	}
}

// A REAPPLY OF ONE ACTIVATION WRITES NOTHING, AND SETS RIGHT WHAT AN EQUAL
// ACTIVATION WALKED BACK.
//
// Every boot of every node reapplies the activation it holds, so a reapply that
// wrote would be a record per boot per container. But two activations inside
// one millisecond share a stamp, and the one that lost the race can land its
// settings second — so a reapply that finds DIFFERENT settings at its own
// stamp writes them back.
func TestAReapplyWritesOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	at := activation(0)
	r.ensure(at, "ENG", "Platform", "ships it")
	end := r.logEnd()
	if r.ensure(at, "ENG", "Platform", "ships it") {
		t.Error("reapplying one activation wrote")
	}
	if got := r.logEnd(); got != end {
		t.Errorf("reapplying one activation put %d record(s) on the log", got-end)
	}

	if !r.ensure(at, "ENG", "Engineering", "builds it") {
		t.Fatal("the premise: settings that differ at an equal stamp are written")
	}
	if !r.ensure(at, "ENG", "Platform", "ships it") {
		t.Error("the reapply of the current activation did not set its settings back")
	}
	if c := r.container("ENG"); c.Name != "Platform" {
		t.Errorf("the container is named %q, want Platform", c.Name)
	}
}

// A CONTAINER WRITE MUST NAME ITS ACTIVATION. There is no honest default: the
// zero instant would stamp every container as older than any configuration.
func TestAContainerWriteRefusesToGuessItsActivation(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	end := r.logEnd()
	_, _, err := r.store.EnsureContainer(t.Context(), time.Time{}, "ENG", "Engineering", "")
	if !errors.Is(err, pages.ErrNoActivation) {
		t.Fatalf("a container write with no activation = %v, want ErrNoActivation", err)
	}
	if got := r.logEnd(); got != end {
		t.Errorf("a refused write put %d record(s) on the log", got-end)
	}
}

// A CONTAINER'S CREATION INSTANT SURVIVES ITS UPDATES.
//
// The applier wrote the document's `created_at` from each record's own instant,
// so a listing — which reads the document — reported a container as created
// whenever it was last renamed, while the row's own column kept the first.
func TestAContainerKeepsItsCreationThroughAnUpdate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.ensure(activation(0), "ENG", "Engineering", "")
	created := r.container("ENG").CreatedAt
	if created.IsZero() {
		t.Fatal("the created container has no creation instant")
	}
	// A LATER RECORD, which the broker stores at a later instant.
	time.Sleep(10 * time.Millisecond)
	r.ensure(activation(1), "ENG", "Platform", "")
	if got := r.container("ENG").CreatedAt; !got.Equal(created) {
		t.Errorf("the container reads as created at %s after an update, want %s",
			got, created)
	}
}

// TWO NODES APPLYING ONE ACTIVATION PUT ONE RECORD ON THE LOG — the second
// decided a create on rows that did not have the first's yet, lost the
// broker's arbitration, and re-decided on the rows the winner wrote. And it
// says it wrote nothing, because only its last round is what it did.
func TestTwoNodesEnsuringOneContainerWriteOnce(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	b := newRoundTripOn(t, a.log, openNodeStore(t, "node-b.db"), "node-b")

	if _, changed, err := a.store.EnsureContainer(t.Context(), activation(0),
		"ENG", "Engineering", ""); err != nil || !changed {
		t.Fatalf("node a's write = (%v, %v), want a create", changed, err)
	}
	end := a.logEnd()
	// NODE B HAS NOT APPLIED NODE A'S RECORD: its applier runs only inside
	// its own writes' waits, so its first decision is taken on rows from
	// before it, and the wait after the lost round is what applies it.
	_, changed, err := b.store.EnsureContainer(t.Context(), activation(0),
		"ENG", "Engineering", "")
	if err != nil {
		t.Fatalf("node b's write: %v — losing the arbitration to an identical "+
			"write is not a failure", err)
	}
	if changed {
		t.Error("node b reports it wrote — its first round lost the arbitration " +
			"and its second found the settings already there")
	}
	if got := a.logEnd(); got != end {
		t.Errorf("node b put %d record(s) on the log for settings node a had "+
			"already written", got-end)
	}
}

// A WRITE WHOSE OUTCOME IS UNKNOWN IS AN ERROR, never a success the caller
// logs as applied: the next apply is what decides it again, and only a caller
// told so can say that.
func TestAnUnknownContainerWriteIsAnError(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE LEDGER HAS LOST ROWS UP TO AN HOUR FROM NOW, so it can vouch for
	// no operation minted before then — which is every one this call mints.
	r.markLedgerLost(time.Now().Add(time.Hour))
	_, changed, err := r.store.EnsureContainer(t.Context(), activation(0),
		"ENG", "Engineering", "")
	if err == nil {
		t.Fatalf("an unknown outcome was reported as success (changed %v)", changed)
	}
}

// logEnd is the log's last sequence.
func (r *roundTrip) logEnd() uint64 {
	r.t.Helper()
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return end
}
