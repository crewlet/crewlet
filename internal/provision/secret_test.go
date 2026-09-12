package provision_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/provision"
)

// mintSink is a TokenSink with a settable answer for Value, which is the one
// method this rule turns on.
type mintSink struct {
	mu       sync.Mutex
	vals     map[string]string
	readErr  error
	records  int
	reads    int
	cannot   bool
	recorded map[string]string
}

func newMintSink() *mintSink {
	return &mintSink{vals: map[string]string{}, recorded: map[string]string{}}
}

func (s *mintSink) Mints() bool { return !s.cannot }

func (s *mintSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	s.vals[name], s.recorded[name] = value, value
	return nil
}

func (s *mintSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.readErr != nil {
		return "", false, s.readErr
	}
	v, ok := s.vals[name]
	return v, ok, nil
}

func (s *mintSink) Discard(context.Context) error           { return nil }
func (s *mintSink) Forget(context.Context, ...string) error { return nil }
func (s *mintSink) Flush(context.Context) error             { return nil }
func (s *mintSink) Describe() string                        { return "a test sink" }
func (s *mintSink) NextStep() string                        { return "nothing" }

func fixed(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}

// A VALUE THIS DEPLOYMENT ALREADY SEALED IS USED, NOT MINTED OVER.
//
// The resolver answers from a snapshot taken at apply time, so the variable a
// previous pass minted into resolves to nothing until something rebuilds it.
// Every pass in that window used to mint a SECOND secret, seal it over the
// first and re-register the third-party app with it — and the engine's own
// webhook route verifies with the snapshot, so each rotation moved the app
// further from the value the running process holds, on the loop's timer.
func TestASealedValueIsUsedRatherThanMintedOver(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.vals["WEBHOOK_SECRET"] = "already-sealed"

	got, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false, fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got.Value != "already-sealed" || got.Minted {
		t.Errorf("MintSecret = %+v, want the sealed value and no mint", got)
	}
	if sink.records != 0 {
		t.Errorf("the sink recorded %d value(s) over one it already held", sink.records)
	}
}

// AND A NAME NOTHING HOLDS IS MINTED INTO, which is the other half: a rule
// that never minted would leave every fresh deployment with no secret at all.
func TestANameNothingHoldsIsMintedInto(t *testing.T) {
	t.Parallel()
	sink := newMintSink()

	got, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false, fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got.Value != "fresh" || !got.Minted {
		t.Errorf("MintSecret = %+v, want a fresh value reported as minted", got)
	}
	if sink.recorded["WEBHOOK_SECRET"] != "fresh" {
		t.Errorf("the sink holds %q", sink.recorded["WEBHOOK_SECRET"])
	}
}

// A SEALED VALUE THAT IS ONLY WHITESPACE IS NOT A VALUE. It reads as held and
// signs nothing, so registering with it leaves every delivery refused by an
// app the pass has just reported ready.
func TestABlankSealedValueIsMintedOver(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.vals["WEBHOOK_SECRET"] = "   \n"

	got, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false, fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got.Value != "fresh" || !got.Minted {
		t.Errorf("MintSecret = %+v, want a fresh value", got)
	}
}

// AND A SEALED VALUE COMES BACK TRIMMED. Every caller resolves the config
// value through strings.TrimSpace, so a value carrying whitespace that is
// honoured verbatim here is registered at the third-party app as a DIFFERENT
// key from the one the engine's own webhook route verifies with — every
// delivery refused, by a pass reporting the hook converged.
func TestASealedValueComesBackTrimmed(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.vals["WEBHOOK_SECRET"] = "  already-sealed\n"

	got, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false, fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got.Value != "already-sealed" {
		t.Errorf("MintSecret value = %q, want it trimmed the way every caller "+
			"trims what the resolver answers", got.Value)
	}
}

// A STORE THAT COULD NOT SAY RAISES, and this is the clause the whole
// three-valued discipline exists for: "not held" and "I could not tell" lead
// to opposite actions, and guessing not-held mints over a live credential
// because a store blinked.
func TestAStoreThatCouldNotSayStopsTheMint(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.readErr = errors.New("the store is unreachable")

	_, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false, fixed("fresh"))
	if err == nil {
		t.Fatal("an unreadable sink was treated as one holding nothing")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Errorf("the error does not name the variable:\n%v", err)
	}
	if sink.records != 0 {
		t.Errorf("%d value(s) were minted over a store that could not say "+
			"whether one was already held", sink.records)
	}
}

// RECREATE SKIPS THE READ, because the operator asked for a fresh key having
// planned the restart that rotating one costs. Reading first would find the
// old value and honour it, which is the one case where doing nothing is wrong.
func TestRecreateMintsOverWhatIsHeld(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.vals["WEBHOOK_SECRET"] = "already-sealed"

	got, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", true, fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got.Value != "fresh" || !got.Minted {
		t.Errorf("MintSecret = %+v, want the rotation the operator asked for", got)
	}
	if sink.reads != 0 {
		t.Errorf("the sink was read %d time(s) on a run told to recreate", sink.reads)
	}
}

// A NODE WITH NO KEYRING REPORTS, IT DOES NOT FAULT. An error here is a fault
// the loop retries while telling an operator the engine is working on it —
// and no pass will ever succeed until somebody sets secrets.keys.
func TestANodeWithNoKeyringReportsRatherThanFaulting(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.cannot = true

	got, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false, fixed("fresh"))
	if err != nil {
		t.Fatalf("a node with no keyring faulted: %v", err)
	}
	if !got.NoKeyring || got.Value != "" || got.Minted {
		t.Errorf("MintSecret = %+v, want the keyring posture and no value", got)
	}
	if sink.records != 0 || sink.reads != 0 {
		t.Errorf("a sink that cannot seal was asked anyway: %d read(s), %d record(s)",
			sink.reads, sink.records)
	}
}

// AND NO SINK AT ALL IS STILL A REFUSAL, which is the command line's case: a
// run told to mint with nowhere to put the result would leave a live signing
// secret at the third-party app and print none of it.
func TestNoSinkIsRefused(t *testing.T) {
	t.Parallel()
	_, err := provision.MintSecret(
		context.Background(), nil, "WEBHOOK_SECRET", false, fixed("fresh"))
	if !errors.Is(err, provision.ErrNoSink) {
		t.Fatalf("MintSecret with no sink = %v, want ErrNoSink", err)
	}
}

// A MINT THAT FAILS SEALS NOTHING. The shape is the vendor's — gitlab's host
// requires a whsec_ value over exactly 32 bytes — so producing one can fail,
// and a half-written variable would be a value the engine resolves and no app
// signs with.
func TestAFailedMintSealsNothing(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	want := errors.New("no entropy")

	_, err := provision.MintSecret(
		context.Background(), sink, "WEBHOOK_SECRET", false,
		func() (string, error) { return "", want })
	if !errors.Is(err, want) {
		t.Fatalf("MintSecret = %v, want the mint's own error", err)
	}
	if sink.records != 0 {
		t.Errorf("%d value(s) were sealed by a run that minted none", sink.records)
	}
}
