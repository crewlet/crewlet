package compact_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// model answers every rewrite with answer(text), recording what it was shown.
type model struct {
	mu     sync.Mutex
	answer func(text string, call int) string
	err    error
	shown  []string
	asked  []llm.Request
}

func (m *model) Model() string { return "aux-model" }

func (m *model) Complete(_ context.Context, r llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, r)
	if m.err != nil {
		return nil, m.err
	}
	user := r.Messages[len(r.Messages)-1].Content
	text := user[strings.Index(user, "<<<TEXT\n")+len("<<<TEXT\n") : strings.LastIndex(user, "\nTEXT>>>")]
	m.shown = append(m.shown, text)
	return &llm.Completion{Content: m.answer(text, len(m.asked))}, nil
}

func (m *model) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asked)
}

type models struct {
	provider llm.Provider
	err      error
}

func (m models) Head(*org.Role, phase.Phase) (chain.Member, error) {
	if m.err != nil {
		return chain.Member{}, m.err
	}
	return chain.Member{Key: "aux", Provider: m.provider}, nil
}

// short answers with a fixed small rewrite, whatever it was shown.
func short(string, int) string { return "condensed" }

func seat() *org.Role { return &org.Role{Name: "Writer"} }

// TEXT THAT FITS IS NOT TOUCHED, and costs nothing: no model is asked, and the
// result does not claim to be a rewrite.
func TestTextThatFitsIsReturnedWithNoCall(t *testing.T) {
	t.Parallel()
	m := &model{answer: short}
	c := compact.New(models{provider: m}, compact.NewCache())
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindArgument, Text: "already short", Budget: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "already short" || got.Compacted || got.Note() != "" {
		t.Fatalf("a fitting text came back as %+v", got)
	}
	if m.calls() != 0 {
		t.Fatalf("a fitting text cost %d calls", m.calls())
	}
}

// OVER BUDGET IS REWRITTEN, NOT CUT: the model is shown the WHOLE text, and
// what comes back is its answer, announced as a rewrite.
func TestOverBudgetTextIsRewrittenFromTheWhole(t *testing.T) {
	t.Parallel()
	m := &model{answer: short}
	c := compact.New(models{provider: m}, compact.NewCache())
	long := strings.Repeat("the deploy of v2.3 to staging went out at 14:05; ", 40) + "TAIL-MARKER"
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindArgument, Text: long, Budget: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "condensed" || !got.Compacted || got.From != len(long) {
		t.Fatalf("got %+v", got)
	}
	if len(m.shown) != 1 || m.shown[0] != long {
		t.Fatalf("the model was not shown the whole text — the tail is what a cut loses")
	}
	if !strings.Contains(got.Note(), "condensed by a model") {
		t.Fatalf("a rewrite must say it is one: %q", got.Note())
	}
}

// THE LIMIT REACHES THE MODEL, as a number, and the call is reproducible.
func TestTheBudgetIsStatedToTheModel(t *testing.T) {
	t.Parallel()
	m := &model{answer: short}
	c := compact.New(models{provider: m}, compact.NewCache())
	_, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindSource, Text: strings.Repeat("x ", 500),
		Budget: 300, Focus: "how do I rotate the signing key?",
	})
	if err != nil {
		t.Fatal(err)
	}
	system := m.asked[0].Messages[0].Content
	if !strings.Contains(system, "at most 300 characters") {
		t.Fatalf("the budget is not in the instructions:\n%s", system)
	}
	if !strings.Contains(system, "how do I rotate the signing key?") {
		t.Fatalf("a source's question is not in the instructions:\n%s", system)
	}
	if temp := m.asked[0].Temperature; temp == nil || *temp != 0 {
		t.Fatalf("a rewrite must be reproducible, temperature %v", temp)
	}
	if len(m.asked[0].Tools) != 0 {
		t.Fatal("a rewrite offers no tools")
	}
}

// A MISS IS RETRIED ONCE, FROM THE ANSWER: the second call is shown the first
// rewrite, not the original, and told by how much it missed.
func TestAnOverlongRewriteIsAskedAgainFromItself(t *testing.T) {
	t.Parallel()
	first := strings.Repeat("y", 150)
	m := &model{answer: func(_ string, call int) string {
		if call == 1 {
			return first
		}
		return "shorter"
	}}
	c := compact.New(models{provider: m}, compact.NewCache())
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindToolError, Text: strings.Repeat("z", 1000), Budget: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "shorter" || got.Calls != 2 {
		t.Fatalf("got %+v", got)
	}
	if m.shown[1] != first {
		t.Fatal("the retry must start from the over-long rewrite")
	}
	if !strings.Contains(m.asked[1].Messages[1].Content, "150 characters and the limit is 100") {
		t.Fatalf("the retry does not say by how much it missed:\n%s", m.asked[1].Messages[1].Content)
	}
}

// IT NEVER FALLS BACK TO A CUT. A model that cannot get under the budget is an
// error with NO text, so a caller cannot use a half-measure by accident — this
// is the mutation that would quietly reinstate everything this package
// replaced.
func TestAModelThatCannotFitIsAnErrorNotACut(t *testing.T) {
	t.Parallel()
	m := &model{answer: func(text string, _ int) string { return text + text }}
	c := compact.New(models{provider: m}, compact.NewCache())
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindArgument, Text: strings.Repeat("w", 400), Budget: 100,
	})
	if !errors.Is(err, compact.ErrOverBudget) {
		t.Fatalf("err = %v, want ErrOverBudget", err)
	}
	if got.Text != "" || got.Compacted {
		t.Fatalf("a failed rewrite returned text %q — that is a cut by another name", got.Text)
	}
}

// NO MODEL IS UNAVAILABLE, from every direction: a nil compactor, a seat with
// no auxiliary chain, a call that failed, an answer with nothing in it.
func TestNoRewriteIsUnavailable(t *testing.T) {
	t.Parallel()
	req := compact.Request{Seat: seat(), Kind: compact.KindThread,
		Text: strings.Repeat("v", 500), Budget: 50}
	cases := map[string]*compact.Compactor{
		"nil compactor":   nil,
		"nil seam":        compact.New(nil, compact.NewCache()),
		"no chain":        compact.New(models{err: errors.New("no auxiliary model")}, compact.NewCache()),
		"call failed":     compact.New(models{provider: &model{err: errors.New("503")}}, compact.NewCache()),
		"answered blank":  compact.New(models{provider: &model{answer: func(string, int) string { return "  " }}}, compact.NewCache()),
		"answered broken": compact.New(models{provider: &model{answer: func(string, int) string { return "\xff\xfe" }}}, compact.NewCache()),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := c.Fit(context.Background(), req)
			if !errors.Is(err, compact.ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			if got.Text != "" {
				t.Fatalf("an unavailable rewrite returned %q", got.Text)
			}
		})
	}
}

// A REPEATED REQUEST IS FREE: a block re-rendered every round pays once.
// And the cache keys on everything that changes the answer — a different
// budget or question is a different rewrite.
func TestARepeatedRequestIsServedFromTheCache(t *testing.T) {
	t.Parallel()
	m := &model{answer: short}
	c := compact.New(models{provider: m}, compact.NewCache())
	req := compact.Request{Seat: seat(), Kind: compact.KindSource,
		Text: strings.Repeat("u", 900), Budget: 120, Focus: "first question"}
	for range 3 {
		if _, err := c.Fit(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if m.calls() != 1 {
		t.Fatalf("three identical requests cost %d calls", m.calls())
	}
	req.Focus = "second question"
	if _, err := c.Fit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Budget = 121
	if _, err := c.Fit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if m.calls() != 3 {
		t.Fatalf("a different question or budget must be a different rewrite: %d calls", m.calls())
	}
}

// AN INPUT LARGER THAN ONE CALL IS READ IN PIECES, ALL OF THEM: the pieces the
// model was shown concatenate back to the input, and the joined rewrites are
// compacted again until the whole fits.
func TestALargeInputIsReadWholeInPieces(t *testing.T) {
	t.Parallel()
	var line strings.Builder
	for i := range 4000 {
		line.WriteString("line ")
		line.WriteString(strings.Repeat("é", i%7))
		line.WriteString(" of the transcript\n")
	}
	long := line.String() + strings.Repeat("final conclusion\n", 3000)
	if len(long) <= compact.ChunkBytes {
		t.Fatalf("the input must need more than one call: %d bytes", len(long))
	}
	m := &model{answer: func(text string, _ int) string {
		return "piece of " + string(rune('0'+len(text)%10))
	}}
	c := compact.New(models{provider: m}, compact.NewCache())
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindReport, Text: long, Budget: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Text) > 4096 || !got.Compacted {
		t.Fatalf("got %d bytes, compacted %v", len(got.Text), got.Compacted)
	}
	pieces := compact.Split(long, compact.ChunkBytes)
	if len(pieces) < 2 {
		t.Fatal("expected several pieces")
	}
	m.mu.Lock()
	firstPass := append([]string(nil), m.shown[:len(pieces)]...)
	m.mu.Unlock()
	seen := map[string]bool{}
	for _, s := range firstPass {
		seen[s] = true
	}
	for i, p := range pieces {
		if !seen[p] {
			t.Fatalf("piece %d of %d was never shown to the model", i, len(pieces))
		}
	}
}

// PAST WHAT ONE COMPACTION MAY SPEND, IT REFUSES before asking anything.
func TestAnInputPastTheChunkCeilingIsRefused(t *testing.T) {
	t.Parallel()
	m := &model{answer: short}
	c := compact.New(models{provider: m}, compact.NewCache())
	_, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindReport,
		Text: strings.Repeat("t", compact.MaxChunks*compact.ChunkBytes+1), Budget: 1024,
	})
	if !errors.Is(err, compact.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if m.calls() != 0 {
		t.Fatalf("a refused input cost %d calls", m.calls())
	}
}

// SPLITTING DROPS NOTHING AND BREAKS NO RUNE: the pieces concatenate back to
// the input, each is within the limit, and each is valid UTF-8 — across text
// with no spaces or newlines at all, where only the rune walk can place a cut.
func TestSplitLosesNothing(t *testing.T) {
	t.Parallel()
	inputs := map[string]string{
		"prose":       strings.Repeat("a sentence with words in it.\n\n", 400),
		"one line":    strings.Repeat("word ", 3000),
		"no breaks":   strings.Repeat("aé€𝄞", 3000),
		"short":       "tiny",
		"exact limit": strings.Repeat("q", 1024),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			pieces := compact.Split(in, 1024)
			if strings.Join(pieces, "") != in {
				t.Fatal("the pieces do not concatenate back to the input")
			}
			for i, p := range pieces {
				if len(p) > 1024 {
					t.Fatalf("piece %d is %d bytes", i, len(p))
				}
				if !utf8.ValidString(p) {
					t.Fatalf("piece %d splits a rune", i)
				}
				if p == "" {
					t.Fatalf("piece %d is empty", i)
				}
			}
		})
	}
}

// EVERY KIND HAS INSTRUCTIONS, and an unknown one is refused rather than sent
// with none.
func TestEveryKindHasInstructions(t *testing.T) {
	t.Parallel()
	for _, k := range compact.Kinds {
		if !k.Valid() {
			t.Errorf("kind %q has no brief", k)
		}
	}
	m := &model{answer: short}
	_, err := compact.New(models{provider: m}, compact.NewCache()).Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: "nonsense", Text: strings.Repeat("s", 100), Budget: 10,
	})
	if err == nil || m.calls() != 0 {
		t.Fatalf("an unknown kind was sent: err %v, calls %d", err, m.calls())
	}
}

// AN OMITTED TEXT SHOWS NO FRAGMENT OF ITSELF, and two different texts are
// still told apart — the property the judge's repetition question rests on.
func TestOmittedNamesSizeAndIdentityNeverContent(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("secret body one ", 300)
	b := strings.Repeat("secret body two ", 300)
	if strings.Contains(compact.Omitted(a), "secret") {
		t.Fatalf("an omission quoted the text: %q", compact.Omitted(a))
	}
	if compact.Omitted(a) == compact.Omitted(b) {
		t.Fatal("two different texts rendered the same omission")
	}
	if again := strings.Clone(a); compact.Omitted(again) != compact.Omitted(a) {
		t.Fatal("one text rendered two different omissions")
	}
	if !strings.Contains(compact.Omitted(a), "KiB") {
		t.Fatalf("an omission must say how much is missing: %q", compact.Omitted(a))
	}
}

// A BOUND COMPACTOR CHARGES ITS SEAT: the seat it was bound to is the one the
// model seam is asked for.
func TestABoundCompactorAsksForItsOwnSeat(t *testing.T) {
	t.Parallel()
	var asked []*org.Role
	m := &model{answer: short}
	seen := seatModels{provider: m, seen: &asked}
	writer := &org.Role{Name: "Writer"}
	_, err := compact.New(seen, compact.NewCache()).For(writer).Fit(context.Background(),
		compact.KindArgument, strings.Repeat("r", 300), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != writer {
		t.Fatalf("the rewrite was resolved for %v, not the bound seat", asked)
	}
	var zero compact.Bound
	if _, err := zero.Fit(context.Background(), compact.KindArgument, strings.Repeat("r", 300), 50); !errors.Is(err, compact.ErrUnavailable) {
		t.Fatalf("a zero Bound rewrote something: %v", err)
	}
}

type seatModels struct {
	provider llm.Provider
	seen     *[]*org.Role
}

func (m seatModels) Head(role *org.Role, ph phase.Phase) (chain.Member, error) {
	if ph != phase.Auxiliary {
		return chain.Member{}, errors.New("a rewrite runs on the auxiliary chain")
	}
	*m.seen = append(*m.seen, role)
	return chain.Member{Key: "aux", Provider: m.provider}, nil
}
