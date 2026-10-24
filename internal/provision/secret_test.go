package provision_test

import (
	"context"
	"errors"
	"fmt"
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
	recorded map[string]string
}

func newMintSink() *mintSink {
	return &mintSink{vals: map[string]string{}, recorded: map[string]string{}}
}

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
		context.Background(), sink, "WEBHOOK_SECRET", false, nil, fixed("fresh"))
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
		context.Background(), sink, "WEBHOOK_SECRET", false, nil, fixed("fresh"))
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
		context.Background(), sink, "WEBHOOK_SECRET", false, nil, fixed("fresh"))
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
		context.Background(), sink, "WEBHOOK_SECRET", false, nil, fixed("fresh"))
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
		context.Background(), sink, "WEBHOOK_SECRET", false, nil, fixed("fresh"))
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
		context.Background(), sink, "WEBHOOK_SECRET", true, nil, fixed("fresh"))
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

// NO SINK AT ALL IS A REFUSAL, which is the command line's case: a
// run told to mint with nowhere to put the result would leave a live signing
// secret at the third-party app and print none of it.
func TestNoSinkIsRefused(t *testing.T) {
	t.Parallel()
	_, err := provision.MintSecret(
		context.Background(), nil, "WEBHOOK_SECRET", false, nil, fixed("fresh"))
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
		context.Background(), sink, "WEBHOOK_SECRET", false, nil,
		func() (string, error) { return "", want })
	if !errors.Is(err, want) {
		t.Fatalf("MintSecret = %v, want the mint's own error", err)
	}
	if sink.records != 0 {
		t.Errorf("%d value(s) were sealed by a run that minted none", sink.records)
	}
}

// unknownTo is an Unvouched that cannot vouch for the names given, as a run
// reading only a stopped node's own rows cannot for a variable they lack.
func unknownTo(names ...string) provision.Unvouched {
	return func(name string) error {
		for _, n := range names {
			if n == name {
				return fmt.Errorf("%w: %s is out of this run's sight",
					provision.ErrUnvouched, name)
			}
		}
		return nil
	}
}

// AN EMPTY ANSWER THE RUN CANNOT VOUCH FOR IS NOT A REASON TO MINT.
//
// A command line run on a node whose engine is stopped resolves the company
// through that node's own rows, and the fleet's store — which every node
// resolves through, and which holds nearly every value — is out of its reach.
// Its "" is UNKNOWN, and minting on it re-points a working hook at a key no
// node holds: every delivery refused, from a run whose plan said "mint".
func TestAnUnvouchedEmptyValueIsNotMintedInto(t *testing.T) {
	t.Parallel()
	sink := newMintSink()

	_, err := provision.MintSecret(context.Background(), sink, "WEBHOOK_SECRET",
		false, unknownTo("WEBHOOK_SECRET"), fixed("fresh"))
	if !errors.Is(err, provision.ErrUnvouched) {
		t.Fatalf("MintSecret = %v, want the unknown answer refused", err)
	}
	if !strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
	if sink.records != 0 {
		t.Errorf("%d value(s) were minted over a variable the run could not see", sink.records)
	}
}

// AND THE SINK STILL ANSWERS FIRST: a value it holds is one the run can use
// whatever the resolver could see, which is what MintSecret has always done
// with a value a resolver had not caught up with.
func TestAnUnvouchedNameTheSinkHoldsIsUsed(t *testing.T) {
	t.Parallel()
	sink := newMintSink()
	sink.vals["WEBHOOK_SECRET"] = "already-sealed"

	got, err := provision.MintSecret(context.Background(), sink, "WEBHOOK_SECRET",
		false, unknownTo("WEBHOOK_SECRET"), fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got.Value != "already-sealed" || got.Minted {
		t.Errorf("MintSecret = %+v, want the value the sink holds", got)
	}
}

// RECREATE IS A DECISION, NOT A REASON, so it is not asked about: an operator
// who asked for a fresh key gets one whatever the run could see.
func TestRecreateIsNotHeldToWhatTheRunCouldSee(t *testing.T) {
	t.Parallel()
	sink := newMintSink()

	got, err := provision.MintSecret(context.Background(), sink, "WEBHOOK_SECRET",
		true, unknownTo("WEBHOOK_SECRET"), fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if !got.Minted {
		t.Errorf("MintSecret = %+v, want the rotation the operator asked for", got)
	}
}

// A VOUCHED-FOR NAME IS MINTED INTO as before, so the question narrows the
// rule to exactly the names the run could not see.
func TestAVouchedForNameIsStillMintedInto(t *testing.T) {
	t.Parallel()
	sink := newMintSink()

	got, err := provision.MintSecret(context.Background(), sink, "WEBHOOK_SECRET",
		false, unknownTo("SOME_OTHER_NAME"), fixed("fresh"))
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if !got.Minted {
		t.Errorf("MintSecret = %+v, want a mint into a name the run vouched for", got)
	}
}

// A NON-EMPTY ANSWER THE RUN CANNOT VOUCH FOR IS NO ANSWER EITHER.
//
// A run on a stopped node resolves through that node's own rows and then its
// shell's environment, as the engine resolves through the store and then its
// own environment. For a variable the rows lack, the exported value is a guess
// at the deployment's — the fleet may hold another, and the store outranks the
// environment in the engine — and registering it with a vendor re-points a
// working hook exactly as a mint would. So it is settled like an empty answer:
// the sink's record where there is one, refused where there is none.
func TestResolvedSettlesAnAnswerTheRunCannotVouchFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		ref       string
		resolved  string
		unvouched provision.Unvouched
		held      map[string]string
		want      string
		ok        bool
		unknown   bool
		reads     int
	}{
		{name: "a vouched answer is used as it is, without a read",
			ref: "${SECRET}", resolved: "  from-the-store\n", unvouched: unknownTo("OTHER"),
			want: "from-the-store", ok: true},
		{name: "the engine's own pass vouches for everything",
			ref: "${SECRET}", resolved: "x", unvouched: nil, want: "x", ok: true},
		{name: "a vouched empty answer is nothing to use, and the caller may mint",
			ref: "${SECRET}", resolved: "", unvouched: unknownTo("OTHER")},
		{name: "a literal reads no variable, so it is always vouched for",
			ref: "a-literal", resolved: "a-literal", unvouched: unknownTo("SECRET"),
			want: "a-literal", ok: true},
		{name: "a guessed answer nothing recorded is refused",
			ref: "${SECRET}", resolved: "from-the-shell", unvouched: unknownTo("SECRET"),
			unknown: true, reads: 1},
		{name: "a guessed answer gives way to the sink's record",
			ref: "${SECRET}", resolved: "from-the-shell", unvouched: unknownTo("SECRET"),
			held: map[string]string{"SECRET": " recorded "}, want: "recorded", ok: true, reads: 1},
		{name: "an empty unvouched answer is refused where nothing recorded it",
			ref: "${SECRET}", resolved: "", unvouched: unknownTo("SECRET"),
			unknown: true, reads: 1},
		{name: "a guessed fragment has no variable to read back, and is refused",
			ref: "prefix-${SECRET}", resolved: "prefix-from-the-shell",
			unvouched: unknownTo("SECRET"), unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := newMintSink()
			for name, value := range tc.held {
				sink.vals[name] = value
			}
			got, ok, err := provision.Resolved(context.Background(), sink, tc.ref,
				tc.resolved, tc.unvouched)
			switch {
			case tc.unknown && !errors.Is(err, provision.ErrUnvouched):
				t.Fatalf("Resolved = (%q, %v, %v), want the guess refused", got, ok, err)
			case !tc.unknown && err != nil:
				t.Fatalf("Resolved: %v", err)
			}
			if got != tc.want || ok != tc.ok {
				t.Errorf("Resolved = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
			if sink.reads != tc.reads {
				t.Errorf("the sink was read %d time(s), want %d", sink.reads, tc.reads)
			}
			// THE CONTRACT A CALLER BUILDS ON: a non-empty answer comes
			// back used or refused, never as nothing — or a caller that
			// planned to reuse it would register an empty key.
			if strings.TrimSpace(tc.resolved) != "" && err == nil && (!ok || got == "") {
				t.Errorf("a non-empty answer came back as nothing: (%q, %v)", got, ok)
			}
		})
	}
}

// AND NO SINK HOLDS NO RECORD: a guessed answer with nowhere to read one back
// is refused rather than read through a nil sink.
func TestResolvedWithNoSinkRefusesAGuess(t *testing.T) {
	t.Parallel()
	_, _, err := provision.Resolved(context.Background(), nil, "${SECRET}",
		"from-the-shell", unknownTo("SECRET"))
	if !errors.Is(err, provision.ErrUnvouched) {
		t.Fatalf("Resolved with no sink = %v, want the guess refused", err)
	}
}
