// Package compact makes text fit a size budget by having a model REWRITE it,
// never by cutting it.
//
// # Why a cut is the wrong tool for anything a model reads
//
// A byte or rune cut keeps whatever happened to come first and drops the rest
// unread. For a log field that is a tolerable loss; for text a model reasons
// over it is a different message: a comment cut at its cap reads as a comment
// that ended there, a runbook cut at four kilobytes reads as a runbook with no
// step four, and an error document cut at two hundred runes keeps the HTTP
// preamble and loses the line naming the missing scope. A marker says
// something was cut, but not what — and the reader cannot go back for it,
// because the reason a bound exists is that the whole was too large to carry.
//
// So where a bound is genuine — a block re-sent on every round of every
// phase, a document bigger than the model answering from it can hold — the
// over-budget text is handed to the seat's AUXILIARY chain (the cheap model
// `llm_auxiliary` names, falling back to `llm`), which rewrites it to fit:
// identifiers verbatim, every action and decision kept, repetition and
// boilerplate dropped first. A cut chooses by position; a rewrite chooses by
// meaning, which is the only thing a budget should ever be spent on.
//
// # What this package does not decide
//
// WHETHER a bound is needed is the caller's question, answered at the call
// site with the reason the bound exists — and most of what used to be cut in
// this tree needed no bound at all, because something upstream already
// limited it (a field's own cap, a tool answer's ceiling) or a reader could
// page through it whole. A compaction is the answer only where the content is
// genuinely unbounded AND has to be read in one piece.
//
// WHAT TO DO WHEN A REWRITE CANNOT BE HAD is the caller's too, because the
// honest fallback differs: a conversation block can still drop its oldest
// entries whole and say how many, an argument value can be named by its size,
// and a document can be skipped with a note. What no caller may do is fall
// back to the cut this package replaced. [Compactor.Fit] therefore returns an
// error rather than a degraded string, and the error says which of three
// things happened — no model ([ErrUnavailable]), a model that could not get
// under the budget ([ErrOverBudget]), or an input past what one compaction may
// spend ([ErrTooLarge]).
//
// # A rewrite reads its text as somebody's
//
// What a rewrite is handed is as often as not somebody else's words — a
// thread, a page, a task — and what it writes back lands where they were
// going, a seat's prompt among them. So every completion reads its text inside
// a fence of a token minted for that one call (ADR-0040), labelled with what
// the caller says the text is ([Request.Label], [Bound.Labelled]; the zero
// label says nothing and reads as text nothing vouches for), its system prompt
// ends with the standing sentence that says what a fence means, and its answer
// is neutralised under that token, so nothing it echoes opens or closes a
// fence wherever the caller puts it.
//
// # Cost
//
// Every rewrite is a completion on the seat's own auxiliary chain, resolved
// through the same seam every learning worker uses, so it is charged to the
// seat's token windows like any other auxiliary call and recorded beside them
// as `auxiliary_spend` under the purpose `condense_<kind>` — in the turn's cost
// when the caller bound a turn's attribution ([Compactor.For]). Three things keep
// that bounded: text that already fits is returned without a call; a result
// is cached by its input, so a block re-rendered on every round pays once per
// turn rather than once per round; and an input is split into at most
// [MaxChunks] pieces, past which the compaction refuses rather than spending
// more on summarising a document than the work it serves.
package compact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

var log = logging.Get("compact")

// Models is the seat-model seam a compaction resolves its model through: the
// engine's ONE auxiliary seam the learning workers and the prefetch also use,
// so a rewrite is charged and recorded exactly as any other auxiliary call
// is. A rewrite states its attribution there like every other call: the
// stage and the turn its caller bound ([Compactor.For]), and the purpose
// `condense_<kind>` this package derives from what the text is.
type Models interface {
	Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error)
}

const (
	// ChunkBytes is the largest piece of input one completion is handed.
	//
	// 64 KiB is about 16 000 tokens — a small share of the context of
	// every model this engine targets, so a chunk never competes with the
	// instructions for room, and small enough that the rewrite of one piece
	// stays faithful: a model asked to compress a quarter of a million
	// tokens in one breath keeps the opening and the end and blurs the
	// middle, which is the cut this package exists to replace, done by a
	// model instead.
	ChunkBytes = 64 << 10

	// MaxChunks bounds how many first-pass completions one compaction may
	// make, which bounds its input at MaxChunks × ChunkBytes (2 MiB).
	//
	// THIRTY-TWO, anchored on cost rather than on capacity: thirty-two
	// sixteen-thousand-token calls are about half a million input tokens,
	// which is the order of a whole long Execute phase. Past that a
	// compaction costs more than the work it is serving, and the honest
	// answer is [ErrTooLarge] — the caller then says the content was too
	// large to carry, which a reader can act on, rather than paying for a
	// summary nobody priced.
	MaxChunks = 32

	// Parallel bounds how many chunk rewrites run at once.
	//
	// Four: enough that a two-megabyte input takes the time of eight calls
	// rather than thirty-two, and few enough that one compaction does not
	// open a burst against the seat's provider that its rate limit answers
	// by benching every key in the pool.
	Parallel = 4

	// CallTimeout bounds one completion.
	//
	// Sixty seconds, against [github.com/crewlet/crewlet/internal/agent/prefetch.AuxTimeout]'s
	// thirty: a chunk is up to sixteen thousand tokens in and a few
	// thousand out, which on a small model is twenty to forty seconds at
	// the tail. A compaction that cannot finish in a minute per piece is a
	// provider in trouble, and the caller's fallback is better than a turn
	// stalled behind it.
	CallTimeout = 60 * time.Second

	// minPieceBytes is the floor one chunk's share of the budget may fall to
	// in the first pass.
	//
	// A budget divided evenly across thirty-two chunks can leave each one a
	// few dozen bytes, which no model can honour faithfully. A piece's share
	// is at least this much, and a joined first pass that is then
	// over the whole budget is compacted once more as a whole, so the floor
	// costs one more call, never a cut.
	minPieceBytes = 2 << 10

	// maxDepth bounds how many reduce passes a compaction may take.
	//
	// A first-pass piece is ASKED for its share but not held to it (see
	// [pass.fit]), so what bounds the reduction is this depth and the rule
	// that every pass must leave the text shorter than it found it. Three
	// passes take [MaxChunks] chunks down to one with room to spare; a model
	// that needs a fourth keeps answering longer than it was asked to, and
	// that is [ErrOverBudget], not a reason to keep spending.
	maxDepth = 3

	// aimPercent is the share of its budget a rewrite is ASKED for, as
	// against the budget it is HELD to.
	//
	// SIXTY PERCENT, from what models answer when asked for a length. A
	// model writes tokens and judges length loosely, and it overshoots a
	// stated limit far more often than it undershoots one: asked for at most
	// 200, 588 or 6000 characters, the auxiliary models this engine runs
	// answered between 1.1 and 1.6 times that, and on short budgets up to
	// three times. Asking for the budget itself made a typical answer a miss.
	// Asking for three fifths of it puts the typical overshoot back inside
	// the budget, and the attempts after it are calibrated by the miss that
	// was measured (see [nextAim]).
	aimPercent = 60

	// maxAttempts bounds the rewrites of one text that must fit.
	//
	// THREE: the first at [aimPercent], and two more from the previous
	// over-long answer, each aimed lower by exactly the ratio the model
	// overshot by. The retries read the answer rather than the original, so
	// each costs a fraction of the first.
	maxAttempts = 3

	// charsPerWord converts a length in characters into the words a model
	// is also told. A model keeps to a word count far better than to a
	// character count it cannot see, and English prose with identifiers in
	// it runs to about six characters a word, space included.
	charsPerWord = 6
)

var (
	// ErrUnavailable is no rewrite at all: no auxiliary model resolves for
	// the seat, or the call failed or answered nothing.
	ErrUnavailable = errors.New("compact: no auxiliary model could rewrite the text")

	// ErrOverBudget is a model that answered, every attempt, with a rewrite
	// still larger than the budget, or a reduce pass that did not shrink.
	ErrOverBudget = errors.New("compact: the rewrite did not fit the budget")

	// ErrTooLarge is an input past [MaxChunks] × [ChunkBytes].
	ErrTooLarge = errors.New("compact: the text is too large to compact")
)

// Kind is what the text is, which decides what the rewrite must keep.
//
// A named string so an unknown value is a value rather than a panic; [Valid]
// says whether this build has instructions for it.
type Kind string

const (
	// KindConversation is the earlier part of a conversation an agent had:
	// what it was asked, what it called, what it replied.
	KindConversation Kind = "conversation"
	// KindThread is the earlier messages of a chat thread.
	KindThread Kind = "thread"
	// KindArgument is one argument value of a tool call already made — a
	// message body, a document, a diff.
	KindArgument Kind = "argument"
	// KindToolError is the text a failed tool call returned.
	KindToolError Kind = "tool_error"
	// KindProduced is what an earlier round of a turn produced, ending in
	// its deliverable.
	KindProduced Kind = "produced"
	// KindSource is a document read to answer a question; the request's
	// Focus is that question.
	KindSource Kind = "source"
	// KindTask is the task an agent was given, as its trigger rendered it.
	KindTask Kind = "task"
	// KindAnswer is what a delegated worker returned for its task: its
	// structured submission as JSON, or its prose.
	KindAnswer Kind = "answer"
	// KindOutcome is what a turn concluded — its final answer, or the
	// reviewer's account of what landed — condensed for a one-glance card.
	KindOutcome Kind = "outcome"
	// KindReport is a harness's report on the run it finished.
	KindReport Kind = "report"
)

// Kinds is every kind this build has instructions for.
var Kinds = []Kind{KindConversation, KindThread, KindArgument, KindToolError,
	KindProduced, KindSource, KindTask, KindAnswer, KindOutcome, KindReport}

// Valid reports whether this build knows the kind.
func (k Kind) Valid() bool {
	_, ok := kindBriefs[k]
	return ok
}

// Request is one text to fit.
type Request struct {
	// Seat is whose auxiliary chain runs the rewrite and whose token
	// windows it is charged to.
	Seat *org.Role

	// Use is whose cost the rewrite is — its stage, the turn it serves and,
	// inside a turn, the turn's tally — as the caller states it. The
	// purpose is this package's to set, from Kind: `condense_<kind>`
	// (types.AuxCondense), so a rewrite of a chat thread and one of an
	// executor's report are told apart on every breakdown. The event
	// catalogue keeps its own copy of the kinds, and this package's suite
	// holds the two equal in both directions.
	Use auxspend.Use

	// Kind says what the text is.
	Kind Kind

	// Text is the whole of it.
	Text string

	// Budget is the most the result may weigh, in BYTES — the unit every
	// consumer downstream measures, from a prompt's block bound to an
	// event's payload ceiling. A result within it is within any rune
	// budget of the same number, so a caller holding a rune budget may
	// pass it unchanged.
	Budget int

	// Focus is what the reader needs the text FOR, when there is one: the
	// question a [KindSource] document is read to answer. Part of the cache
	// key, because the same page compacted for two questions is two
	// different rewrites.
	Focus string

	// Label is who wrote the text and how far to rely on it, as the rewrite
	// reads it inside its fence (ADR-0040): the caller's own label where it
	// has one — a thread's speakers, a page's, the asker of a past turn —
	// and the zero label otherwise, which says nothing and reads as text
	// nothing vouches for. For a rewrite that is enough, since everything it
	// is handed is data to rewrite and never an instruction to it. Part of
	// the cache key, because a different label is a different prompt.
	Label provenance.Label
}

// Result is a text that fits.
type Result struct {
	// Text is the input unchanged when it already fit, or its rewrite.
	Text string

	// Compacted is true when Text is a rewrite rather than the input.
	Compacted bool

	// From is the input's size in bytes, for the note a caller renders
	// beside a rewrite so a reader knows it is not reading the original.
	From int

	// Calls is how many completions this request made — zero for an input
	// that fit and for a cache hit.
	Calls int
}

// Note is the line a caller renders beside a rewrite: what the reader is
// looking at, and how much it stands in for.
//
// A REWRITE IS ALWAYS ANNOUNCED. It is shorter than the original and in a
// model's words rather than the author's, and a reader who took it for the
// original would quote it as though somebody had written it.
func (r Result) Note() string {
	if !r.Compacted {
		return ""
	}
	return fmt.Sprintf("(condensed by a model from %s; identifiers are verbatim, "+
		"wording is not)", size(r.From))
}

// size renders a byte count as a reader would say it.
func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// Compactor rewrites text to fit, through one seat-model seam.
//
// A NIL *Compactor is meaningful: it is a node with no auxiliary model wired,
// and every [Compactor.Fit] that needs a rewrite answers [ErrUnavailable] —
// so a caller holds one field and never a nil check of its own, and a test
// that wires none exercises the caller's fallback.
type Compactor struct {
	models Models
	cache  *Cache
	// timeout bounds one completion: [CallTimeout], held on the value so
	// a test can shorten it without a package variable.
	timeout time.Duration
}

// New builds a compactor over a seat-model seam and a cache.
//
// THE CACHE IS PASSED IN because the two live for different lengths of
// time: the seam belongs to one configuration epoch and is rebuilt by every
// apply, while the rewrites it produced stay right across an apply that did
// not touch them — and the turn whose ledger they serve may well be running
// across it. A nil seam is a compactor that can rewrite nothing; a nil cache
// is one that remembers nothing.
func New(models Models, cache *Cache) *Compactor {
	return &Compactor{models: models, cache: cache, timeout: CallTimeout}
}

// For binds the compactor to one seat and one attribution, which is the shape
// a caller inside a turn holds: every rewrite it asks for is that seat's, and
// that stage's and turn's cost — explicitly, because an ambient attribution
// would charge a rewrite to whichever turn last wrote it.
func (c *Compactor) For(seat *org.Role, use auxspend.Use) Bound {
	return Bound{c: c, seat: seat, use: use}
}

// Bound is a compactor bound to one seat and one attribution. Its zero value —
// and one bound from a nil compactor — rewrites nothing.
type Bound struct {
	c     *Compactor
	seat  *org.Role
	use   auxspend.Use
	label provenance.Label
}

// Labelled is this binding with its rewrites reading their text under label —
// see [Request.Label]: the caller that knows who wrote the text says so, and
// the rewrite is fenced as such.
func (b Bound) Labelled(label provenance.Label) Bound {
	b.label = label
	return b
}

// Tallied is this binding with its rewrites tallied on spend instead: the same
// seat and the same attribution, paid by another segment of the turn — see
// [github.com/crewlet/crewlet/internal/agent/turnctx.Turn.WithAuxSpend], whose
// reason it shares.
func (b Bound) Tallied(spend *auxspend.Tally) Bound {
	b.use.Tally = spend
	return b
}

// Fit is [Compactor.Fit] for the bound seat.
func (b Bound) Fit(ctx context.Context, kind Kind, text string, budget int) (Result, error) {
	return b.c.Fit(ctx, Request{Seat: b.seat, Use: b.use, Kind: kind, Text: text, Budget: budget,
		Label: b.label})
}

// Focused is [Compactor.Fit] for the bound seat with a question the text is
// read to answer — a [KindSource] document.
func (b Bound) Focused(ctx context.Context, kind Kind, text string, budget int, focus string) (Result, error) {
	return b.c.Fit(ctx, Request{Seat: b.seat, Use: b.use, Kind: kind, Text: text,
		Budget: budget, Focus: focus, Label: b.label})
}

// Omitted is how a caller renders a text it could neither carry nor rewrite:
// its size and a digest of it, never a fragment of it.
//
// A FRAGMENT IS THE ONE THING IT MAY NOT SHOW, because a fragment is the cut
// this package replaced: a reader handed the first two hundred bytes of a
// message reads them as the message. A size says how much is missing, and the
// digest is what still lets a reader tell two omitted values apart — the
// round-cap judge's whole question is whether two calls were the same call,
// and two identical bodies share a digest where two different ones do not.
func Omitted(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("(%s not shown, digest %s)", size(len(text)), hex.EncodeToString(sum[:4]))
}

// Fit returns text within the request's budget.
//
// Text that already fits is returned unchanged with no call. Otherwise it is
// rewritten by the seat's auxiliary model, from the cache when this exact
// request was answered before. It NEVER cuts: every failure is an error, and
// the caller's fallback is its own (see the package doc).
func (c *Compactor) Fit(ctx context.Context, req Request) (Result, error) {
	from := len(req.Text)
	if from <= req.Budget {
		return Result{Text: req.Text, From: from}, nil
	}
	if req.Budget <= 0 {
		return Result{From: from}, fmt.Errorf("%w: a budget of %d bytes holds nothing",
			ErrOverBudget, req.Budget)
	}
	if !req.Kind.Valid() {
		return Result{From: from}, fmt.Errorf("compact: unknown kind %q", req.Kind)
	}
	if c == nil || c.models == nil {
		return Result{From: from}, ErrUnavailable
	}
	if chunks := (from + ChunkBytes - 1) / ChunkBytes; chunks > MaxChunks {
		return Result{From: from}, fmt.Errorf("%w: %s is %d chunks of %s and one "+
			"compaction may read %d", ErrTooLarge, size(from), chunks, size(ChunkBytes), MaxChunks)
	}
	key := cacheKey(req)
	if text, ok := c.cache.Get(key); ok {
		return Result{Text: text, Compacted: true, From: from}, nil
	}
	member, err := c.models.Auxiliary(req.Seat, req.Use.For(types.AuxCondense(string(req.Kind))))
	if err != nil {
		return Result{From: from}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	run := &pass{c: c, req: req, member: member}
	text, err := run.fit(ctx, req.Text, req.Budget, 0)
	if retryAt, _, limited := llm.LimitedUntil(err); limited {
		// A WAIT, NOT A FAILURE: the model is at a usage limit it already
		// reported, and nothing was launched to hear it again. The caller
		// takes the error as it takes any compaction it cannot have.
		log.InfoContext(ctx, "compaction_limited", "seat", seatHandle(req.Seat),
			"kind", string(req.Kind), "model", member.Key,
			"retry_at", retryAt.UTC().Format(time.RFC3339))
		return Result{From: from, Calls: int(run.calls.Load())}, err
	}
	if err != nil {
		log.WarnContext(ctx, "compaction_failed", "seat", seatHandle(req.Seat),
			"kind", string(req.Kind), "bytes", from, "budget", req.Budget,
			"model", member.Key, "calls", run.calls.Load(), "error", err.Error())
		return Result{From: from, Calls: int(run.calls.Load())}, err
	}
	c.cache.Put(key, text)
	log.DebugContext(ctx, "compacted", "seat", seatHandle(req.Seat),
		"kind", string(req.Kind), "from", from, "to", len(text),
		"budget", req.Budget, "model", member.Key, "calls", run.calls.Load())
	return Result{Text: text, Compacted: true, From: from, Calls: int(run.calls.Load())}, nil
}

// pass is one compaction's state: its model and how many calls it made.
type pass struct {
	c      *Compactor
	req    Request
	member chain.Member
	// calls is atomic because the first pass rewrites chunks in parallel.
	calls atomic.Int64
}

// fit makes text fit budget, splitting it first when one call cannot read it.
//
// A FIRST-PASS PIECE IS ASKED FOR ITS SHARE BUT NOT HELD TO IT. Its answer is
// never the result: the pieces are joined and fitted again, so a piece that
// came back somewhat over its share is still progress, and failing the whole
// compaction over it (which this used to do, cancelling every sibling) threw
// away a reduction one more call would have finished. Only the rewrite whose
// answer IS the result is held to the budget ([pass.rewrite]); what bounds the
// passes before it is [maxDepth] and the rule that each one shrinks the text.
func (p *pass) fit(ctx context.Context, text string, budget, depth int) (string, error) {
	if len(text) <= budget {
		return text, nil
	}
	if depth > maxDepth {
		return "", fmt.Errorf("%w: still %s after %d passes against %s",
			ErrOverBudget, size(len(text)), maxDepth, size(budget))
	}
	if len(text) <= ChunkBytes {
		return p.rewrite(ctx, text, budget)
	}
	chunks := Split(text, ChunkBytes)
	per := max(budget/len(chunks), minPieceBytes)
	parts := make([]string, len(chunks))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(Parallel)
	for i, chunk := range chunks {
		group.Go(func() error {
			rewritten, err := p.complete(gctx, chunk, askFor(aim(per), chunk), "")
			if err != nil {
				return err
			}
			parts[i] = rewritten
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return "", err
	}
	joined := strings.Join(parts, "\n\n")
	if len(joined) >= len(text) {
		return "", fmt.Errorf("%w: a pass over %d pieces of %s answered %s, no shorter",
			ErrOverBudget, len(chunks), size(len(text)), size(len(joined)))
	}
	return p.fit(ctx, joined, budget, depth+1)
}

// rewrite asks the model for a rewrite of text that fits budget, and asks
// again, from its own over-long answer, while one misses.
//
// THE BUDGET IS WHAT AN ANSWER IS HELD TO, NOT WHAT IT IS ASKED FOR. The first
// attempt asks for [aimPercent] of it; every attempt after a miss asks for
// less by the ratio the model overshot what it was asked ([nextAim]), from the
// answer rather than the original: it is already most of the way there and a
// fraction of the input's size, so the attempt is cheap, and a target moved
// by the measured miss lands where repeating the same limit did not.
func (p *pass) rewrite(ctx context.Context, text string, budget int) (string, error) {
	want := aim(budget)
	source, retry := text, ""
	misses := make([]string, 0, maxAttempts)
	for range maxAttempts {
		ask := askFor(want, text)
		answer, err := p.complete(ctx, source, ask, retry)
		if err != nil {
			return "", err
		}
		if len(answer) <= budget {
			return answer, nil
		}
		misses = append(misses, size(len(answer)))
		want = nextAim(budget, want, len(answer))
		retry = retryNote(answer, askFor(want, text))
		source = answer
	}
	return "", fmt.Errorf("%w: %s held to %s answered %s", ErrOverBudget, p.member.Key,
		size(budget), strings.Join(misses, ", then "))
}

// aim is the bytes a rewrite held to budget is first asked for: [aimPercent]
// of it, and never nothing.
func aim(budget int) int {
	return max(budget*aimPercent/100, 1)
}

// nextAim is what the attempt after a miss is asked for. The model was asked
// for asked bytes and answered got; it is asked next for the first attempt's
// aim scaled down by that same ratio, so that the overshoot it showed lands
// the next answer near the aim rather than over the budget again.
func nextAim(budget, asked, got int) int {
	return max(aim(budget)*asked/got, 1)
}

// askFor is a length a model can see, for a rewrite measured in bytes.
//
// THE UNIT A MODEL READS IS NOT THE UNIT A RESULT IS MEASURED IN. A model sees
// characters and words; every consumer downstream measures bytes. The text's
// own ratio of characters to bytes converts one into the other, which is exact
// for ASCII and keeps a text in a script of two- or three-byte characters
// from being asked for two or three times the room it has.
func askFor(bytes int, sample string) length {
	chars := bytes
	if n := len(sample); n > 0 {
		chars = max(bytes*utf8.RuneCountInString(sample)/n, 1)
	}
	return length{chars: chars, words: max(chars/charsPerWord, 1)}
}

// length is what one completion is asked for, in the units it can see.
type length struct {
	chars int
	words int
}

// complete runs one completion and returns its visible answer.
//
// THE TEXT IS FENCED UNDER A TOKEN MINTED FOR THIS CALL ALONE (ADR-0040), and
// the answer is neutralised under it: a rewrite reads text somebody else wrote
// as often as not — a thread, a page, a task — and what it writes back lands
// where that text was going, a seat's prompt among them. It used to be
// bracketed by a FIXED marker, `<<<TEXT` and `TEXT>>>`, which is exactly the
// delimiter the record rejects: a text that spelled the closer and then an
// instruction had stepped outside the text it was being rewritten from.
func (p *pass) complete(ctx context.Context, text string, ask length, retry string) (string, error) {
	call, cancel := context.WithTimeout(ctx, p.c.timeout)
	defer cancel()
	p.calls.Add(1)
	fence := provenance.NewBoundary()
	completion, err := p.member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt(p.req.Kind, p.req.Focus, ask)},
			{Role: llm.RoleUser, Content: userPrompt(fence, p.req.Label, text, retry)},
		},
		// NO TOOLS: a rewrite is text back, and a tool on the surface
		// invites a model to call it and answer nothing. NO CAP either:
		// the length is the prompt's to state (systemPrompt), which is
		// the one channel the model reads, and [pass.rewrite] is what
		// holds a model that answers longer to the budget.
	})
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrUnavailable, p.member.Key, err)
	}
	if completion == nil {
		return "", fmt.Errorf("%w: %s returned nothing", ErrUnavailable, p.member.Key)
	}
	answer := strings.TrimSpace(completion.Content)
	if answer == "" {
		return "", fmt.Errorf("%w: %s answered nothing visible (%d output tokens — "+
			"a model that spent its output reasoning)",
			ErrUnavailable, p.member.Key, completion.OutputTokens)
	}
	if !utf8.ValidString(answer) {
		// A backend that decoded its own stream badly. Refused rather
		// than passed on, because every reader after this one would
		// render the damage as replacement characters.
		return "", fmt.Errorf("%w: %s answered invalid UTF-8", ErrUnavailable, p.member.Key)
	}
	// NOTHING IT ECHOES OPENS OR CLOSES A FENCE wherever the caller puts it:
	// the call's own token is removed and every marker-shaped run escaped.
	return provenance.Neutralise(fence, answer), nil
}

// Split divides text into pieces of at most limit bytes, preferring a
// paragraph boundary, then a line, then a space, and splitting a run with
// none of those on a rune boundary.
//
// NOTHING IS DROPPED: the pieces concatenate back to the input exactly. This
// is how a compaction reads an input larger than one call can hold, and it is
// exported because a test of any caller's map-reduce needs the same pieces.
func Split(text string, limit int) []string {
	if limit <= 0 || len(text) <= limit {
		return []string{text}
	}
	var out []string
	for len(text) > limit {
		cut := cutAt(text, limit)
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// cutAt is where the next piece of text ends, given len(text) > limit: after
// the last paragraph break inside the limit, else the last newline, else the
// last space — each only when it keeps at least half the piece, so a boundary
// near the start does not produce a sliver — else the last rune boundary.
func cutAt(text string, limit int) int {
	head := text[:limit]
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(head, sep); i >= limit/2 {
			return i + len(sep)
		}
	}
	// text[cut] is the first byte of the NEXT piece, so the piece ends on
	// a boundary exactly when that byte starts a rune. At most three steps:
	// a UTF-8 encoding is four bytes at its longest.
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut == 0 {
		// No rune starts anywhere in the limit: the text is not UTF-8.
		// Taken whole rather than looping forever.
		return limit
	}
	return cut
}

func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}

// cacheKey is the request's identity: kind, budget, focus, label and text. The
// seat is NOT in it — the input is the seat's own and the rewrite is derived
// from nothing else, so two seats reading one thread share one rewrite.
func cacheKey(req Request) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\x00%s\x00", req.Kind, req.Budget, req.Focus,
		req.Label.Source, req.Label.Trust)
	for _, a := range req.Label.Authors {
		fmt.Fprintf(h, "%s:%s\x00", a.Kind, a.Seat)
	}
	h.Write([]byte{0})
	h.Write([]byte(req.Text))
	return hex.EncodeToString(h.Sum(nil))
}
