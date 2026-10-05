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
// # Cost
//
// Every rewrite is a completion on the seat's own auxiliary chain, resolved
// through the same metered seam every learning worker uses, so it is charged
// to the seat's token windows like any other auxiliary call. Three things keep
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

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

var log = logging.Get("compact")

// Models is the seat-model seam a compaction resolves its model through: the
// metered phase registry the learning workers and the prefetch already use,
// so a rewrite is charged exactly as any other auxiliary call is.
type Models interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
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
	// few dozen bytes, which no model can honour and which would make every
	// first pass fail as over budget. A piece is given at least this much,
	// and a joined first pass that is then over the whole budget is
	// compacted once more as a whole — so the floor costs one more call,
	// never a cut.
	minPieceBytes = 2 << 10

	// maxDepth bounds how many reduce passes a compaction may take.
	//
	// Each pass shrinks its input by at least the ratio between
	// [ChunkBytes] and [minPieceBytes], so three passes take MaxChunks
	// chunks down to one. A fourth would only be reached by a model that
	// keeps answering longer than it was asked to, and that is
	// [ErrOverBudget], not a reason to keep spending.
	maxDepth = 3

	// temperature is zero because the same input must produce the same
	// rewrite: the cache keys on the input, and a block whose compacted
	// form moved between two renders of one turn would invalidate a
	// provider's prompt cache on every round.
	temperature = 0.0

	// thinkingHeadroom is added to every completion's token cap.
	//
	// A thinking model spends its cap reasoning before it writes anything,
	// and an answer cut off there comes back EMPTY — which reads as a model
	// that refused rather than one that ran out of room. The visible answer
	// is at most the budget; this is the room to get there.
	thinkingHeadroom = 2048
)

var (
	// ErrUnavailable is no rewrite at all: no auxiliary model resolves for
	// the seat, or the call failed or answered nothing.
	ErrUnavailable = errors.New("compact: no auxiliary model could rewrite the text")

	// ErrOverBudget is a model that answered, twice, with a rewrite still
	// larger than the budget.
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
	// KindReport is a coding agent's report on the run it finished.
	KindReport Kind = "report"
)

// Kinds is every kind this build has instructions for.
var Kinds = []Kind{KindConversation, KindThread, KindArgument, KindToolError,
	KindProduced, KindSource, KindTask, KindAnswer, KindReport}

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

// For binds the compactor to one seat, which is the shape a caller inside a
// turn holds: every rewrite it asks for is that seat's.
func (c *Compactor) For(seat *org.Role) Bound { return Bound{c: c, seat: seat} }

// Bound is a compactor bound to one seat. Its zero value — and one bound
// from a nil compactor — rewrites nothing.
type Bound struct {
	c    *Compactor
	seat *org.Role
}

// Fit is [Compactor.Fit] for the bound seat.
func (b Bound) Fit(ctx context.Context, kind Kind, text string, budget int) (Result, error) {
	return b.c.Fit(ctx, Request{Seat: b.seat, Kind: kind, Text: text, Budget: budget})
}

// Focused is [Compactor.Fit] for the bound seat with a question the text is
// read to answer — a [KindSource] document.
func (b Bound) Focused(ctx context.Context, kind Kind, text string, budget int, focus string) (Result, error) {
	return b.c.Fit(ctx, Request{Seat: b.seat, Kind: kind, Text: text, Budget: budget, Focus: focus})
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
	member, err := c.models.Head(req.Seat, phase.Auxiliary)
	if err != nil {
		return Result{From: from}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	run := &pass{c: c, req: req, member: member}
	text, err := run.fit(ctx, req.Text, req.Budget, 0)
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
			rewritten, err := p.rewrite(gctx, chunk, per)
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
	return p.fit(ctx, strings.Join(parts, "\n\n"), budget, depth+1)
}

// rewrite asks the model for one rewrite of text within budget, and asks
// once more — from its own over-long answer — when the first misses.
func (p *pass) rewrite(ctx context.Context, text string, budget int) (string, error) {
	answer, err := p.complete(ctx, text, budget, "")
	if err != nil {
		return "", err
	}
	if len(answer) <= budget {
		return answer, nil
	}
	// ONE RETRY, FROM THE ANSWER rather than the original: it is already
	// most of the way there and a fraction of the input's size, so the
	// second call is cheap — and a model told by how much it missed hits
	// a limit far more reliably than one asked the same thing twice.
	retry := fmt.Sprintf("Your previous rewrite was %d characters and the limit is %d. "+
		"Rewrite it again, shorter, keeping every identifier.", len(answer), budget)
	second, err := p.complete(ctx, answer, budget, retry)
	if err != nil {
		return "", err
	}
	if len(second) > budget {
		return "", fmt.Errorf("%w: %s asked for %s answered %s, then %s",
			ErrOverBudget, p.member.Key, size(budget), size(len(answer)), size(len(second)))
	}
	return second, nil
}

// complete runs one completion and returns its visible answer.
func (p *pass) complete(ctx context.Context, text string, budget int, retry string) (string, error) {
	call, cancel := context.WithTimeout(ctx, p.c.timeout)
	defer cancel()
	p.calls.Add(1)
	completion, err := p.member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt(p.req.Kind, p.req.Focus, budget)},
			{Role: llm.RoleUser, Content: userPrompt(text, retry)},
		},
		// NO TOOLS: a rewrite is text back, and a tool on the surface
		// invites a model to call it and answer nothing.
		Temperature: llm.Temp(temperature),
		// Tokens run at about four bytes each in prose; a third of the
		// budget in tokens is room for the answer, and the headroom is
		// room for a thinking model to get there.
		MaxTokens: budget/3 + thinkingHeadroom,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrUnavailable, p.member.Key, err)
	}
	if completion == nil {
		return "", fmt.Errorf("%w: %s returned nothing", ErrUnavailable, p.member.Key)
	}
	answer := strings.TrimSpace(completion.Content)
	if answer == "" {
		return "", fmt.Errorf("%w: %s answered nothing visible (%d output tokens "+
			"against a cap of %d — a thinking model that spent the cap reasoning)",
			ErrUnavailable, p.member.Key, completion.OutputTokens, budget/3+thinkingHeadroom)
	}
	if !utf8.ValidString(answer) {
		// A backend that decoded its own stream badly. Refused rather
		// than passed on, because every reader after this one would
		// render the damage as replacement characters.
		return "", fmt.Errorf("%w: %s answered invalid UTF-8", ErrUnavailable, p.member.Key)
	}
	return answer, nil
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

// cacheKey is the request's identity: kind, budget, focus and text. The seat
// is NOT in it — the input is the seat's own and the rewrite is derived from
// nothing else, so two seats reading one thread share one rewrite.
func cacheKey(req Request) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%s\x00", req.Kind, req.Budget, req.Focus)
	h.Write([]byte(req.Text))
	return hex.EncodeToString(h.Sum(nil))
}
