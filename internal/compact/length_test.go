package compact_test

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// askedChars is the test's own reading of the length a request asked for: the
// last "at most N characters" in it, the retry note's when there is one and the
// instructions' otherwise.
var askedChars = regexp.MustCompile(`at most (\d+) characters`)

// overshooting is a model that answers LONGER THAN IT WAS ASKED by a fixed
// ratio, the way the auxiliary models on a live company did: asked for 200, 588
// or 6000 characters, they answered between 1.1 and 1.6 times that, and on
// short budgets up to three times. It writes in the rune it is given, so a test
// can hold the characters it answers apart from the bytes they weigh.
type overshooting struct {
	mu    sync.Mutex
	ratio func(call int) float64
	r     rune
	asked []int
}

func (m *overshooting) Model() string { return "aux-model" }

func (m *overshooting) Complete(_ context.Context, r llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var stated int
	for _, msg := range r.Messages {
		for _, match := range askedChars.FindAllStringSubmatch(msg.Content, -1) {
			n, err := strconv.Atoi(match[1])
			if err != nil {
				return nil, err
			}
			stated = n
		}
	}
	if stated == 0 {
		return nil, errors.New("the request stated no length in characters")
	}
	m.asked = append(m.asked, stated)
	n := int(math.Ceil(float64(stated) * m.ratio(len(m.asked))))
	return &llm.Completion{Content: strings.Repeat(string(m.r), n)}, nil
}

func (m *overshooting) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asked)
}

func steady(ratio float64) func(int) float64 { return func(int) float64 { return ratio } }

// A MODEL THAT OVERSHOOTS WHAT IT IS ASKED STILL FITS, FIRST TIME. Asking for
// the budget itself made a typical answer a miss: a model that answers 1.4
// times the length it was told failed both of its attempts on every budget. It
// is asked for less than the result is held to, so the same model lands inside
// on its first call.
func TestAModelThatOvershootsItsAskStillFits(t *testing.T) {
	t.Parallel()
	for _, budget := range []int{200, 588, 6000} {
		m := &overshooting{ratio: steady(1.4), r: 'a'}
		c := compact.New(models{provider: m}, compact.NewCache())
		got, err := c.Fit(context.Background(), compact.Request{
			Seat: seat(), Kind: compact.KindArgument, Text: strings.Repeat("b", budget*3),
			Budget: budget,
		})
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if len(got.Text) > budget || m.calls() != 1 {
			t.Fatalf("budget %d: %d bytes in %d calls", budget, len(got.Text), m.calls())
		}
		if m.asked[0] >= budget {
			t.Fatalf("budget %d: the model was asked for %d characters, the budget itself",
				budget, m.asked[0])
		}
	}
}

// A MISS MOVES THE AIM BY THE MISS. A model that answered three times what it
// was asked is asked next for a third of that, from its own answer, and fits;
// told the limit it missed and asked for "shorter", it shrank by a few percent
// and missed again.
func TestAMissIsCalibratedByHowFarItMissed(t *testing.T) {
	t.Parallel()
	m := &overshooting{ratio: steady(3), r: 'a'}
	c := compact.New(models{provider: m}, compact.NewCache())
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindToolError, Text: strings.Repeat("c", 2000), Budget: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Text) > 200 || got.Calls != 2 {
		t.Fatalf("%d bytes in %d calls, want a fit on the second", len(got.Text), got.Calls)
	}
	if m.asked[1]*3 > 200 {
		t.Fatalf("after a threefold miss the model was asked for %d characters", m.asked[1])
	}
}

// THE LENGTH A MODEL IS ASKED FOR IS IN ITS UNIT, NOT OURS. A result is held to
// bytes and a model counts characters; for a text in two-byte characters the
// two differ by half, and a model that wrote exactly the characters it was told
// used to land at twice its budget.
func TestTheAskIsInCharactersOfTheTextsOwnWeight(t *testing.T) {
	t.Parallel()
	m := &overshooting{ratio: steady(1), r: 'é'}
	c := compact.New(models{provider: m}, compact.NewCache())
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindThread, Text: strings.Repeat("é", 1000), Budget: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Text) > 200 || m.calls() != 1 {
		t.Fatalf("%d bytes in %d calls", len(got.Text), m.calls())
	}
	if utf8.RuneCountInString(got.Text)*2 != len(got.Text) {
		t.Fatal("the stub did not answer in two-byte characters")
	}
}

// A FIRST-PASS PIECE IS NOT HELD TO ITS SHARE. A conversation of 145 KiB held
// to 6000 bytes is read in three pieces, each asked for its share; a model that
// answers every piece somewhat over that share used to fail the whole
// compaction, cancelling its siblings, which left a live seat's older turns
// uncondensed on every turn. The pieces are joined and fitted again, so a
// piece over its share is progress, and the result fits.
func TestALargeConversationFitsWhenEveryPieceOvershootsItsShare(t *testing.T) {
	t.Parallel()
	m := &overshooting{ratio: steady(1.5), r: 'a'}
	c := compact.New(models{provider: m}, compact.NewCache())
	text := strings.Repeat("turn: the agent posted the release notes to #eng and opened PR 42.\n", 2200)
	if len(text) <= 2*compact.ChunkBytes {
		t.Fatalf("the input must need three pieces: %d bytes", len(text))
	}
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindConversation, Text: text, Budget: 6000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Text) > 6000 || !got.Compacted {
		t.Fatalf("%d bytes, compacted %v", len(got.Text), got.Compacted)
	}
}

// A PASS THAT DOES NOT SHRINK ENDS THE COMPACTION, at once: a model that echoes
// every piece back gains nothing from another pass, and paying for one is
// spending on a summary that is not happening.
func TestAPassThatDoesNotShrinkIsOverBudget(t *testing.T) {
	t.Parallel()
	m := &model{answer: func(text string, _ int) string { return text }}
	c := compact.New(models{provider: m}, compact.NewCache())
	text := strings.Repeat("line of a long report\n", 2*compact.ChunkBytes/22)
	pieces := len(compact.Split(text, compact.ChunkBytes))
	got, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindReport, Text: text, Budget: 4096,
	})
	if !errors.Is(err, compact.ErrOverBudget) || got.Text != "" {
		t.Fatalf("err = %v, text %d bytes; want ErrOverBudget and no text", err, len(got.Text))
	}
	if m.calls() != pieces {
		t.Fatalf("a pass that did not shrink cost %d calls, want the %d of that pass",
			m.calls(), pieces)
	}
}

// THE ATTEMPTS ARE BOUNDED: a model that keeps answering longer than any length
// it is asked for is over budget after three calls, never a fourth.
func TestTheAttemptsAtOneRewriteAreBounded(t *testing.T) {
	t.Parallel()
	m := &model{answer: func(string, int) string { return strings.Repeat("d", 500) }}
	c := compact.New(models{provider: m}, compact.NewCache())
	_, err := c.Fit(context.Background(), compact.Request{
		Seat: seat(), Kind: compact.KindArgument, Text: strings.Repeat("e", 1000), Budget: 100,
	})
	if !errors.Is(err, compact.ErrOverBudget) {
		t.Fatalf("err = %v, want ErrOverBudget", err)
	}
	if m.calls() != 3 {
		t.Fatalf("a model that never fits was asked %d times, want 3", m.calls())
	}
}
