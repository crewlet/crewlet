package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// GATHERS: one read across several partitions, answered at a cut, with what
// did not answer NAMED.
//
// # The plan
//
//  1. Resolve the partitions the read addresses.
//  2. Choose a holder for each: this node where it serves the partition,
//     otherwise the node that answered last for it, then a rendezvous order,
//     with a node that went silent last — the order a single-partition read
//     asks in ([Router.order]).
//  3. Batch the partitions by holder: ONE request per holder carrying all of
//     its partitions ([request.Partitions], [request.Slices]). The partitions
//     this node serves are answered in-process, concurrently, at most
//     [runtime.GOMAXPROCS] at a time.
//  4. A partition its holder failed is asked of its next holder, within the
//     caller's deadline — and NEVER again of a holder that already failed it
//     in this gather: a node that was silent, behind or not serving a moment
//     ago is the least likely of all to answer now, and asking it again is
//     how a gather spends its whole deadline on one dead node.
//  5. Merge with the operation's own merge — its own sort key, and for a
//     paged list each partition's own cursor ([mergePaged]).
//
// # A gather of ONE partition is a single-partition read
//
// It asks for the operation's WHOLE answer rather than a slice of it, at the
// level the read itself names — exactly the request a single-partition read
// sends, and the one a build that predates gathers sends and answers. Under
// layout 0 every gather is one, so nothing about a running fleet's reads
// changes until a layout divides a domain.
//
// # Never a short list
//
// A partition that did not answer is a [statelog.MissingPartition] on the
// answer's [statelog.Coverage], with the reason the holders gave — and when
// NOTHING answered, the read is an error rather than an empty answer, as a
// single-partition read is: an empty list would say the company has none of
// what was asked for.

// PartResult is one partition's answer to a gather, or why it has none.
type PartResult[P any] struct {
	Partition statelog.PartitionID
	Value     P

	// At is where each of the partition's logs was when its read began —
	// see [statelog.Coverage.At].
	At []statelog.Position

	// Cursor is the partition's own cursor this page was read from, in a
	// paged gather: empty for its first page.
	Cursor string

	// Missing is why the partition has no Value, and nil when it answered.
	Missing *statelog.MissingPartition
}

// gatherOp is a typed handle on one registered gather: its declaration, and
// the halves the router calls in the caller's own types.
type gatherOp[A, P, R any] struct {
	spec       *opSpec
	partitions partitionsFunc[A]
	merge      func(args A, parts []PartResult[P]) (R, error)
	hooks      *gatherHooks[A]
}

// gatherHooks are what a gather declares beyond its halves, held where the
// server half's closures can read them — set once, at declaration.
type gatherHooks[A any] struct {
	level  *gatherLevel[A]
	paging *gatherPaging[A]
}

// gatherLevel is how a gather's per-partition level reaches its arguments, for
// an operation that reads its domain's log at a level ([gatherOp.leveled]).
type gatherLevel[A any] struct {
	// domain is the domain whose log in each partition the level's
	// position is on — the floor a `session` slice waits for, the barrier
	// a `linearizable` one is read after.
	domain string

	of func(A) statelog.ReadLevel
	at func(args A, level statelog.ReadLevel, floor statelog.Position) A
}

// gatherPaging is how a paged gather's cursor reaches its arguments: the
// caller's gathered cursor, and each partition's own put back in its place.
type gatherPaging[A any] struct {
	cursor func(A) string
	with   func(args A, own string) A
}

// defineGather declares a gather: an operation that addresses several
// partitions, serves each partition's P, and merges them into R. Package-level,
// at init, like [define].
func defineGather[A, P, R any](name string, partitions partitionsFunc[A],
	serve func(ctx context.Context, b Backend, p statelog.PartitionID, args A) (P, error),
	merge func(args A, parts []PartResult[P]) (R, error),
) gatherOp[A, P, R] {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("estate: operation %q declared twice", name))
	}
	spec := &opSpec{
		name: name, class: opGatherRead, covered: true,
		args: reflect.TypeFor[A](), result: reflect.TypeFor[R](), part: reflect.TypeFor[P](),
	}
	hooks := &gatherHooks[A]{}
	decode := func(raw json.RawMessage) (A, error) {
		var args A
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return args, fmt.Errorf("estate: %s: decode the arguments: %w", name, err)
			}
		}
		return args, nil
	}
	spec.partitions = func(ctx context.Context, l statelog.Layout, r Resolver,
		raw json.RawMessage) ([]statelog.PartitionID, error) {
		args, err := decode(raw)
		if err != nil {
			return nil, err
		}
		return partitions(ctx, l, r, args)
	}
	slice := func(ctx context.Context, b Backend, s sliceAsk, args A) (P, []statelog.Position, error) {
		var zero P
		if hooks.paging != nil {
			args = hooks.paging.with(args, s.cursor)
		}
		streams := streamsOf(s.layout, s.partition)
		var at []statelog.Position
		if level := hooks.level; level != nil && s.level != "" {
			own, _ := s.layout.Stream(statelog.LogID{Domain: level.domain, Partition: s.partition})
			switch s.level {
			case statelog.ReadLinearizable:
				var through statelog.Position
				var err error
				if at, through, err = barriers(ctx, b, streams, own); err != nil {
					return zero, nil, err
				}
				// AT OR AFTER THE BARRIER, and not a second one: the
				// barrier was appended and applied here, so the read
				// is a `session` read floored at it.
				args = level.at(args, statelog.ReadSession, through)
			case statelog.ReadSession:
				args = level.at(args, statelog.ReadSession, floorOn(s.floors, own))
			default:
				args = level.at(args, s.level, statelog.Position{})
			}
		}
		if at == nil && !spec.floorless {
			at = appliedAt(b, streams)
		}
		v, err := serve(ctx, b, s.partition, args)
		return v, at, err
	}
	spec.slice = func(ctx context.Context, b Backend, s sliceAsk, raw json.RawMessage) (
		any, []statelog.Position, error) {
		args, err := decode(raw)
		if err != nil {
			return nil, nil, err
		}
		return slice(ctx, b, s, args)
	}
	spec.whole = func(ctx context.Context, b Backend, s sliceAsk, raw json.RawMessage) (
		any, []statelog.Position, error) {
		args, err := decode(raw)
		if err != nil {
			return nil, nil, err
		}
		v, at, err := slice(ctx, b, s, args)
		if err != nil {
			return nil, at, err
		}
		out, err := merge(args, []PartResult[P]{{
			Partition: s.partition, Value: v, At: at, Cursor: s.cursor,
		}})
		return out, at, err
	}
	registry[name] = spec
	return gatherOp[A, P, R]{spec: spec, partitions: partitions, merge: merge, hooks: hooks}
}

// leveled declares that this gather reads its domain's log at a level its
// arguments carry: of reads it, at sets the level each partition is read at
// and the position it is floored at ([statelog.GatherLevel]).
func (g gatherOp[A, P, R]) leveled(domain string, of func(A) statelog.ReadLevel,
	at func(A, statelog.ReadLevel, statelog.Position) A) gatherOp[A, P, R] {
	g.hooks.level = &gatherLevel[A]{domain: domain, of: of, at: at}
	return g
}

// paged declares that this gather is a paged list: cursor reads the caller's
// gathered cursor ([encodeGatherCursor]) and with sets one partition's own.
func (g gatherOp[A, P, R]) paged(cursor func(A) string, with func(A, string) A) gatherOp[A, P, R] {
	g.hooks.paging = &gatherPaging[A]{cursor: cursor, with: with}
	return g
}

// floorless declares that this gather carries no session floor — see
// [opSpec.floorless].
func (g gatherOp[A, P, R]) floorless() gatherOp[A, P, R] {
	g.spec.floorless = true
	return g
}

// barriers establishes a barrier on each of a partition's logs that keeps a
// read index and waits for this copy to apply through it: the cut a
// `linearizable` slice is answered at, and the barrier on own — the log the
// read itself reads.
func barriers(ctx context.Context, b Backend, streams []string, own string) (
	at []statelog.Position, through statelog.Position, err error) {

	if b.Barrier == nil {
		return nil, statelog.Position{}, errors.New("estate: this copy cannot establish a " +
			"barrier, so it cannot answer a linearizable gather")
	}
	for _, stream := range streams {
		pos, kept, err := b.Barrier(ctx, stream)
		switch {
		case err != nil:
			return nil, statelog.Position{}, err
		case !kept:
			// A LOG WITH NO READ INDEX makes no freshness claim: where
			// this copy has applied it is all the cut can say of it.
			if b.Applied != nil {
				pos = b.Applied(stream)
			}
		}
		if pos.Stream == "" {
			continue
		}
		at = append(at, pos)
		if stream == own {
			through = pos
		}
	}
	return at, through, nil
}

// appliedAt is where this copy has applied each of streams — the cut a read
// beginning now is answered at or after.
func appliedAt(b Backend, streams []string) []statelog.Position {
	if b.Applied == nil {
		return nil
	}
	var out []statelog.Position
	for _, stream := range streams {
		if pos := b.Applied(stream); pos.Stream != "" {
			out = append(out, pos)
		}
	}
	return out
}

// floorOn is the floor among floors on stream, or the zero position.
func floorOn(floors []statelog.Position, stream string) statelog.Position {
	for _, f := range floors {
		if f.Stream == stream {
			return f
		}
	}
	return statelog.Position{}
}

// corpusOf is the partition function of a search over one source domain's
// corpus: every partition carrying that domain's log AND the vectors kept
// beside its rows — the partitions a search over it reads. Under layout 0 that
// is `estate.000`.
//
// THE VECTORS DECIDE IT because they are co-located with their source: a
// partition carrying a domain's log and no vectors (the company space's
// catalogue) holds nothing a search ranks.
func corpusOf[A any](domain string) partitionsFunc[A] {
	return func(_ context.Context, l statelog.Layout, _ Resolver, _ A) ([]statelog.PartitionID, error) {
		var out []statelog.PartitionID
		for _, log := range l.LogsOf(domain) {
			for _, other := range l.Logs(log.Partition) {
				if other.Domain == vectorsDomain {
					out = append(out, log.Partition)
					break
				}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: layout %d keeps no index of the %s domain anywhere",
				ErrUnaddressed, l.Number, domain)
		}
		return out, nil
	}
}

// gather runs a gather from this node: every partition it addresses, each
// answered by a node that serves it, merged — and what it covered.
//
// surface is who is asking, which decides the level each partition of a
// gather that reads at a level is answered at ([statelog.GatherLevel]); a
// gather that reads at none takes the empty surface.
//
// THE COVERAGE IS ANSWERED EVEN WITH AN ERROR, naming every partition missing:
// a best-effort caller that answers empty on a failure still says the search
// did not reach what it did not reach.
func gather[A, P, R any](ctx context.Context, r *Router, o gatherOp[A, P, R],
	surface statelog.Surface, args A) (R, statelog.Coverage, error) {

	var zero R
	spec := o.spec
	layout, err := r.placement.Layout()
	if err != nil {
		return zero, statelog.Coverage{}, fmt.Errorf("estate: %s: read which layout the fleet runs: %w",
			spec.name, err)
	}
	parts, err := o.resolve(ctx, layout, args)
	if err != nil {
		return zero, statelog.Coverage{}, fmt.Errorf("estate: %s: %w", spec.name, err)
	}
	cursors := map[statelog.PartitionID]string{}
	if paging := o.hooks.paging; paging != nil {
		if gathered := paging.cursor(args); gathered != "" {
			own, cursorErr := decodeGatherCursor(gathered)
			if cursorErr != nil {
				return zero, statelog.Coverage{}, fmt.Errorf("estate: %s: %w", spec.name, cursorErr)
			}
			// A PARTITION THE CURSOR DOES NOT NAME HAS NO MORE ROWS: the
			// page that minted it took its last one.
			parts = slices.DeleteFunc(parts, func(p statelog.PartitionID) bool {
				_, more := own[p]
				return !more
			})
			cursors = own
		}
	}
	var level statelog.ReadLevel
	if lv := o.hooks.level; lv != nil && len(parts) > 1 {
		if !surface.Valid() {
			return zero, statelog.Coverage{}, fmt.Errorf("estate: %s reads at a level and "+
				"was asked from no surface, which is what decides it", spec.name)
		}
		level = statelog.GatherLevel(surface, lv.of(args), len(parts))
	}
	if len(parts) == 0 {
		// EVERY PARTITION THE CURSOR NAMED HAS RUN OUT: the page past the
		// last, which is empty and complete.
		out, mergeErr := o.merge(args, nil)
		return out, statelog.Coverage{}, mergeErr
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return zero, statelog.Coverage{}, fmt.Errorf("estate: %s: encode the arguments: %w", spec.name, err)
	}
	plan := gatherPlan{
		spec: spec, layout: layout, whole: len(parts) == 1, level: level,
		args: encoded, cursors: cursors,
		decode: func(raw json.RawMessage) (any, error) {
			if len(parts) == 1 {
				var out R
				if len(raw) == 0 {
					return out, nil
				}
				decodeErr := json.Unmarshal(raw, &out)
				return out, decodeErr
			}
			var out P
			if len(raw) == 0 {
				return out, nil
			}
			decodeErr := json.Unmarshal(raw, &out)
			return out, decodeErr
		},
	}
	states := r.runGather(ctx, plan, parts)
	cov, failure := coverageOf(states)
	if len(cov.Answered) == 0 {
		return zero, cov, failure
	}
	if plan.whole {
		out, _ := states[0].value.(R)
		return out, cov, nil
	}
	results := make([]PartResult[P], 0, len(states))
	for _, st := range states {
		res := PartResult[P]{Partition: st.p, At: st.at, Cursor: cursors[st.p], Missing: st.missing()}
		if res.Missing == nil {
			res.Value, _ = st.value.(P)
		}
		results = append(results, res)
	}
	out, err := o.merge(args, results)
	return out, cov, err
}

// resolve is the partitions a gather's arguments address under l, each once,
// in the layout's own order — and every one a partition the layout has.
func (o gatherOp[A, P, R]) resolve(ctx context.Context, l statelog.Layout, args A) (
	[]statelog.PartitionID, error) {

	parts, err := o.partitions(ctx, l, layoutResolver{layout: l}, args)
	if err != nil {
		return nil, err
	}
	var out []statelog.PartitionID
	for _, p := range l.Partitions() {
		if slices.Contains(parts, p) {
			out = append(out, p)
		}
	}
	for _, p := range parts {
		if !slices.Contains(out, p) {
			return nil, fmt.Errorf("%w: layout %d has no partition %s", ErrUnaddressed, l.Number, p)
		}
	}
	return out, nil
}

// gatherPlan is one gather as the router runs it.
type gatherPlan struct {
	spec   *opSpec
	layout statelog.Layout

	// whole is a gather of ONE partition, which asks for the operation's
	// whole answer — a single-partition read.
	whole bool

	// level is each slice's level, empty for a whole read and for an
	// operation that reads at none.
	level statelog.ReadLevel

	args    json.RawMessage
	cursors map[statelog.PartitionID]string

	// decode is a remote answer's value: the whole answer, or a slice.
	decode func(raw json.RawMessage) (any, error)
}

// partState is one partition's progress through a gather.
type partState struct {
	p statelog.PartitionID

	// done is set once the partition is settled: value and at where it
	// answered, err where the read ran there and failed.
	done  bool
	value any
	at    []statelog.Position
	err   error

	// tried is every holder that FAILED this partition in this gather,
	// this node included — never asked it again.
	tried map[string]bool

	// lagging is every holder whose copy said it lags its logs, asked
	// again with [request.AcceptLagging] once nobody better answered, and
	// laggingHere is this node's own copy saying so.
	lagging     []string
	laggingHere bool

	// overflow is the holder that answered this partition and could not
	// fit it in its reply: asked again, for this partition alone.
	overflow string

	// epoch is the map epoch the partition's holders were read at.
	epoch uint64

	// placement is why the placement could not say who serves it, if it
	// could not.
	placement error

	// what each holder said, and the strongest reason among it.
	reasons []string
	reason  statelog.MissingReason
}

func (s *partState) note(reason statelog.MissingReason, format string, args ...any) {
	s.reasons = append(s.reasons, fmt.Sprintf(format, args...))
	if missingRank[reason] > missingRank[s.reason] {
		s.reason = reason
	}
}

// missingRank orders the reasons a partition's holders give, so the one named
// is the closest the gather came: a holder that was there and BEHIND says more
// than one that did not serve it, which says more than one that did not answer,
// which says more than a placement naming nobody.
var missingRank = map[statelog.MissingReason]int{
	statelog.MissingUnserved:    1,
	statelog.MissingUnreachable: 2,
	statelog.MissingNotHolder:   3,
	statelog.MissingBehind:      4,
	statelog.MissingError:       5,
}

// missing is why this partition has no value, or nil when it answered.
func (s *partState) missing() *statelog.MissingPartition {
	if s.done && s.err == nil {
		return nil
	}
	m := &statelog.MissingPartition{Partition: s.p.String(), Reason: s.reason}
	switch {
	case s.err != nil:
		m.Reason, m.Detail = statelog.MissingError, s.err.Error()
	case s.placement != nil && len(s.reasons) == 0:
		m.Reason = statelog.MissingError
		m.Detail = fmt.Sprintf("read who serves %s: %v", s.p, s.placement)
	default:
		if m.Reason == "" {
			m.Reason = statelog.MissingUnserved
		}
		m.Detail = strings.Join(s.reasons, "; ")
		if m.Detail == "" {
			m.Detail = "no node serves it"
		}
	}
	return m
}

// failure is the error this partition's read ends in when NOTHING answered:
// the read's own error where it ran and failed, and otherwise the error a
// single-partition read gives.
func (s *partState) failure(op string) error {
	switch {
	case s.err != nil:
		return s.err
	case s.placement != nil && len(s.reasons) == 0:
		return fmt.Errorf("estate: %s: read who serves %s: %w", op, s.p, s.placement)
	}
	return &ErrPartitionUnserved{Partition: s.p.String(), Detail: strings.Join(s.reasons, "; ")}
}

// coverageOf is what the settled partitions covered, and — when nothing
// answered — the error the gather ends in: one partition's own, or every
// partition's joined.
func coverageOf(states []*partState) (statelog.Coverage, error) {
	cov := statelog.Coverage{Addressed: len(states), Answered: []string{}}
	var failures []error
	for _, st := range states {
		if m := st.missing(); m != nil {
			cov.Missing = append(cov.Missing, *m)
			failures = append(failures, st.failure(""))
			continue
		}
		cov.Answered = append(cov.Answered, st.p.String())
		for _, pos := range st.at {
			if cov.At == nil {
				cov.At = statelog.Cut{}
			}
			cov.At[pos.Stream] = pos
		}
	}
	if len(cov.Answered) > 0 {
		return cov, nil
	}
	if len(failures) == 1 {
		return cov, failures[0]
	}
	return cov, errors.Join(failures...)
}

// runGather settles every partition of a gather, in rounds: each round asks
// every unsettled partition of its next holder, one request per holder and this
// node's own partitions in-process, until each has answered, failed, or run out
// of holders — then asks the copies that said they lag, as a last resort.
func (r *Router) runGather(ctx context.Context, plan gatherPlan, parts []statelog.PartitionID) []*partState {
	states := make([]*partState, len(parts))
	for i, p := range parts {
		states[i] = &partState{p: p, tried: map[string]bool{}}
	}
	refreshed := false
	for ctx.Err() == nil {
		round := r.planRound(ctx, states)
		if round.empty() {
			break
		}
		refresh := r.runRound(ctx, plan, round, false)
		if refresh && !refreshed {
			// THE SERVER KNOWS A NEWER MAP: read ours again, once. The
			// partitions still unsettled read their holders afresh
			// next round, and a holder that failed one stays failed.
			refreshed = true
			if err := r.placement.Refresh(ctx); err != nil {
				for _, st := range states {
					if !st.done {
						st.note(statelog.MissingNotHolder, "refresh: %v", err)
					}
				}
			}
		}
	}
	// THE LAST RESORT: the copies that lag their logs, told to answer anyway.
	if ctx.Err() == nil {
		r.runRound(ctx, plan, r.lastResortRound(states), true)
	}
	if err := ctx.Err(); err != nil {
		for _, st := range states {
			if !st.done {
				st.note(statelog.MissingUnreachable, "the caller stopped waiting: %v", err)
			}
		}
	}
	return states
}

// gatherRound is one round's asks: this node's partitions, and each holder's
// batch.
type gatherRound struct {
	local   []*partState
	holders map[string][]*partState

	// alone is the partitions a holder answered and could not fit, each
	// asked of that holder by itself.
	alone []*partState
}

func (g gatherRound) empty() bool {
	return len(g.local) == 0 && len(g.holders) == 0 && len(g.alone) == 0
}

// planRound chooses each unsettled partition's next holder.
func (r *Router) planRound(ctx context.Context, states []*partState) gatherRound {
	round := gatherRound{holders: map[string][]*partState{}}
	for _, st := range states {
		switch {
		case st.done:
			continue
		case st.overflow != "":
			round.alone = append(round.alone, st)
			continue
		case !st.tried[r.self] && r.serves(ctx, st):
			round.local = append(round.local, st)
			continue
		}
		// THIS NODE DOES NOT SERVE IT, which is settled for the gather:
		// the holders the placement names are asked instead.
		st.tried[r.self] = true
		nodes, epoch, err := r.placement.Serving(st.p)
		if err != nil {
			st.placement = err
			continue
		}
		st.placement, st.epoch = nil, epoch
		for _, node := range r.order(st.p, nodes) {
			if !st.tried[node] {
				round.holders[node] = append(round.holders[node], st)
				break
			}
		}
	}
	return round
}

// serves reports whether this node serves st's partition — and notes it
// where it cannot tell, which is a holder that failed the partition.
func (r *Router) serves(ctx context.Context, st *partState) bool {
	if r.local == nil {
		return false
	}
	_, ok, unknown := r.local.For(ctx, st.p)
	if unknown != nil {
		st.tried[r.self] = true
		st.note(statelog.MissingUnserved, "%s: cannot tell whether it serves %s: %v",
			r.self, st.p, unknown)
	}
	return ok
}

// lastResortRound is the asks of the copies that said they lag, for the
// partitions nobody else answered.
func (r *Router) lastResortRound(states []*partState) gatherRound {
	round := gatherRound{holders: map[string][]*partState{}}
	for _, st := range states {
		if st.done {
			continue
		}
		if st.laggingHere {
			st.laggingHere = false
			round.local = append(round.local, st)
		}
		for _, node := range st.lagging {
			round.holders[node] = append(round.holders[node], st)
		}
		st.lagging = nil
	}
	return round
}

// runRound runs one round's asks concurrently and settles what they answer.
// It reports whether a holder named a map newer than the one this node routed
// by.
func (r *Router) runRound(ctx context.Context, plan gatherPlan, round gatherRound,
	acceptLagging bool) bool {

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		refresh bool
	)
	// THIS NODE'S OWN PARTITIONS, at most GOMAXPROCS at a time: each is a
	// read of a local file, so more at once only queues on the CPU.
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for _, st := range round.local {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.runLocalPart(ctx, plan, st, acceptLagging, &mu)
		}()
	}
	ask := func(node string, batch []*partState) {
		defer wg.Done()
		if r.askBatch(ctx, plan, node, batch, acceptLagging, &mu) {
			mu.Lock()
			refresh = true
			mu.Unlock()
		}
	}
	for node, batch := range round.holders {
		wg.Add(1)
		go ask(node, batch)
	}
	for _, st := range round.alone {
		node := st.overflow
		st.overflow = ""
		wg.Add(1)
		go ask(node, []*partState{st})
	}
	wg.Wait()
	return refresh
}

// runLocalPart answers one partition from this node's own copy, through the
// same gates a remote holder puts in front of it.
//
// THE READ RUNS WITHOUT THE ROUND'S LOCK, which guards only the settlement:
// another partition's answer must not wait on this one's file.
func (r *Router) runLocalPart(ctx context.Context, plan gatherPlan, st *partState,
	acceptLagging bool, mu *sync.Mutex) {

	b, ok, unknown := r.local.For(ctx, st.p)
	var out partRun
	if unknown == nil && ok {
		b.ServerSeams = r.seams
		ask := sliceAsk{partition: st.p, layout: plan.layout, level: plan.level,
			floors: r.floorsFor(plan.spec, plan.layout, st.p), cursor: plan.cursors[st.p]}
		out = runPart(ctx, r.self, plan.spec, b, ask, plan.args, plan.whole, acceptLagging)
		r.session.Forget(out.obsolete...)
	}
	mu.Lock()
	defer mu.Unlock()
	st.tried[r.self] = true
	switch {
	case unknown != nil:
		st.note(statelog.MissingUnserved, "%s: cannot tell whether it serves %s: %v",
			r.self, st.p, unknown)
	case !ok:
		st.note(statelog.MissingNotHolder, "%s: does not serve %s", r.self, st.p)
	case out.reason == unservedLagging:
		st.laggingHere = true
		st.note(statelog.MissingUnserved, "%s: %s", r.self, out.detail)
	case out.reason != "":
		st.note(missingFor(out.reason), "%s: %s", r.self, out.detail)
	case out.err != nil:
		st.done, st.err = true, out.err
	default:
		st.done, st.value, st.at = true, out.value, out.at
	}
}

// askBatch asks one holder for every partition of batch in one request and
// settles each from its answer. It reports whether the holder named a newer
// map than the one a partition was routed by.
func (r *Router) askBatch(ctx context.Context, plan gatherPlan, node string, batch []*partState,
	acceptLagging bool, mu *sync.Mutex) bool {

	mu.Lock()
	names := make([]string, 0, len(batch))
	cursors := map[string]string{}
	var floors []statelog.Position
	var epoch uint64
	for _, st := range batch {
		names = append(names, st.p.String())
		if c, ok := plan.cursors[st.p]; ok {
			cursors[st.p.String()] = c
		}
		floors = append(floors, r.floorsFor(plan.spec, plan.layout, st.p)...)
		epoch = max(epoch, st.epoch)
	}
	mu.Unlock()
	if len(cursors) == 0 {
		cursors = nil
	}
	rep, answered, err := r.ask(ctx, node, request{
		Op: plan.spec.name, Args: plan.args, Partitions: names, MapEpoch: epoch,
		Floors: floors, From: r.self, AcceptLagging: acceptLagging,
		Slices: !plan.whole, Level: plan.level, Cursors: cursors,
	}, r.readBudget)

	mu.Lock()
	defer mu.Unlock()
	switch {
	case err != nil:
		// THE ASK COULD NOT BE MADE AT ALL — no other node would fare
		// better, so every partition of the batch fails here, with why.
		for _, st := range batch {
			st.tried[node] = true
			st.note(statelog.MissingError, "%s: %v", node, err)
		}
		return false
	case !answered:
		for _, st := range batch {
			st.tried[node] = true
			st.note(statelog.MissingUnreachable, "%s: no answer", node)
		}
		return false
	}
	r.session.Forget(named(floors, rep.Obsolete)...)
	if plan.whole {
		return r.settleWhole(plan, node, batch[0], rep)
	}
	if rep.Unserved != "" || rep.Err != nil || len(rep.Parts) == 0 {
		// THE WHOLE REQUEST WAS REFUSED — an operation this build does
		// not know, a request it could not decode — which says nothing
		// about any partition but that this node could not answer it.
		why := rep.Detail
		if rep.Err != nil {
			why = decodeError(rep.Err).Error()
		}
		for _, st := range batch {
			st.tried[node] = true
			st.note(missingFor(rep.Unserved), "%s: %s", node, why)
		}
		return false
	}
	newer := false
	for _, st := range batch {
		part, found := partNamed(rep.Parts, st.p.String())
		if !found {
			st.tried[node] = true
			st.note(statelog.MissingError, "%s: answered the batch without %s", node, st.p)
			continue
		}
		if r.settlePart(plan, node, st, part) {
			newer = true
		}
	}
	return newer
}

// settleWhole settles a one-partition gather from a whole answer — the
// reply a single-partition read gets.
func (r *Router) settleWhole(plan gatherPlan, node string, st *partState, rep reply) bool {
	return r.settlePart(plan, node, st, partReply{
		Partition: st.p.String(), Unserved: rep.Unserved, Epoch: rep.Epoch,
		Detail: rep.Detail, Result: rep.Result, Err: rep.Err, At: rep.At,
	})
}

// settlePart settles one partition from one holder's answer for it. It reports
// whether the holder named a newer map than the partition was routed by.
func (r *Router) settlePart(plan gatherPlan, node string, st *partState, part partReply) bool {
	switch part.Unserved {
	case "":
	case unservedOverflow:
		// ANSWERED, AND IT DID NOT FIT: asked again of the same holder,
		// alone — which is not a failure of that holder.
		st.overflow = node
		return false
	case unservedLagging:
		st.tried[node] = true
		st.lagging = append(st.lagging, node)
		st.note(statelog.MissingUnserved, "%s: %s", node, part.Detail)
		return false
	case unservedNotHolder:
		st.tried[node] = true
		st.note(statelog.MissingNotHolder, "%s: %s", node, part.Detail)
		return part.Epoch > st.epoch
	default:
		st.tried[node] = true
		st.note(missingFor(part.Unserved), "%s: %s", node, part.Detail)
		return false
	}
	r.markAnswered(st.p, node)
	if part.Err != nil {
		st.done, st.err = true, decodeError(part.Err)
		return false
	}
	value, err := plan.decode(part.Result)
	if err != nil {
		st.done = true
		st.err = fmt.Errorf("estate: %s: %s answered %s with a result this build cannot "+
			"decode: %w", plan.spec.name, node, st.p, err)
		return false
	}
	st.done, st.value, st.at = true, value, part.At
	return false
}

// partNamed is the part of parts for partition, if the reply holds one.
func partNamed(parts []partReply, partition string) (partReply, bool) {
	for _, p := range parts {
		if p.Partition == partition {
			return p, true
		}
	}
	return partReply{}, false
}

// missingFor is the missing-partition reason a holder's refusal amounts to.
func missingFor(reason unservedReason) statelog.MissingReason {
	switch reason {
	case unservedBehind:
		return statelog.MissingBehind
	case unservedNotHolder:
		return statelog.MissingNotHolder
	}
	return statelog.MissingUnserved
}

// partRun is one partition's read on a copy this node serves: the value and
// cut where it ran, the refusal where the copy could not take it, and the
// floors no holder can ever reach.
type partRun struct {
	value    any
	at       []statelog.Position
	err      error
	reason   unservedReason
	detail   string
	obsolete []statelog.Position
}

// runPart runs one partition of a gather on a copy this node serves — whole
// for a one-partition gather, a slice otherwise — behind the two gates a
// remote holder puts in front of any request ([ready]): the same function for
// a request this node answers and for its own router's in-process ask.
func runPart(ctx context.Context, self string, spec *opSpec, b Backend, s sliceAsk,
	raw json.RawMessage, whole, acceptLagging bool) partRun {

	reason, detail, obsolete := ready(ctx, self, spec, b, s.partition, s.floors,
		streamsOf(s.layout, s.partition), acceptLagging)
	out := partRun{reason: reason, detail: detail, obsolete: obsolete}
	if reason != "" {
		return out
	}
	run := spec.slice
	if whole {
		run = spec.whole
	}
	out.value, out.at, out.err = run(ctx, b, s, raw)
	switch {
	case errors.Is(out.err, errNoHalf):
		out.value, out.err = nil, nil
		out.reason, out.detail = unservedNoBackend, fmt.Sprintf("%s runs no native backend for %s",
			self, spec.name)
	case errors.Is(out.err, errNotAdmitting):
		out.value, out.err = nil, nil
		out.reason, out.detail = unservedNotEstablished, fmt.Sprintf("%s's copy of %s admits "+
			"no seat yet", self, s.partition)
	}
	return out
}

// ---- answering a gather batch ------------------------------------------ //

// answerSlices answers a gather batch: each partition the request names, from
// this node's copy where it serves it, each at most once, at most GOMAXPROCS at
// a time — and a reply that fits under the broker's ceiling, with the slices
// that did not fit answered [unservedOverflow] for the asker to ask again.
func (s server) answerSlices(ctx context.Context, spec *opSpec, req request) reply {
	out := reply{Node: s.self}
	layout, err := s.placement.Layout()
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s: read which layout the fleet runs: %w",
			req.Op, err))
		return out
	}
	parts := make([]partReply, len(req.Partitions))
	var obsolete []statelog.Position
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, name := range req.Partitions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			part, gone := s.answerPart(ctx, spec, layout, req, name)
			mu.Lock()
			parts[i] = part
			obsolete = append(obsolete, gone...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, gone := range obsolete {
		if !slices.Contains(out.Obsolete, gone.Stream) {
			out.Obsolete = append(out.Obsolete, gone.Stream)
		}
	}
	out.Parts = fitParts(s.self, s.ceiling, parts)
	return out
}

// answerPart is one partition's slice, as this node answers it.
func (s server) answerPart(ctx context.Context, spec *opSpec, layout statelog.Layout,
	req request, name string) (partReply, []statelog.Position) {

	out := partReply{Partition: name}
	p, err := statelog.ParsePartitionID(name)
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s: the request names %q: %w", req.Op, name, err))
		return out, nil
	}
	b, serves, unknown := s.local.For(ctx, p)
	switch {
	case unknown != nil:
		out.Unserved = unservedHoldingUnknown
		out.Detail = fmt.Sprintf("%s cannot tell whether it serves %s: %v", s.self, p, unknown)
		return out, nil
	case !serves:
		_, epoch, _ := s.placement.Serving(p)
		out.Unserved, out.Epoch = unservedNotHolder, epoch
		out.Detail = fmt.Sprintf("%s does not serve %s (asked at map epoch %d)", s.self, p, req.MapEpoch)
		return out, nil
	}
	b.ServerSeams = s.seams
	run := runPart(ctx, s.self, spec, b, sliceAsk{
		partition: p, layout: layout, level: req.Level, floors: req.Floors, cursor: req.Cursors[name],
	}, req.Args, false, req.AcceptLagging)
	switch {
	case run.reason != "":
		out.Unserved, out.Detail = run.reason, run.detail
	case run.err != nil:
		out.Err = encodeError(run.err)
	default:
		raw, err := json.Marshal(run.value)
		if err != nil {
			out.Err = encodeError(fmt.Errorf("estate: %s could not encode %s's answer to %s: %w",
				s.self, p, req.Op, err))
			break
		}
		out.Result, out.At = raw, run.at
	}
	return out, run.obsolete
}

// replyHeadroom is what a gather reply keeps free of its slices for the rest of
// its envelope — the node's id and the obsolete floors.
//
// FOUR KiB: the envelope beside the parts is a node id and at most one stream
// name per log of the batch's partitions, a few hundred bytes for every batch a
// layout can make; the margin is the difference between a reply measured to
// fit and one that does.
const replyHeadroom = 4 << 10

// fitParts is parts as a reply can carry them under ceiling bytes: in order,
// every slice that fits, and every one after the first that does not answered
// [unservedOverflow] instead — so the asker asks again for those alone. A
// slice too large to fit even alone is answered as the error that says so,
// naming its size: every reply answers at least one partition, so a batch
// always makes progress.
//
// MEASURED, not estimated: each part is encoded as the reply will encode it,
// because what a body costs on the wire is a property of its bytes rather than
// of its length — see [queue.ErrTooLarge].
func fitParts(self string, ceiling int, parts []partReply) []partReply {
	budget := ceiling - replyHeadroom
	used := 0
	for i := range parts {
		size := encodedSize(parts[i]) + 1
		if used+size <= budget {
			used += size
			continue
		}
		if used == 0 {
			// THE FIRST SLICE ALONE DOES NOT FIT: nothing smaller can be
			// sent for it, so it is answered as the error it is.
			parts[i] = partReply{Partition: parts[i].Partition, Err: encodeError(fmt.Errorf(
				"estate: %s's answer for %s is %d bytes, over the %d one reply carries — "+
					"narrow the read: %w", self, parts[i].Partition, size, budget, queue.ErrTooLarge))}
			used += encodedSize(parts[i]) + 1
			continue
		}
		parts[i] = partReply{Partition: parts[i].Partition, Unserved: unservedOverflow,
			Detail: fmt.Sprintf("%s answered %s and it did not fit beside the slices "+
				"before it", self, parts[i].Partition)}
		used += encodedSize(parts[i]) + 1
	}
	return parts
}

// encodedSize is how many bytes p encodes to.
func encodedSize(p partReply) int {
	raw, err := json.Marshal(p)
	if err != nil {
		return 0
	}
	return len(raw)
}
