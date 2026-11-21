package embeddings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A backlog is sent a PASS at a time, one tick of the caller's loop. [Pass]
// holds every pass to the rules here, written once so the two tables a seat's
// memory fills share one implementation.
//
//   - Every call is ONE request, packed by [Limits.Requests]; every input is
//     embedded WHOLE as [EmbedWhole] would, cut into [Chunks] and pooled.
//   - A refusal ([ErrRefused]) is SPLIT, both halves of a split before either
//     half's own, until the input is alone: at most 1 + 2·⌈log₂n⌉ requests (15
//     for a full [PassBatch]), plus the canary. The bound each input may be
//     sent in is REMEMBERED ([Refusals]), so an interrupted isolation resumes.
//   - An input refused alone is HELD BACK for [RefusalRetry] at no request,
//     then offered alone again; changed text is a new input (keyed by digest).

// Two more rules.
//
//   - An input answered with NO DIRECTION (zeros, a non-finite component, or
//     pieces whose directions cancel: no cosine, pool or store can use it) is
//     HELD BACK like one refused alone, its neighbours keeping their vectors.
//     Forgotten, it was read on every pass for ever.
//   - Any other failure ENDS THE PASS (rate limit, timeout, server down,
//     [ErrConfiguration], a foreign width, cancellation, an unreadable answer):
//     it is a fact about the provider, not an input, and the next request
//     would meet it too. The caller's next pass is the retry.

// A pass is BOUNDED in what it SENDS (requests and prepared bytes), never in
// what came of it: a refused request still counts. It overshoots the bytes by
// at most ONE PIECE, since its first call always carries one, so an allowance
// smaller than one input still moves. A pass that cannot afford its next call
// is spent.

// An input may need more requests than a pass has, more bytes than the
// account's minute, or more time than the caller's tick. Sent as one call it
// broke the pass's bound, and a call failing part way discarded every answered
// request. So an input that does not fit is sent A REQUEST AT A TIME and each
// answer's pieces are KEPT: their vectors summed in order ([pooling]) in the
// memory ([Refusals]) across calls and passes, then pooled at the last piece to
// the bit as one call would. A pass running out resumes at the next piece; a
// refused or directionless piece holds the input back with the sum kept. The
// memory keeps one vector-width sum per input, forgotten like all it holds
// about inputs no pass meets any more.

// A provider refusing a SETTING (a `dimensions` the model does not take, a
// model not served at that width) refuses every request, as a refusal;
// isolating "the input" would hold every input back, each costing a request an
// hour for ever. So the first refusal before anything was accepted is JUDGED by
// a CANARY ([canary]), one plain word sent alone. Accepted: the pass is PROVEN
// and isolates as above. Refused: the pass ends with [ErrRefusedWhole], records
// nothing against the inputs, and every pass under that memory is held back for
// [RefusalRetry]; the next refusal is judged again, so a still-refused
// configuration costs two requests an hour and a fixed one embeds within it.

// THE CANARY, NEVER THE INPUTS: a fill's inputs are not a sample, since in
// steady state the only rows without a vector are those whose embed failed,
// which is where a poison input is. Concluding from "two inputs refused alone
// while nothing was accepted" blamed providers.embeddings for two poison notes
// and stopped the fill for an hour. And PER PASS, not once per memory: proof is
// an acceptance NOW, so a setting the endpoint stops honouring mid-life is
// concluded at the first pass that meets it. A pass with anything accepted
// needs no canary.

// PassBatch is the most inputs one call of a pass carries.
//
// 128, the knowledge corpus's own batch (internal/search EmbedBatch) and for
// its reason: the round-trip amortisation has flattened by there, and a call
// carries no more than the model admits either — its input count and its
// request total ([Limits]) — so a call of long inputs is smaller.
const PassBatch = 128

// RefusalRetry is how long an input the provider refused ALONE is held back
// before it is offered again — alone — and how long a configuration the pass
// concluded is refused whole ([ErrRefusedWhole]) is left alone.
//
// AN HOUR, the corpus duty's figure (internal/search EmbedRefusalRetry) and its
// reasoning: held back, an input costs nothing; offered again, it costs one
// request and one log line, so an hour is one request against the sixty passes
// a minute's loop makes, while a provider that stops refusing (a gateway
// fixed, a rule relaxed) has it embedded within the hour with no restart.
const RefusalRetry = time.Hour

// ErrRefusedWhole ends a pass whose provider refused the [canary] — one plain
// word sent alone — as it had refused what the pass sent before it: the shape
// of a refused SETTING rather than of a refused input (see [Pass]).
var ErrRefusedWhole = errors.New("embeddings: the provider refuses every request " +
	"under this configuration, one plain word sent alone included — the refusal " +
	"is the configuration's (providers.embeddings), not any input's")

// canary is what judges a refusal: the input a pass sends alone when the first
// answer it has had is a refusal (see [Pass]).
//
// ONE PLAIN WORD OF FOUR BYTES. Plain, so a provider that refuses an input for
// what it says — a content rule, a byte it cannot tokenize — has nothing to
// refuse in it, and none of anybody's data leaves the node in it. Four bytes,
// because the smallest input bound [Limits.Validate] admits is utf8.UTFMax, so
// it is inside every model's bound and fits one request of any valid limits.
const canary = "note"

// PassInput is one text a pass embeds whole, and what it is called.
type PassInput struct {
	// Scope is what the input belongs to — a table and a seat — and ID is
	// its name there. Together with the digest of Text they are what the
	// refusal memory knows it by.
	Scope, ID string

	// Text is exactly what the input's vector is of.
	Text string
}

// PassRefusal is one input the provider refused sent alone.
type PassRefusal struct {
	Input PassInput

	// Bytes is the prepared bytes of its pieces: what sending the whole
	// input costs, however many requests carry it.
	Bytes int

	// Retry says it had been refused alone before, and this was its due
	// retry rather than its first isolation.
	Retry bool

	// Err is the provider's answer.
	Err error
}

// PassUnusable is one input whose answer had no direction to keep: the
// provider accepted the request and answered this input — or one of its
// pieces — with a vector of zeros or one that is not finite, or its pieces'
// directions cancelled (see [Pass]).
type PassUnusable struct {
	Input PassInput
	// Bytes is the prepared bytes of its pieces, as [PassRefusal.Bytes].
	Bytes int
	// Retry says an answer for it had no direction before, and this was its
	// due retry.
	Retry bool
	// Err says what had no direction: which piece, or the pool.
	Err error
}

// PassProgress is one input a pass embedded part of and left for the next pass
// to finish: a text longer than what the pass could send (see [Pass]).
type PassProgress struct {
	Input PassInput

	// Embedded is how many of its Pieces are embedded, by this pass and the
	// passes before it; Bytes is the prepared bytes of all of them.
	Embedded, Pieces, Bytes int
}

// Pass is one pass's allowance, and what became of what it sent.
//
// Not safe for concurrent use: a pass is one tick's sequence of calls.
type Pass struct {
	refusals *Refusals
	now      time.Time

	// requests and bytes are what is left of the pass's allowance.
	requests, bytes int

	sent  bool
	spent bool
	err   error

	// proven says a request of this pass was accepted — the [canary]'s
	// included — so a refusal it meets is the inputs', not the
	// configuration's.
	proven bool

	// Requests and Bytes are what the pass sent, the canary included;
	// Accepted the inputs it embedded; Refused the requests of inputs
	// refused; Canaries the canaries it sent to judge one; Held the inputs
	// it passed over because they were refused alone, or answered with no
	// direction, inside the last [RefusalRetry].
	Requests, Bytes, Accepted, Refused, Canaries, Held int

	// RefusedAlone are the inputs refused sent alone, in the order they were.
	RefusedAlone []PassRefusal

	// Unusable are the inputs of an accepted request whose answer had no
	// direction to keep, in the order they were answered: each is held back
	// for [RefusalRetry].
	Unusable []PassUnusable

	// Unfinished are the inputs this pass embedded part of and stopped
	// before finishing: the next pass sends the rest.
	Unfinished []PassProgress

	// Paused says the pass began held back: its memory concluded the
	// configuration is refused less than [RefusalRetry] ago. Concluded says
	// THIS pass reached that conclusion.
	Paused, Concluded bool
}

// NewPass opens a pass that may send at most requests provider requests and
// bytes prepared bytes, under the refusal memory r of the provider it will
// send to, at now.
//
// A pass whose memory concluded the configuration is refused less than
// [RefusalRetry] ago begins ended, with [ErrRefusedWhole] and Paused set.
func NewPass(r *Refusals, now time.Time, requests, bytes int) *Pass {
	if r == nil {
		panic("embeddings: NewPass: a pass needs the refusal memory of its provider")
	}
	r.expire(now)
	p := &Pass{refusals: r, now: now, requests: requests, bytes: bytes}
	if r.paused(now) {
		p.err, p.Paused = ErrRefusedWhole, true
	}
	return p
}

// Open reports whether the pass may send another call.
func (p *Pass) Open() bool {
	return p.err == nil && !p.spent && p.requests > 0 && p.bytes > 0
}

// Err is the failure that ended the pass — [ErrRefusedWhole], or the
// provider's own answer that was not a refusal — or nil.
func (p *Pass) Err() error { return p.err }

// queued is one input as a pass holds it.
type queued struct {
	in     PassInput
	key    refusalKey
	pieces []string

	// sizes are the pieces' prepared bytes, and total their sum: what
	// sending the whole input costs.
	sizes []int
	total int

	// begun is what earlier calls embedded of it — of this pass or an
	// earlier one, whose sum the memory kept — or nil for an input none
	// has; its pieces still to send are pieces[begun.pieces:], and rest is
	// their bytes. advanced says THIS pass sent some of it.
	begun    *pooling
	rest     int
	advanced bool

	// limit is the largest call it may be sent in, 0 for any; retry says
	// it is the due retry of an input refused alone, and again that an
	// answer for it had no direction before.
	limit int
	retry bool
	again bool
}

// next is the first of q's pieces still to send.
func (q *queued) next() int {
	if q.begun == nil {
		return 0
	}
	return q.begun.pieces
}

// call is what one request carries: whole inputs from the front of a queue, or
// the next pieces of the input at its front.
type call struct {
	members []queued

	// upTo is, for each member, where the pieces this call carries end — it
	// carries pieces[next():upTo] — which is every piece of a member sent
	// whole, and fewer of one sent a request at a time.
	upTo []int

	// bytes is the prepared bytes the call carries.
	bytes int
}

// Embed sends inputs, in order, under the pass, and hands store each input whose
// embedding it settled — positionally, with its vector, or a nil one for an
// input whose answer had no direction to keep ([Pass.Unusable]), which the
// memory holds back. An input embedded only in part by the time the pass stops
// is not handed over; the next pass that is offered it finishes it
// ([Pass.Unfinished]).
//
// It returns store's error, which ends the pass too: a store that refuses a
// write refuses the next one identically, and carrying on would spend the
// provider's answers on vectors nothing keeps. Every PROVIDER failure is the
// pass's ([Pass.Err]), never returned, so a caller tells "the provider said
// stop" from "this node could not keep what it was given".
func (p *Pass) Embed(ctx context.Context, e BatchEmbedder, inputs []PassInput,
	store func([]PassInput, [][]float32) error,
) error {
	limits := e.Limits()
	if err := limits.Validate(); err != nil {
		// UNREACHABLE: the limits were validated when the provider was
		// built. Said rather than assumed, and it ends the pass like any
		// failure that is not about an input.
		p.err = fmt.Errorf("embeddings: plan a pass's request: %w", err)
		return nil
	}
	width := e.Width()
	suspects, fresh := p.queue(inputs, limits, width)
	defer func() { p.unfinished(suspects, fresh) }()
	for p.Open() && (len(suspects) > 0 || len(fresh) > 0) {
		from := &fresh
		if len(suspects) > 0 {
			from = &suspects
		}
		c, err := p.form(*from, limits)
		if err != nil {
			// UNREACHABLE: every piece is cut inside the model's own
			// bound. Said rather than assumed, as above.
			p.err = fmt.Errorf("embeddings: plan a pass's request: %w", err)
			break
		}
		if len(c.members) == 0 {
			p.spent = true
			break
		}
		var (
			pieces []string
			spans  = make([][2]int, len(c.members))
		)
		for i, q := range c.members {
			spans[i] = [2]int{len(pieces), len(pieces) + c.upTo[i] - q.next()}
			pieces = append(pieces, q.pieces[q.next():c.upTo[i]]...)
		}
		raw, err := e.EmbedBatch(ctx, pieces)
		if err == nil && len(raw) != len(pieces) {
			// A SHORT ANSWER IS A RE-FILING, never a partial result:
			// vectors are matched to inputs by position. Unclassified,
			// so it ends the pass.
			err = fmt.Errorf("embeddings: %s answered %d vectors for %d pieces",
				e.Model(), len(raw), len(pieces))
		}
		if err == nil {
			err = widthsOf(raw, width, e.Model())
		}
		p.requests--
		p.bytes -= c.bytes
		p.Requests++
		p.Bytes += c.bytes
		p.sent = true

		switch {
		case err == nil:
			p.proven = true
			settled, vectors := p.settle(c, raw, spans, width, *from)
			*from = (*from)[len(settled):]
			if len(settled) == 0 {
				continue
			}
			if stored := store(settled, vectors); stored != nil {
				p.err = stored
				return stored
			}
		case errors.Is(err, ErrRefused):
			refused := append([]queued(nil), c.members...)
			*from = (*from)[len(refused):]
			p.Refused++
			if !p.proven && p.judge(ctx, e) == configurationRefused {
				// NOTHING IS RECORDED AGAINST THESE INPUTS: the refusal
				// was not theirs, and held back or halved they would
				// cost requests the pass after the pause need not spend.
				p.refusals.conclude(p.now)
				p.err, p.Concluded = ErrRefusedWhole, true
				break
			}
			// The inputs' refusal — proven, or not judged because the
			// canary could not be sent or met a failure of its own,
			// which ends the pass ([Pass.judge]). Taken as the inputs'
			// either way, because that is what costs least whichever it
			// was: the next pass judges its own first refusal.
			p.refuse(refused, err, &suspects)
		default:
			p.err = err
		}
	}
	return nil
}

// settle takes an accepted call's answer: every member it embedded whole, or
// finished, is settled — accepted with its vector and forgotten by the memory,
// or unusable with none and held back by it — and a member it carried only part
// of has what was embedded of it summed, kept in the memory for the next pass,
// and stays at the front of from for the rest. It returns the settled inputs
// and their vectors, which are the first len(settled) members of from.
func (p *Pass) settle(c call, raw [][]float32, spans [][2]int, width int,
	from []queued,
) ([]PassInput, [][]float32) {
	var (
		settled []PassInput
		vectors [][]float32
	)
	for i := range c.members {
		q := &from[i]
		answer := raw[spans[i][0]:spans[i][1]]
		vector, done, unusable := p.embedded(q, c.upTo[i], answer, width)
		if !done {
			// Only the front member of a call is ever sent in part,
			// and it is then the call's only member.
			break
		}
		if unusable != nil {
			p.refusals.noDirection(q.key, q.begun, p.now)
			p.Unusable = append(p.Unusable, PassUnusable{
				Input: q.in, Bytes: q.total, Retry: q.again, Err: unusable,
			})
		} else {
			p.refusals.forget(q.key)
			p.Accepted++
		}
		settled = append(settled, q.in)
		vectors = append(vectors, vector)
	}
	return settled, vectors
}

// embedded is what an accepted answer made of q (its pieces up to upTo) and
// whether that settles it: one piece takes the provider's vector as answered
// (what [Embedder.Embed] gives), several take the [Pool] of all of them, as
// [EmbedWhole] gives, summed across the calls that carried them.
//
// An answer with NO DIRECTION settles q with no vector and says why: a single
// piece of zeros or non-finite, the first such piece of several (q's begun is
// then the sum before it, so the retry resumes there), or pieces whose
// directions cancel, which begin again. It costs only q; its neighbours keep
// theirs. Not settled, what is summed is kept in the memory for the next pass.
func (p *Pass) embedded(q *queued, upTo int, answer [][]float32, width int) (
	vector []float32, done bool, unusable error,
) {
	from := q.next()
	if from == 0 && upTo == len(q.pieces) && len(q.pieces) == 1 {
		if _, err := directionOf(answer[0]); err != nil {
			return nil, true, fmt.Errorf("embeddings: the vector %w", err)
		}
		return answer[0], true, nil
	}
	sum := q.begun
	if sum == nil {
		sum = newPooling(width)
	}
	for i, piece := range answer {
		if err := sum.add(piece, q.sizes[from+i]); err != nil {
			// add sums nothing of the piece it refuses, so sum is
			// every piece before it.
			q.begun = sum
			return nil, true, err
		}
	}
	if upTo < len(q.pieces) {
		q.begun, q.rest, q.advanced = sum, q.rest-bytesOf(q.sizes[from:upTo]), true
		p.refusals.keep(q.key, sum, p.now)
		return nil, false, nil
	}
	vector, err := sum.vector()
	if err != nil {
		q.begun = nil
		return nil, true, err
	}
	return vector, true, nil
}

// widthsOf checks that every vector of an answer is width wide — the width the
// provider reports ([Embedder.Width]) and every pool of a pass is summed at.
//
// AN ANSWER OF ANOTHER WIDTH IS THE PROVIDER'S, never an input's: a model
// configured otherwise than the store was sized for answers every input so
// ([ErrConfiguration]), so it ends the pass like any failure that is not about
// an input. Judged one input at a time, as a pool refusing the piece, it would
// hold every input back for an hour for a fault none of them has. Every
// provider this build ships checks its answers already; this says the pass
// does not lean on it.
func widthsOf(raw [][]float32, width int, model string) error {
	for _, vector := range raw {
		if _, err := checkedWidth(vector, width, model); err != nil {
			return err
		}
	}
	return nil
}

// unfinished records, when the pass stops, the input at the front of either
// queue it embedded part of and did not finish.
func (p *Pass) unfinished(queues ...[]queued) {
	for _, queue := range queues {
		if len(queue) == 0 || !queue[0].advanced {
			continue
		}
		q := queue[0]
		p.Unfinished = append(p.Unfinished, PassProgress{
			Input: q.in, Embedded: q.next(), Pieces: len(q.pieces), Bytes: q.total,
		})
	}
}

// refuse records a refusal of call's inputs: a call of several is SPLIT, and
// queued behind the suspects already waiting so both halves of a split go
// before either half's own — the half the refusal did not concern is accepted
// before the pass reaches any input alone; an input alone is named, and held
// back for [RefusalRetry].
func (p *Pass) refuse(call []queued, err error, suspects *[]queued) {
	if len(call) > 1 {
		half := (len(call) + 1) / 2
		p.refusals.split(call, half, p.now)
		for i := range call {
			call[i].limit = half
		}
		*suspects = append(*suspects, call...)
		return
	}
	q := call[0]
	p.RefusedAlone = append(p.RefusedAlone, PassRefusal{
		Input: q.in, Bytes: q.total, Retry: q.retry, Err: err,
	})
	p.refusals.refuseAlone(q.key, p.now)
}

// judgement is what the [canary] said about a refusal.
type judgement int

const (
	// unjudged: the canary was not sent — the pass had no request left for
	// it — or met a failure that is not a refusal, which ends the pass.
	unjudged judgement = iota

	// inputsRefused: the canary was accepted, so the configuration is, and
	// the refusal was of the inputs sent.
	inputsRefused

	// configurationRefused: the canary was refused as well.
	configurationRefused
)

// judge sends the [canary] alone, charged to the pass like any request, and
// answers what it says about the refusal the pass just met.
func (p *Pass) judge(ctx context.Context, e BatchEmbedder) judgement {
	if p.requests < 1 {
		return unjudged
	}
	raw, err := e.EmbedBatch(ctx, []string{canary})
	if err == nil && len(raw) != 1 {
		err = fmt.Errorf("embeddings: %s answered %d vectors for 1 piece", e.Model(), len(raw))
	}
	p.requests--
	p.bytes -= len(canary)
	p.Requests++
	p.Bytes += len(canary)
	p.Canaries++
	p.sent = true
	switch {
	case err == nil:
		p.proven = true
		return inputsRefused
	case errors.Is(err, ErrRefused):
		return configurationRefused
	}
	p.err = err
	return unjudged
}

// queue sorts inputs into the two queues a pass sends from — the suspects of
// an earlier refusal, and everything else — dropping the inputs held back and
// the ones with nothing to embed, and taking up what an earlier pass embedded
// of each it began.
func (p *Pass) queue(inputs []PassInput, limits Limits, width int) (suspects, fresh []queued) {
	for _, in := range inputs {
		pieces := Chunks(in.Text, limits.InputBytes)
		if len(pieces) == 0 {
			continue
		}
		q := queued{in: in, key: keyOfInput(in), pieces: pieces, sizes: make([]int, len(pieces))}
		for i, piece := range pieces {
			q.sizes[i] = len(piece)
		}
		q.total = bytesOf(q.sizes)
		known := p.refusals.standingOf(q.key, p.now)
		if b := known.begun; b != nil && b.width == width && b.pieces < len(pieces) {
			// THE SAME TEXT UNDER THE SAME CONFIGURATION is the same
			// pieces, so what was summed of them is still theirs; any
			// other shape is a sum of something else, and the input
			// starts again.
			q.begun = b
		}
		q.rest = bytesOf(q.sizes[q.next():])
		q.again = known.unusable
		switch known.standing {
		case refusalHeld:
			p.Held++
			continue
		case refusalDue:
			q.limit, q.retry = 1, true
			suspects = append(suspects, q)
		case refusalSuspect:
			q.limit = known.limit
			suspects = append(suspects, q)
		default:
			fresh = append(fresh, q)
		}
	}
	return suspects, fresh
}

// form is the next call from the front of from: what one request carries and
// the pass has the bytes left for. An empty call is the pass unable to afford
// even one piece of the front input; the first call of a pass always carries at
// least one, so a pass whose whole allowance is smaller than one piece still
// sends it, and moves.
//
// WHOLE INPUTS where the front one's pieces still to send fit one request
// and the bytes left: it and the longest run behind it that fits too, at the
// size every member may be sent in. Otherwise the front input's NEXT PIECES —
// as many as one request takes and the bytes left cover — alone in the call.
func (p *Pass) form(from []queued, limits Limits) (call, error) {
	if len(from) == 0 {
		return call{}, nil
	}
	first := from[0]
	rest := first.pieces[first.next():]
	whole, err := fitsOneRequest(limits, rest)
	if err != nil {
		return call{}, err
	}
	if !whole || first.rest > p.bytes {
		return p.formPart(first, limits)
	}
	c := call{members: from[:1], upTo: []int{len(first.pieces)}, bytes: first.rest}
	size := min(PassBatch, limits.BatchInputs)
	if first.limit > 0 {
		size = min(size, first.limit)
	}
	pieces := append([]string(nil), rest...)
	for _, next := range from[1:] {
		if len(c.members) >= size || (next.limit > 0 && len(c.members)+1 > next.limit) {
			break
		}
		if c.bytes+next.rest > p.bytes {
			break
		}
		groups, err := limits.Requests(append(pieces, next.pieces[next.next():]...))
		if err != nil {
			return call{}, err
		}
		if len(groups) > 1 {
			break
		}
		pieces = append(pieces, next.pieces[next.next():]...)
		c.members = from[:len(c.members)+1]
		c.upTo = append(c.upTo, len(next.pieces))
		c.bytes += next.rest
	}
	return c, nil
}

// formPart is the call that carries the next pieces of first alone: as many as
// the packing rule puts in one request ([Limits.Requests]) and the pass's bytes
// cover — one at least on the pass's first call, and none, an empty call, on a
// later one that cannot afford one.
func (p *Pass) formPart(first queued, limits Limits) (call, error) {
	at := first.next()
	sizes := first.sizes[at:]
	groups, err := limits.requests(sizes[:min(len(sizes), limits.BatchInputs)])
	if err != nil {
		return call{}, err
	}
	take, bytes := 0, 0
	for take < len(groups[0]) && bytes+sizes[take] <= p.bytes {
		bytes += sizes[take]
		take++
	}
	if take == 0 {
		if p.sent {
			return call{}, nil
		}
		take, bytes = 1, sizes[0]
	}
	return call{members: []queued{first}, upTo: []int{at + take}, bytes: bytes}, nil
}

// fitsOneRequest reports whether pieces go in one request under limits.
func fitsOneRequest(limits Limits, pieces []string) (bool, error) {
	if len(pieces) > limits.BatchInputs {
		// A request carries at most that many inputs, and every piece
		// is one: no grouping is needed to say so.
		return false, nil
	}
	groups, err := limits.Requests(pieces)
	if err != nil {
		return false, err
	}
	return len(groups) == 1, nil
}

// bytesOf is the sum of sizes.
func bytesOf(sizes []int) int {
	total := 0
	for _, size := range sizes {
		total += size
	}
	return total
}

// keyOfInput is what the refusal memory knows an input by: its scope, its id
// and the digest of the text that would be sent for it.
func keyOfInput(in PassInput) refusalKey {
	sum := sha256.Sum256([]byte(Prepare(in.Text)))
	return refusalKey{scope: in.Scope, id: in.ID, sha: hex.EncodeToString(sum[:])}
}

// Refusals is what ONE provider configuration refused, held across the passes
// sent to it: the size each input of an unfinished isolation may be sent in,
// when an input was refused alone or answered with no direction, and until
// when a configuration the [canary] found refused is left alone (see [Pass]),
// plus what it has embedded of each unfinished text ([PassProgress]).
//
// THIS NODE'S ALONE, and in memory: a cache whose loss costs one more
// isolation per input and one more send of unfinished text; no fleet agrees on
// it. A caller builds a new one when the provider's model, width, limits or
// endpoint change (a refusal is a fact about the provider as configured).
//
// Safe for concurrent use.
type Refusals struct {
	mu sync.Mutex
	at map[refusalKey]*refusal

	// pausedUntil is when a pass may send again after a canary found the
	// configuration refused whole.
	pausedUntil time.Time
}

// refusalKey is what one refusal is remembered by.
type refusalKey struct{ scope, id, sha string }

// refusal is what the memory holds about one input.
type refusal struct {
	// limit is the largest call it may be sent in, halved at each refusal
	// of a call it was in; 1 once it was refused alone; 0 for an input that
	// was never in a refused call.
	limit int

	// alone is when it was last refused alone, zero while it is only a
	// suspect.
	alone time.Time

	// unusable is when an accepted answer for it last had no direction,
	// zero if none ever had. Cleared by a refusal alone, which is the
	// later fact, and by an answer that had one, which forgets the input.
	unusable time.Time

	// begun is what passes embedded of it, for a text a pass began and did
	// not finish, or nil. Kept when the input is refused, or a piece of it
	// is answered with no direction, so its retry resumes at that piece
	// rather than at the first.
	begun *pooling

	// seen is when a pass last met it, for [Refusals.expire].
	seen time.Time
}

// refusalStanding is what the memory says about one input now.
type refusalStanding int

const (
	// refusalNone is an input never refused: sent with its neighbours.
	refusalNone refusalStanding = iota

	// refusalSuspect is an input of a refused call not yet isolated: sent
	// only in calls of at most its limit.
	refusalSuspect

	// refusalHeld is an input refused alone, or answered with no
	// direction, inside the last [RefusalRetry]: not sent.
	refusalHeld

	// refusalDue is an input refused alone longer ago than that: offered
	// again, alone.
	refusalDue
)

// known is what the memory says about one input when a pass meets it.
type known struct {
	standing refusalStanding

	// limit is a suspect's largest call.
	limit int

	// begun is a copy of what was embedded of it, if a pass began it.
	begun *pooling

	// unusable says an answer for it had no direction before. Due — past
	// [RefusalRetry] — it is offered again with its neighbours: an answer
	// with no direction was ACCEPTED, so nothing about it puts the inputs
	// sent beside it at risk, as a refused input's neighbours are.
	unusable bool
}

// NewRefusals is an empty memory, for one provider.
func NewRefusals() *Refusals { return &Refusals{at: map[refusalKey]*refusal{}} }

// Len is how many inputs the memory holds something about.
func (r *Refusals) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.at)
}

// standingOf answers what the memory says about k at now, and records that a
// pass met it.
func (r *Refusals) standingOf(k refusalKey, now time.Time) known {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.at[k]
	if !ok {
		return known{standing: refusalNone}
	}
	entry.seen = now
	out := known{standing: refusalNone, unusable: !entry.unusable.IsZero()}
	if entry.begun != nil {
		out.begun = entry.begun.clone()
	}
	switch {
	case !entry.alone.IsZero() && now.Sub(entry.alone) < RefusalRetry:
		out.standing, out.limit = refusalHeld, 1
	case !entry.alone.IsZero():
		out.standing, out.limit = refusalDue, 1
	case out.unusable && now.Sub(entry.unusable) < RefusalRetry:
		out.standing = refusalHeld
	case entry.limit > 0:
		out.standing, out.limit = refusalSuspect, entry.limit
	}
	return out
}

// split records that a call holding these inputs was refused: each may be sent
// again only in calls of at most limit.
func (r *Refusals) split(call []queued, limit int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range call {
		entry := r.entry(q.key)
		if entry.limit == 0 || limit < entry.limit {
			entry.limit = limit
		}
		entry.seen = now
	}
}

// refuseAlone records k refused sent alone at now: it is held back for
// [RefusalRetry] and then offered again alone.
func (r *Refusals) refuseAlone(k refusalKey, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entry(k)
	entry.limit, entry.alone, entry.unusable, entry.seen = 1, now, time.Time{}, now
}

// noDirection records that an accepted answer for k had no direction at now:
// it is held back for [RefusalRetry] and then offered again with its
// neighbours. begun is what was summed of it before the piece that had none,
// kept so the retry resumes at that piece, or nil to begin again.
//
// ACCEPTED, so it is no suspect of any refusal: the size a refused call left it
// and the refusal alone it may have been the retry of are both over.
func (r *Refusals) noDirection(k refusalKey, begun *pooling, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entry(k)
	entry.limit, entry.alone, entry.unusable, entry.seen = 0, time.Time{}, now, now
	entry.begun = nil
	if begun != nil && begun.pieces > 0 {
		entry.begun = begun.clone()
	}
}

// keep records what passes have embedded of k so far, a copy of sum, for the
// pass that sends the rest of it.
func (r *Refusals) keep(k refusalKey, sum *pooling, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entry(k)
	entry.begun, entry.seen = sum.clone(), now
}

// entry is k's entry, made empty if the memory had none. The caller holds mu.
func (r *Refusals) entry(k refusalKey) *refusal {
	entry, ok := r.at[k]
	if !ok {
		entry = &refusal{}
		r.at[k] = entry
	}
	return entry
}

// conclude records that a canary found the configuration refused at now:
// every pass is held back for [RefusalRetry].
func (r *Refusals) conclude(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pausedUntil = now.Add(RefusalRetry)
}

// forget lets go of everything the memory held about k: the provider embedded
// it after all.
func (r *Refusals) forget(k refusalKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.at, k)
}

// paused reports whether passes are held back at now.
func (r *Refusals) paused(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.Before(r.pausedUntil)
}

// expire forgets every input no pass has met for twice [RefusalRetry]: one that
// was due an hour ago and was offered nowhere since is an input no caller
// selects any more — it was deleted, or it was embedded another way, or its
// text changed and it carries another digest — and keeping it would grow the
// memory for the life of the process. A text a pass began and no pass has
// offered since is the same case, and what was embedded of it goes with it.
func (r *Refusals) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, entry := range r.at {
		if now.Sub(entry.seen) >= 2*RefusalRetry {
			delete(r.at, k)
		}
	}
}
