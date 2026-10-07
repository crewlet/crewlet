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

// # Sending a backlog: one PASS at a time, and what is remembered between
//
// A caller with a backlog to embed — the node holding a seat filling the
// vectors its diary and its episodes lack after a model change — sends it a
// PASS at a time, one tick of its own loop, and every pass is held to what the
// provider answered by four rules. [Pass] is them, written once, so the two
// tables a seat's memory fills share one implementation rather than two copies
// that drift.
//
//   - THE PASS FORMS EVERY REQUEST. A provider call that fails fails whole, and
//     a caller handed one error for a hundred and twenty-eight inputs cannot
//     tell the one the model will never accept from the rest. So inputs are
//     grouped through the provider's own packing rule ([Limits.Requests], the
//     one rule) into calls that each fit ONE request, and a refused call is a
//     request whose inputs the pass knows. Every input is embedded WHOLE, as
//     [EmbedWhole] would embed it: cut into [Chunks] at the model's bound, the
//     pieces sent together and their vectors pooled, so a long ask or a long
//     account is read to its end — and an input too long for one request
//     goes in a call of its own.
//   - A REFUSAL ([ErrRefused]) IS SPLIT until the input it refuses is alone: a
//     refused call's inputs may be sent again only in calls of at most half its
//     size, so one input among n costs at most 1 + 2·⌈log₂n⌉ requests, fifteen
//     for a full call of [PassBatch] — and one more, the canary below, when
//     the refusal is the first answer the pass has had. The halves are sent
//     in the order they were made — both halves of a split before either
//     half's own — so the inputs a refusal did not concern are accepted
//     before the pass reaches any input alone. And the bound each input may
//     be sent in is REMEMBERED ([Refusals]) rather than held by this pass: a
//     pass that runs out of requests half way through an isolation leaves
//     the next pass to resume it, rather than to start again at the full
//     size and spend its own requests on the same halves.
//   - AN INPUT REFUSED ALONE IS HELD BACK for [RefusalRetry], costing no
//     request, and then offered again alone. A changed text is a new input
//     (it is remembered by the digest of what was sent) and is offered at
//     once.
//   - ANY OTHER FAILURE ENDS THE PASS — a rate limit, a timeout, a server
//     down, a credential or model refused ([ErrConfiguration]), a
//     cancellation, an answer this package could not read — because it is a
//     fact about the provider rather than about an input, and every request
//     after it would meet it too. The caller's next pass is the retry.
//
// And a pass is BOUNDED in what it SENDS — its requests and the prepared bytes
// of its inputs — never in what came of them: a refused request was still a
// request, and still bytes against the account's minute. A pass that cannot
// afford its next call is spent, and the next pass sends it.
//
// # When the refusal is the configuration's
//
// A provider that refuses a SETTING — a `dimensions` the model does not take,
// a model the endpoint does not serve at that width — refuses every request
// whatever its inputs, and answers it as a refusal. Isolating "the input" would
// then hold every input back one by one, each a request of its own once an hour,
// for ever. So the first refusal a pass meets before it has had anything
// accepted is JUDGED, by the one request that tells the two apart: a CANARY
// ([canary]), one plain word sent alone, which a provider refusing an input for
// what it says has no reason to refuse and a provider refusing its
// configuration refuses like everything else. Accepted, the pass is PROVEN —
// what it meets is refused for being what it is, and is isolated as above.
// Refused, the configuration is: the pass ends with [ErrRefusedWhole], records
// nothing against the inputs whose refusal was not theirs, and every pass under
// that memory is held back for [RefusalRetry]; the first refusal after the
// pause is judged again, so a configuration that is still refused costs two
// requests an hour and one that was fixed is embedding again within the hour.
//
// THE CANARY, NEVER THE INPUTS, because the inputs a fill meets are not a
// sample of anything: in steady state the only rows left without a vector are
// the ones whose embed failed when they were written, which is exactly where a
// poison input is. A rule concluding from what the inputs did — "two inputs
// refused alone while nothing was accepted" — took two poison notes met first
// by a fresh memory, after any boot or apply, for a refused configuration,
// blamed providers.embeddings and stopped the node's whole fill for an hour,
// and again after it. And PER PASS, never once per memory: what proves a
// configuration accepted is an acceptance NOW, a minute's worth of evidence
// rather than whatever the provider took when the process started, so a
// setting the endpoint stops honouring mid-life is concluded at the first
// pass that meets it rather than isolated input by input. A pass that has had
// anything accepted needs no canary, so a provider that works costs none: the
// canary is spent only on a pass whose first answer was a refusal.

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

	// Bytes is what was sent of it: its prepared text.
	Bytes int

	// Retry says it had been refused alone before, and this was its due
	// retry rather than its first isolation.
	Retry bool

	// Err is the provider's answer.
	Err error
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
	// Accepted the inputs it embedded; Unusable the inputs of an accepted
	// request whose answer had no direction to pool; Refused the requests
	// of inputs refused; Canaries the canaries it sent to judge one; Held
	// the inputs it passed over because they were refused alone inside the
	// last [RefusalRetry].
	Requests, Bytes, Accepted, Unusable, Refused, Canaries, Held int

	// RefusedAlone are the inputs refused sent alone, in the order they were.
	RefusedAlone []PassRefusal

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
	bytes  int

	// limit is the largest call it may be sent in, 0 for any; retry says
	// it is the due retry of an input refused alone.
	limit int
	retry bool
}

// Embed sends inputs, in order, under the pass, and hands each accepted call's
// inputs and vectors to store — positionally, a nil vector for an input with
// nothing to embed.
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
	suspects, fresh := p.queue(inputs, limits)
	for p.Open() && (len(suspects) > 0 || len(fresh) > 0) {
		from := &fresh
		if len(suspects) > 0 {
			from = &suspects
		}
		call, requests, bytes, err := p.form(*from, limits)
		if err != nil {
			// UNREACHABLE: every piece is cut inside the model's own
			// bound and the limits were validated when the provider was
			// built. Said rather than assumed, and it ends the pass like
			// any failure that is not about an input.
			p.err = fmt.Errorf("embeddings: plan a pass's request: %w", err)
			break
		}
		if len(call) == 0 {
			p.spent = true
			break
		}
		call = append([]queued(nil), call...)
		*from = (*from)[len(call):]

		var (
			pieces []string
			spans  = make([][2]int, len(call))
		)
		for i, q := range call {
			spans[i] = [2]int{len(pieces), len(pieces) + len(q.pieces)}
			pieces = append(pieces, q.pieces...)
		}
		raw, err := e.EmbedBatch(ctx, pieces)
		if err == nil && len(raw) != len(pieces) {
			// A SHORT ANSWER IS A RE-FILING, never a partial result:
			// vectors are matched to inputs by position. Unclassified,
			// so it ends the pass.
			err = fmt.Errorf("embeddings: %s answered %d vectors for %d pieces",
				e.Model(), len(raw), len(pieces))
		}
		p.requests -= requests
		p.bytes -= bytes
		p.Requests += requests
		p.Bytes += bytes
		p.sent = true

		switch {
		case err == nil:
			p.proven = true
			p.refusals.accept(call)
			// ONE INPUT'S DEGENERATE ANSWER COSTS THAT INPUT: a text whose
			// pieces' vectors cannot be pooled keeps no vector, and its
			// neighbours keep theirs ([wholeOf]).
			vectors := wholeOf(raw, pieces, spans, e.Width())
			accepted := make([]PassInput, len(call))
			for i, q := range call {
				accepted[i] = q.in
				if vectors[i] != nil {
					p.Accepted++
				} else {
					p.Unusable++
				}
			}
			if stored := store(accepted, vectors); stored != nil {
				p.err = stored
				return stored
			}
		case errors.Is(err, ErrRefused):
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
			p.refuse(call, err, &suspects)
		default:
			p.err = err
		}
	}
	return nil
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
		Input: q.in, Bytes: q.bytes, Retry: q.retry, Err: err,
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
// the ones with nothing to embed.
func (p *Pass) queue(inputs []PassInput, limits Limits) (suspects, fresh []queued) {
	for _, in := range inputs {
		pieces := Chunks(in.Text, limits.InputBytes)
		if len(pieces) == 0 {
			continue
		}
		q := queued{in: in, key: keyOfInput(in), pieces: pieces}
		for _, piece := range pieces {
			q.bytes += len(piece)
		}
		switch standing, limit := p.refusals.standingOf(q.key, p.now); standing {
		case refusalHeld:
			p.Held++
			continue
		case refusalDue:
			q.limit, q.retry = 1, true
			suspects = append(suspects, q)
		case refusalSuspect:
			q.limit = limit
			suspects = append(suspects, q)
		default:
			fresh = append(fresh, q)
		}
	}
	return suspects, fresh
}

// form is the next call from the front of from: the longest run that fits one
// request, the size every member may be sent in and what is left of the pass —
// and its requests and bytes. An empty call is the pass unable to afford even
// the first input; the first input of a pass is always affordable, so an input
// larger than a whole pass's bytes is still sent, in a pass of its own.
func (p *Pass) form(from []queued, limits Limits) ([]queued, int, int, error) {
	if len(from) == 0 {
		return nil, 0, 0, nil
	}
	first := from[0]
	alone, err := limits.Requests(first.pieces)
	if err != nil {
		return nil, 0, 0, err
	}
	if p.sent && (len(alone) > p.requests || first.bytes > p.bytes) {
		return nil, 0, 0, nil
	}
	call, bytes := from[:1], first.bytes
	if len(alone) > 1 {
		// TOO LONG FOR ONE REQUEST, so it is pooled from pieces sent
		// across several and goes in a call of its own.
		return call, len(alone), bytes, nil
	}
	size := min(PassBatch, limits.BatchInputs)
	if first.limit > 0 {
		size = min(size, first.limit)
	}
	pieces := append([]string(nil), first.pieces...)
	for _, next := range from[1:] {
		if len(call) >= size || (next.limit > 0 && len(call)+1 > next.limit) {
			break
		}
		if bytes+next.bytes > p.bytes {
			break
		}
		groups, err := limits.Requests(append(pieces, next.pieces...))
		if err != nil {
			return nil, 0, 0, err
		}
		if len(groups) > 1 {
			break
		}
		pieces = append(pieces, next.pieces...)
		call = from[:len(call)+1]
		bytes += next.bytes
	}
	return call, 1, bytes, nil
}

// wholeOf is each input's vector from its pieces' vectors, positionally: an
// input of one piece takes that piece's vector as the provider answered it —
// the vector [Embedder.Embed] gives the same text wherever the provider answers
// an array as it answers a string — and an input of several takes their [Pool],
// the vector [EmbedWhole] gives it. An input whose pieces cannot be pooled (a
// piece's vector with no direction) keeps a nil vector, and costs only itself.
func wholeOf(vectors [][]float32, pieces []string, spans [][2]int, width int) [][]float32 {
	out := make([][]float32, len(spans))
	for i, span := range spans {
		start, end := span[0], span[1]
		if end-start == 1 {
			out[i] = vectors[start]
			continue
		}
		weights := make([]int, 0, end-start)
		for _, piece := range pieces[start:end] {
			weights = append(weights, len(piece))
		}
		if pooled, err := Pool(vectors[start:end], weights, width); err == nil {
			out[i] = pooled
		}
	}
	return out
}

// keyOfInput is what the refusal memory knows an input by: its scope, its id
// and the digest of the text that would be sent for it.
func keyOfInput(in PassInput) refusalKey {
	sum := sha256.Sum256([]byte(Prepare(in.Text)))
	return refusalKey{scope: in.Scope, id: in.ID, sha: hex.EncodeToString(sum[:])}
}

// Refusals is what ONE provider configuration refused, held across the passes
// sent to it: the size each input of an unfinished isolation may be sent in,
// when an input was refused alone, and until when a configuration the [canary]
// found refused is left alone — see [Pass].
//
// THIS NODE'S ALONE, and in memory: a cache of what one provider told one
// caller, whose loss costs one more isolation of each input it held. Nothing
// about it is a fact a fleet must agree on. A caller builds a new one when the
// provider is rebuilt, because a refusal is a fact about the provider as it was
// configured.
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
	// of a call it was in; 1 once it was refused alone.
	limit int

	// alone is when it was last refused alone, zero while it is only a
	// suspect.
	alone time.Time

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

	// refusalHeld is an input refused alone inside the last
	// [RefusalRetry]: not sent.
	refusalHeld

	// refusalDue is an input refused alone longer ago than that: offered
	// again, alone.
	refusalDue
)

// NewRefusals is an empty memory, for one provider.
func NewRefusals() *Refusals { return &Refusals{at: map[refusalKey]*refusal{}} }

// Len is how many inputs the memory holds something about.
func (r *Refusals) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.at)
}

// standingOf answers what the memory says about k at now, and the limit of a
// suspect — and records that a pass met it.
func (r *Refusals) standingOf(k refusalKey, now time.Time) (refusalStanding, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.at[k]
	if !ok {
		return refusalNone, 0
	}
	entry.seen = now
	switch {
	case entry.alone.IsZero():
		return refusalSuspect, entry.limit
	case now.Sub(entry.alone) < RefusalRetry:
		return refusalHeld, 1
	}
	return refusalDue, 1
}

// split records that a call holding these inputs was refused: each may be sent
// again only in calls of at most limit.
func (r *Refusals) split(call []queued, limit int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range call {
		entry, ok := r.at[q.key]
		if !ok {
			entry = &refusal{}
			r.at[q.key] = entry
		}
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
	entry, ok := r.at[k]
	if !ok {
		entry = &refusal{}
		r.at[k] = entry
	}
	entry.limit, entry.alone, entry.seen = 1, now, now
}

// conclude records that a canary found the configuration refused at now:
// every pass is held back for [RefusalRetry].
func (r *Refusals) conclude(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pausedUntil = now.Add(RefusalRetry)
}

// accept forgets what the memory held about a call's inputs — the provider
// embedded them after all.
func (r *Refusals) accept(call []queued) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range call {
		delete(r.at, q.key)
	}
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
// memory for the life of the process.
func (r *Refusals) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, entry := range r.at {
		if now.Sub(entry.seen) >= 2*RefusalRetry {
			delete(r.at, k)
		}
	}
}
