package jetstream

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// A LOG CANNOT REMOVE AN INTERIOR SEQUENCE, and this is what keeps that a
// TESTED INVARIANT rather than a comment.
//
// The strict replay protocol treats contiguity as an invariant and a hole as a
// fault — which is only sound because six settings on the stream make an
// interior removal impossible. Each of them fails silently if it drifts: an
// age bound or a per-subject limit turns the log into a keyed table and the
// strict loop stalls on the first ordinary write; a delete or a rollup erases
// a committed record from under every node's applier; a discard-old policy
// drops the oldest record to accept a new one, which is the company's own
// history going quietly missing under load.
func TestMutationStreamCannotRemoveInteriorSequences(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, DomainStream{
		Name:       "CREWLET_STRICT_LOG",
		Subjects:   []string{"crewlet.strict.log.>"},
		MaxBytes:   16 << 20,
		Duplicates: 2 * time.Minute,
	})
	s, err := q.js.Stream(t.Context(), "CREWLET_STRICT_LOG")
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	// THE LIVE CONFIGURATION, not the spec that was sent: what matters is
	// what the broker is running, and a field the server ignored or
	// defaulted differently is exactly the drift this case is for.
	info, err := s.Info(t.Context())
	if err != nil {
		t.Fatalf("read the stream info: %v", err)
	}
	cfg := info.Config

	if cfg.MaxMsgsPerSubject > 0 {
		t.Errorf("the log keeps %d message(s) per subject — a per-subject limit "+
			"removes an INTERIOR sequence wherever it sits, and the strict loop "+
			"stalls on the first one for ever", cfg.MaxMsgsPerSubject)
	}
	if cfg.MaxAge != 0 {
		t.Errorf("the log bounds its age at %s — an age bound deletes records a "+
			"node has not applied, and there is no node whose word it takes",
			cfg.MaxAge)
	}
	if !cfg.DenyDelete {
		t.Error("the log permits message deletion — a key-value bucket gets " +
			"DenyDelete for free and a plain stream does not, so without it any " +
			"client holding the API can erase a committed record from under " +
			"every node's applier")
	}
	if cfg.AllowRollup {
		t.Error("the log permits a rollup — a rollup is a delete of everything " +
			"below it, spelled as a publish")
	}
	if cfg.Discard != jetstream.DiscardNew {
		t.Errorf("the log discards %v — a full log must REFUSE the append "+
			"loudly, because a dropped record is nobody's problem until the day "+
			"somebody reads the gap", cfg.Discard)
	}
	if cfg.AllowDirect || cfg.MirrorDirect {
		t.Errorf("the log allows direct gets (direct=%v mirror=%v) — the "+
			"framework asks the broker for a subject's last message to decide "+
			"whether a write raced, and a direct get is served from replica "+
			"state that may be behind an acknowledged write, so the "+
			"discriminator would answer \"no message here\" for a message the "+
			"quorum already has", cfg.AllowDirect, cfg.MirrorDirect)
	}
	if cfg.Retention != jetstream.LimitsPolicy {
		t.Errorf("the log retains under %v — an interest policy removes a "+
			"message once every filtered consumer has acked it, so one "+
			"permanently dead node pins the whole log, and it DROPS a publish "+
			"no consumer's filter covers", cfg.Retention)
	}
}

// AND NOTHING TRIMS PER SUBJECT.
//
// A keep-newest purge would remove the trimmed-anchor hazard outright, and it
// is rejected on cost: the option is mutually exclusive with a
// purge-by-sequence, so a keep-one trim must be issued PER SUBJECT — a hundred
// thousand API calls per trim tick against one stream-wide call — and it would
// destroy the contiguity invariant the strict protocol rests on.
//
// A static walk rather than a comment, because the option is exactly what
// somebody reaches for on meeting the hazard for the first time.
func TestNothingPurgesPerSubject(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{".", filepath.Join("..", "..", "statelog")} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "WithPurgeSubject", "WithPurgeKeep":
						t.Errorf("%s calls %s — a keep-newest purge is mutually "+
							"exclusive with purging by sequence, so it must be "+
							"issued per subject, and it removes the interior "+
							"sequences the strict replay protocol treats as an "+
							"invariant", relative(path), sel.Sel.Name)
					}
					return true
				})
			}
		}
	}
}

// relative renders a path the way a person reading the failure would type it.
func relative(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, path); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return path
}

// A DOMAIN LOG ANSWERS AN EMPTY SUBJECT AS A FACT, NOT AS A FAILURE.
//
// It is what tells a trimmed anchor from a lost race, and the two have
// opposite remedies: one publishes at an expectation of zero and the other
// must never. A caller that had to re-derive that distinction from an error
// would take the trimmed-anchor branch on a broker that was merely
// unreachable.
func TestAnEmptySubjectIsAFactRatherThanAnError(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}

	seq, found, err := log.LastSeq(t.Context(), "crewlet.probe.log.object.never-written")
	if err != nil {
		t.Fatalf("LastSeq on an empty subject = %v, want no error — \"no "+
			"message here\" is the answer rather than a failure", err)
	}
	if found || seq != 0 {
		t.Fatalf("LastSeq on an empty subject = (%d, %v), want (0, false)", seq, found)
	}

	// AND AN OCCUPIED ONE ANSWERS ITS SEQUENCE.
	at, dup, err := log.Append(t.Context(), "crewlet.probe.log.object.a", "op-1", nil, []byte("x"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if dup {
		t.Error("a first append was reported as a duplicate")
	}
	seq, found, err = log.LastSeq(t.Context(), "crewlet.probe.log.object.a")
	if err != nil || !found || seq != at {
		t.Fatalf("LastSeq after an append = (%d, %v, %v), want (%d, true, nil)",
			seq, found, err, at)
	}
}

// A CONDITIONAL APPEND IS ARBITRATED BY THE BROKER, and a nil expectation is
// no expectation at all.
//
// The pointer is what makes those two states distinguishable: zero is a real
// expectation meaning "this subject holds nothing", and it is the one branch
// where being wrong costs a silent lost update rather than a refused write.
func TestAnExpectationIsArbitratedAndNilIsNoExpectation(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	subject := "crewlet.probe.log.object.a"

	zero := uint64(0)
	first, _, err := log.Append(t.Context(), subject, "op-1", &zero, []byte("one"))
	if err != nil {
		t.Fatalf("an expectation of zero on an empty subject: %v", err)
	}
	// The same expectation now names a subject that holds something.
	if _, _, err := log.Append(t.Context(), subject, "op-2", &zero, []byte("two")); err == nil {
		t.Fatal("an expectation of zero was accepted on an occupied subject — " +
			"the broker is the only party that sees every writer, and this is " +
			"the whole concurrency control")
	}
	// The current one is accepted.
	if _, _, err := log.Append(t.Context(), subject, "op-3", &first, []byte("three")); err != nil {
		t.Fatalf("the current expectation was refused: %v", err)
	}
	// AND NO EXPECTATION IS NOT AN EXPECTATION OF ZERO.
	if _, _, err := log.Append(t.Context(), subject, "op-4", nil, []byte("four")); err != nil {
		t.Fatalf("an append with no expectation was refused: %v — a nil "+
			"expectation must send no header at all, which is what the additive "+
			"pattern is", err)
	}
}
