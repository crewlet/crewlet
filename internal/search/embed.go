package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"

	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The embedding duty: ONE fleet singleton, one provider bill, N copies.
//
// # Why this is a duty and not a hook on the write path
//
// An embedding costs a provider call, and a write path that made one would put
// a third-party HTTP round trip inside the transaction that saves somebody's
// page. It would also charge the company once per NODE, for a value every node
// needs and nobody can compute differently. So it is a fleet singleton that
// publishes a record, and the record is what every node applies.
//
// # What it costs, from the constants below
//
// EmbedSourcesPerTick = 1 024 sources a tick, on a one-minute tick — across
// EVERY corpus rather than each, divided between them round robin by
// [Embedder.Tick], so the figures here are the company's and never one
// corpus's. A cold fill of 110 000 sources is ≈ 108 minutes and ≈ 860
// requests of 128 sources — more requests where the sources run long, since a
// request carries only what the model's request total admits, but the same
// sources, the same minutes and the same bill — billed once for the whole
// fleet; and those numbers do NOT move with the configured width, because the
// provider bills per input TOKEN and `dimensions` is a truncation parameter it
// already receives. The write rate IS a function of the width: a record is
// JSON carrying the vector as base64, ≈ 16.9 KB at 3 072 dimensions, so 1 024
// of them a minute is ≈ 290 KB/s, a factor of five under the pace the walk
// paths are held to.

const (
	// EmbedBatch is the most sources one provider REQUEST carries.
	//
	// 128, which is where the round-trip amortisation has essentially
	// flattened. A request carries no more than the MODEL admits either —
	// its own input count, and its request total counted over the inputs'
	// bytes ([embeddings.Limits]) — and the duty forms every request itself
	// through that one packing rule ([embeddings.Limits.Requests]), so a
	// request the model would refuse for its size is never formed: 128
	// short sources are one request, and sources at the full 8 KiB are 36 a
	// request under OpenAI's 300 000-token total.
	EmbedBatch = 128

	// EmbedSourcesPerTick bounds the sources one tick publishes a vector
	// for.
	//
	// 1 024, eight requests of [EmbedBatch]: the figure the cold-fill time
	// and the write rate above are written against, and the selection each
	// corpus is asked for. Withdrawals are outside it, as they always were
	// (see [Embedder.Tick]).
	//
	// It is also the FAIRNESS FLOOR. [Embedder.Tick] hands the requests out
	// one at a time, round robin over the corpora, each carrying at most
	// [EmbedBatch] sources — so each of N corpora is guaranteed ⌊8/N⌋
	// requests' worth however far behind its neighbours are, and
	// [NewEmbedder] refuses a wiring with more corpora than eight, because
	// below one full request apiece there is no per-tick share left to
	// guarantee.
	EmbedSourcesPerTick = 8 * EmbedBatch

	// EmbedRequestsPerTick bounds one tick's provider requests.
	//
	// THIRTY-TWO, and two needs agree on it.
	//
	//   - PACKING. A tick's 1 024 sources at the full 8 KiB are 29 requests
	//     under OpenAI's request total, so a tick of long sources still
	//     embeds its whole [EmbedSourcesPerTick] on the default model, and
	//     a model that takes fewer inputs a request is held to 32 of them
	//     rather than sent as many as its limits divide the tick into.
	//   - ISOLATION. A refused request is split until the input it refuses
	//     is alone, which costs at most 1 + 2·log₂128 = 15 requests for one
	//     input among 128; with two corpora behind, each is guaranteed 16,
	//     and its share of the tick's sources besides ([tickRequests.room]),
	//     so a refusal is isolated inside the tick it is met in. Where a
	//     corpus's share is smaller — three corpora or more — the isolation
	//     takes more than one tick, and each tick resumes it where the last
	//     one stopped ([Refusals.suspend]) rather than meeting it again from
	//     the whole request.
	//
	// It is also what bounds the tick's provider wall clock: every request
	// is held to its own ceiling (embeddings.BatchTimeout), and a request
	// that fails for any reason but a refusal ends the tick's requests
	// rather than being followed by thirty-one more that would meet the
	// same answer ([Embedder.Tick]).
	EmbedRequestsPerTick = 32

	// EmbedRefusalRetry is how long an input the provider refused ALONE is
	// held back before it is offered again — alone ([Refusals]).
	//
	// AN HOUR. Held back, the input costs no request and no place in the
	// selection, which passes over it ([Held]) — only the scan's step over
	// its key, and the coverage gauge counting it stale. Offered again, it
	// costs one request and one log line — so an hour is one request in the
	// 1 920 sixty ticks may make, and one warning an hour per input rather
	// than one a minute, while a provider that stops refusing it (a gateway
	// fixed, a content rule relaxed) has it embedded within the hour with
	// no restart, as long as its corpus holds no more refusals than an
	// hour's retries reach ([EmbedRetriesPerTick]). Shorter buys a quicker
	// retry of a refusal that is almost always permanent; longer leaves a
	// fixed one unsearchable by meaning for longer than an operator who
	// fixed it would expect.
	EmbedRefusalRetry = time.Hour

	// EmbedRetriesPerTick is how many of one corpus's DUE refusals a tick
	// offers again, each alone in a request of its own; the rest are held
	// one more tick, the longest refused first ([Refusals]).
	//
	// TWO, because a retry costs a whole request for one source and the
	// requests are what a corpus is guaranteed a share of: with the two
	// corpora shipped that share is sixteen and retries take an eighth of
	// it, and at the eight corpora [NewEmbedder] allows, half of four.
	// Uncapped, refusals that fall due together — every one a provider met
	// in one cold fill does, an hour later — took the corpus's whole share
	// at one request each, and nothing new in that corpus was embedded
	// until they had all been offered. The price is the retry's reach: 120
	// an hour a corpus, so a corpus holding more refusals than that offers
	// each of them less often than hourly — a corpus whose provider refuses
	// that many has a configuration to fix, and fixing it starts a new
	// memory with nothing held at all.
	EmbedRetriesPerTick = 2

	// EmbedInterval is how often the duty ticks.
	//
	// ONE MINUTE, which is the tick the whole arithmetic above is written
	// against: 1 024 sources a tick, so a cold fill of 110 000 sources is
	// about 108 minutes and a steady company's backlog is emptied within a
	// minute of the write that created it. It is also what bounds the WRITE
	// rate this duty puts on the vector log — 1 024 records of ≈ 16.9 KB a
	// minute is about 290 KB/s at a width of 3 072, a factor of five under
	// the pace the walk paths are held to.
	//
	// Faster would not embed anything sooner on a company that is caught
	// up (a tick with no stale source costs one indexed count and stops),
	// and on one that is behind it would raise the publish rate without
	// raising the provider's, which is the half that is actually slow.
	EmbedInterval = time.Minute

	// EmbedInputBytes is how much of ONE source the corpus embeds: the
	// opening of its prepared text — title and body, every run of
	// whitespace collapsed ([embeddings.Prepare]) — up to this many bytes,
	// or the model's own per-input bound where that is smaller
	// ([Embedder.inputBound]).
	//
	// 8 KiB, and two reasons hold it there, one from below and one from
	// above.
	//
	// FROM BELOW IT IS A REPRESENTATION, not a limit anybody imposed. One
	// vector stands for one source, and 8 KiB of English prose is about
	// 2 000 tokens — a quarter of OpenAI's 8 192-token window — which is
	// the span the ranking, the floor curve and every capacity figure in
	// this package were measured over. An opening that long already says
	// what a page or a task is about; a vector over much more of a long,
	// multi-topic page is a mean of its topics and matches none of them
	// well. What lies past the window is not lost to search — the keyword
	// half indexes the WHOLE body ([Indexer]) — only to a search by
	// meaning, and `crewlet search eval` reports how much of each corpus
	// that is ([Window]).
	//
	// FROM ABOVE IT IS THE MOST THAT IS PROVABLY INSIDE THE WINDOW without a
	// tokenizer. A tokenizer emits at most one token per byte of its input
	// (config.EmbeddingModels states the argument for each family), and
	// OpenAI's models take 8 192 tokens an input with nothing wrapped
	// around it — so 8 192 bytes is the largest opening that can never be
	// refused there, whatever the script. Raising it means counting tokens,
	// which this engine deliberately does not ship a tokenizer to do.
	//
	// CUT RATHER THAN REFUSED, the deliberate opposite of what this engine
	// does with a vendor-limited field a person wrote: no person or model
	// ever reads this text — it is machine input whose only output is a
	// vector — and refusing would remove the source from the semantic half
	// with nothing to say so.
	EmbedInputBytes = 8 << 10

	// embedReadChars is how much of a source's body a selection reads to
	// form its opening: twice [EmbedInputBytes], in CHARACTERS, because
	// that is what `substr` counts on a TEXT value in SQLite and in Turso.
	//
	// ENOUGH, because every character is at least one byte and collapsing
	// whitespace removes only whitespace: a prefix of 2·N characters of
	// which at most N are whitespace still prepares to at least N bytes,
	// so the opening reaches the full bound. A body whose first 2·N
	// characters are mostly whitespace — deeply indented code, a padded
	// table — gets a SHORTER opening than its whole would give, and the
	// same shorter opening every time, so its digest is as stable as any
	// other's.
	//
	// AND NO MORE, because the selection is asked for a whole tick's worth
	// of sources per corpus and a page holds up to 512 KiB: read whole,
	// 1 024 of them were up to half a gibibyte held to send 8 KiB of each.
	// Read like this, it is at most 64 KiB a source at four bytes a
	// character, and 16 KiB for prose.
	embedReadChars = 2 * EmbedInputBytes
)

// THERE IS NO STALL WINDOW HERE, deliberately, and the constant that used to
// declare one is gone rather than wired up.
//
// It named thirty minutes of no progress as the point an alarm fires, and no
// alarm read it — there is no progress timer on this duty at all, so the value
// promised a surface that did not exist. Wiring one is what the alarm table's
// own rule forbids: `recall_below_floor` already reports a duty that has
// stopped, from the coverage the corpora themselves are counted at, and a
// second threshold over the same event is a second opinion that drifts from
// the first. A duty that embeds nothing shows up as coverage that stops
// rising; a duty with nothing to embed shows up as coverage at one. A stall
// window cannot tell those apart, which is why the reading is the fraction and
// not the clock.

// # Two corpora, and the seam is why there was ever one
//
// [Corpus] had one implementation — the tracker's tasks — because
// `tracker_tasks` and `kb_vectors` are both in the replicated estate and the
// selection is one anti-join, while pages were this node's own projection and
// no statement may name a table in both. A page arm then would have been a
// bounded cursor sweep whose whole shape existed to work around that boundary.
// The pages domain removed it: `pages_heads` is written by an applier into the
// replicated estate, so [PageCorpus] is the same single anti-join and the duty
// itself did not change by a line — which is what this seam was for.

// Corpus is where the sources this duty embeds come from.
//
// DECLARED BY THE CONSUMER, which is this file, and kept to what the duty
// actually needs: the next batch of documents whose text has moved since the
// vector was computed. Each source kind implements it over its own tables, and
// both are one anti-join in one file today — the page arm was the one that had
// to cross the estate boundary, and the pages domain removed the boundary
// rather than the seam. Neither shape reaches the duty, which is what let that
// change cost this file nothing.
type Corpus interface {
	// Source is the kind this corpus supplies.
	Source() Source

	// Stale returns up to limit documents whose vector is missing or
	// computed from an older version, oldest first — each carrying the
	// digest of the vector it already has in the asked space
	// ([Document.StoredSHA]) — together with the sources whose vectors
	// must be FORGOTTEN because the document is gone.
	//
	// It PASSES OVER every source held holds, asking before it reads the
	// source's body, and the limit counts only what it returns: it reads
	// up to limit + held.Len() rows of the selection to return limit
	// ([Held] says why a filter after the limit is not the same thing).
	Stale(ctx context.Context, model string, dim, limit int, held Held) (stale []Document, gone []string, err error)

	// Coverage is how many of this corpus's sources carry a CURRENT
	// vector, and how many sources there are.
	//
	// THE SAME PREDICATE AS [Corpus.Stale], COUNTED RATHER THAN SELECTED,
	// and that is the whole requirement: a coverage number derived from a
	// second idea of what "current" means would disagree with the backlog
	// the duty is working through, and the alarm would fire against a
	// denominator nothing was ever going to fill.
	Coverage(ctx context.Context, model string, dim int) (current, total int, err error)
}

// Coverage is the fraction of every corpus's sources carrying a current
// vector, and whether there was anything to measure.
//
// FALSE RATHER THAN ZERO ON AN EMPTY CORPUS. A company that has written
// nothing down has no coverage, which is not the same fact as a company whose
// embeddings have stopped — and zero is the value the alarm fires on, so
// collapsing the two would page every fleet on its first day.
//
// IT SUMS ACROSS THE CORPORA rather than reporting the worst, because what the
// alarm is about is how much of what a search can reach is reachable by
// MEANING: a company whose tasks are fully embedded and whose pages are not is
// one whose searches are half-degraded, and the worst-of reading would call it
// wholly degraded while the average of two fractions would weight a
// twelve-page wiki equally with a hundred thousand tasks.
func Coverage(ctx context.Context, corpora []Corpus, model string, dim int) (float64, bool, error) {
	var current, total int
	for _, corpus := range corpora {
		have, all, err := corpus.Coverage(ctx, model, dim)
		if err != nil {
			return 0, false, err
		}
		current += have
		total += all
	}
	if total <= 0 {
		return 0, false, nil
	}
	// NEVER ABOVE ONE. The two counts come from one statement here, but a
	// corpus is free to answer from two — and a vector whose source was
	// removed between them would otherwise report a company as better than
	// completely covered, which reads as a broken gauge rather than as the
	// rounding it is.
	return min(float64(current)/float64(total), 1), true, nil
}

// Document is one source as the duty embeds it.
type Document struct {
	ID        string
	Container string

	// Version is the source's own version, stored beside the vector so
	// the next pass can tell a stale row from a current one without
	// re-embedding anything.
	//
	// IT MOVES WHENEVER THE BODY DOES — a task's version on every record,
	// a page's edit number on every save — and a held refusal is
	// recognised by it, with the title, before the body is read ([Held]):
	// a corpus whose version could stand still while the body moved would
	// hold a rewritten source back as the text it no longer is.
	Version uint64

	// Title is the source's whole title, and Body the OPENING of its body:
	// the first [embedReadChars] characters, which is all an opening of
	// [EmbedInputBytes] can need ([Document.text]).
	//
	// A TASK'S BODY IS NOT A COLUMN — it lives inside the encoded document,
	// deliberately, because nothing indexes it and a duplicate column costs
	// ≈ 200 MB a year. This duty reads it out with json_extract, which is
	// the one reader that needs the body without needing the rest of the
	// document: decoding every row to embed it would be the same 200 MB in
	// allocations per pass.
	Title string
	Body  string

	// StoredSHA is the text digest of the vector this source already has in
	// the space being embedded — the model and width the selection was asked
	// for — or "" when it has none there. A digest equal to the one its
	// text would be sent under says the stored vector IS this text's
	// vector, and the source is restamped rather than embedded again
	// ([Embedder.Tick]).
	StoredSHA string
}

// text is what is actually sent for this source: the [embeddings.Opening] of
// its title and body at bound — [Embedder.inputBound], the corpus's opening
// inside the model's own — which is PREPARED text, exactly the bytes the
// provider receives.
//
// PREPARED FIRST AND CUT SECOND. The provider collapses every run of
// whitespace before it sends anything ([embeddings.Prepare]), so a cut taken
// before that spent the bound on indentation and blank lines the model never
// saw, and a digest taken over it was a digest of bytes nobody embedded.
//
// THE TITLE AND THE BODY ARE SEPARATED BY ONE SPACE, which is what the model
// has always received: the preparation collapses whatever separated them —
// the blank line this used to join them with included — and every vector the
// corpus holds was computed from "Title Body…". A delimiter that survived the
// preparation (a colon, a full stop) would be a token in every vector, and it
// would change the text of every source while nothing selects an unchanged
// source again: the corpus would hold two representations side by side until
// each source happened to be edited.
//
// UNMARKED, because this is embedding INPUT that no reader sees, and an
// appended character would be a token in the vector rather than a note about
// one — the class textcut's package doc names, cut by the provider's own rule.
func (d Document) text(bound int) string {
	return embeddings.Opening(d.Title+" "+d.Body, bound)
}

// EmbedDeps is what the duty needs.
type EmbedDeps struct {
	// Publisher is the vector domain's write authority.
	Publisher *statelog.Publisher

	// Estate is the replicated estate the duty keeps an index for
	// (ADR-0028): the codes it trains on and the rows it reads the index's
	// state from. Read only — what the duty decides it publishes, and the
	// applier writes.
	Estate store.ReplicatedReader

	// Log names the vector log this duty writes, which is what the index's
	// training seed is derived from ([IVFSeed]) — so a re-run of a training
	// draws the same sample.
	Log string

	// Standing reads what the index step must know about the vector log
	// before it may publish onto it: whether this node has applied all of
	// it, and which build every node applying it reads ([LogStanding]).
	//
	// REQUIRED, never defaulted: a duty that could not ask would either
	// publish the index's records onto a log a node cannot read — stopping
	// that node's applier — or decide from rows a moment behind the log,
	// and there is no safe answer to assume in its place.
	Standing func(ctx context.Context) (LogStanding, error)

	// Embedder is the provider. A batch embedder is required rather than
	// preferred: one source at a time is 110 000 round trips for a cold
	// fill, which does not fit in the tick it runs on.
	Embedder embeddings.BatchEmbedder

	// Model is the embedding model id, which rides on every row so a
	// same-width model change can exclude the space it replaces.
	Model string

	// Corpora are the source kinds to embed.
	Corpora []Corpus

	// Refusals is what the provider has refused ALONE: the inputs held
	// back so that one the model will never accept costs a request an hour
	// and no place in the selection, rather than fifteen requests a tick
	// ([Refusals]).
	//
	// REQUIRED, and held by the caller ACROSS TICKS — the duty is rebuilt
	// every tick — for ONE provider CONFIGURATION: a caller starts a new
	// one whenever what the provider is configured as changes, because a
	// refusal is a fact about the provider as it was configured, and keeps
	// it when the provider is merely built again the same — an apply that
	// changed something else, or a rotated key — since each fresh memory
	// isolates every refused input again. Nil is refused rather than
	// defaulted to a memory that lasts one tick, which would isolate the
	// same input again at the front of every tick for ever.
	Refusals *Refusals

	// Logger is where a provider failure is reported. A failure to embed
	// is LOGGED AND CARRIED, never returned: a refused input costs only
	// itself, and anything else the provider answered ends this tick's
	// requests with the tick after it as the retry — a duty that failed
	// its whole tick over one would stop embedding the corpus because of
	// one document in it.
	//
	// Nil is this package's own `component=search` logger, NEVER a
	// discarding one: since a failure is only ever logged, a duty whose
	// logger went nowhere would fail every request with no trace at all.
	Logger *slog.Logger

	// Now is the clock, injected so a test can hold it.
	Now func() time.Time

	// Budget is the bound the caller holds the tick to, which the index's
	// steps show their progress to ([Budget]).
	//
	// REQUIRED, never defaulted to a bound nobody is told about: the duty's
	// caller bounds every tick, and one that forgot to say which bound
	// would measure a training by its length again — which on a one-core
	// node cut off every training at the largest corpus, so its index
	// was never built and the node spent five minutes of its only core on
	// it every tick, for ever.
	Budget Budget
}

// Budget is a tick's bound, as the tick's steps show it their progress.
//
// # A tick is bounded by its progress, never by its length
//
// The caller bounds a tick because a tick can WEDGE — a read that never
// returns, a provider that never answers — and a wedged tick holding the
// duty's lease embeds nothing for anybody. A wedge is the ABSENCE of progress,
// so that is what the bound measures, and every step of a tick shows it in the
// way it can. A step with a natural end reports as it ends ([Budget.Advanced]):
// a provider call answered, a vector published or withdrawn (one publish
// each) and a batch of the index's rollout published. The index's
// reading of every code and its exact pass stream rows, and say so every
// [progressStride] of them. The arithmetic — the
// k-means, filing every code, choosing the probe count — cannot wedge at all:
// it is pure computation over values in memory, each a bounded number of
// steps reading its context at least once a stride ([ivfStride]) or a probe
// count, so cancellation and a lost lease still stop it promptly; it runs
// EXEMPT ([Budget.Exempt]), whole, rather than reporting from inside functions
// that know nothing of a tick.
//
// Measured by its LENGTH instead, a training at the largest corpus an
// index serves never finished on a node allowed one core and sharing it with
// two searchers: its reading projects to three and a half minutes there
// (394 µs a source at ≈ 545 000) and its arithmetic to seven and a half more
// (BenchmarkIndexTraining and BenchmarkIVFTrainingShare at -cpu 1, pinned to
// one CPU), eleven in all against five, so every training such a node began
// was cut off and began again the next tick.
//
// # Why not exempt the arithmetic and keep a budget of time for the rest
//
// At that load the reading alone would have fitted five minutes, 1.4 times
// over. But a budget of time is a cliff that moves: the reading nearly doubled
// between an idle core and two searchers, it grows with the corpus, and
// the tick that trains also makes this tick's embedding requests, each with a
// one-minute ceiling of its own (embeddings.BatchTimeout) — a handful of them
// and the reading pass five minutes at the load measured. Progress has no such
// cliff: it measures the one thing the bound is for, and needs the exemption
// anyway.
type Budget interface {
	// Advanced says a bounded stretch of the tick's work is done.
	Advanced()

	// Exempt stops the bound's clock until the returned function is
	// called, which it must be exactly once, and whose call is progress.
	// Exemptions nest: the clock runs again when the last one open is
	// resumed.
	Exempt() (resume func())
}

// progressStride is how many rows one of the index's streaming reads covers
// between two reports of its progress ([Budget.Advanced]): ONE THOUSAND AND
// TWENTY-FOUR, the stride the arithmetic reads its context at ([ivfStride]).
//
// A report costs a lock and a clock read, under a millionth of the stride's
// rows at the fastest they have been read (about 190 µs a row for the exact
// pass, the costliest of the reading's parts, on an idle core); at the
// slowest — 331 µs, one core shared with two searchers (BenchmarkIndexTraining
// -cpu 1, pinned to one CPU) — a stride is a third of a second, so a read goes
// hundreds of strides inside the engine's budget before its silence could be
// taken for a wedge.
const progressStride = ivfStride

// unwatched is the progress report of a read no bound is watching — the
// evaluation an operator runs, and the tests and benchmarks that read the way
// the duty does.
func unwatched() {}

// Embedder is the duty.
type Embedder struct {
	deps EmbedDeps
}

// NewEmbedder builds the duty, refusing an incomplete wiring rather than
// starting and embedding nothing.
func NewEmbedder(d EmbedDeps) (*Embedder, error) {
	switch {
	case d.Publisher == nil:
		return nil, fmt.Errorf("search: the embed duty has no publisher")
	case d.Estate.IsZero():
		return nil, fmt.Errorf("search: the embed duty has no estate — it keeps " +
			"the corpus's semantic index, and trains it from the codes there")
	case d.Log == "":
		return nil, fmt.Errorf("search: the embed duty names no vector log — " +
			"the index's training seed is derived from it")
	case d.Standing == nil:
		return nil, fmt.Errorf("search: the embed duty cannot read the vector " +
			"log's standing — it publishes nothing about the index without " +
			"knowing this node has applied the log and every node reads the records")
	case d.Budget == nil:
		return nil, fmt.Errorf("search: the embed duty has no tick budget — " +
			"EmbedDeps.Budget is the bound its caller holds the tick to, which " +
			"the duty's steps show their progress to")
	case d.Embedder == nil:
		return nil, fmt.Errorf("search: the embed duty has no embedder")
	case d.Refusals == nil:
		return nil, fmt.Errorf("search: the embed duty has no refusal memory — " +
			"EmbedDeps.Refusals is held across ticks for one provider, and " +
			"without it an input the model refuses is isolated again at the " +
			"front of every tick")
	case d.Model == "":
		return nil, fmt.Errorf("search: the embed duty has no model id — it " +
			"rides on every row, and rows written without one cannot be " +
			"excluded when the model changes at the same width")
	case d.Embedder.Width() <= 0:
		return nil, fmt.Errorf("search: the embed duty's provider reports "+
			"width %d", d.Embedder.Width())
	case len(d.Corpora) == 0:
		return nil, fmt.Errorf("search: the embed duty has no corpus")
	case len(d.Corpora) > EmbedSourcesPerTick/EmbedBatch:
		// REFUSED RATHER THAN SERVED UNFAIRLY. A tick hands its requests
		// out round robin, each up to EmbedBatch sources against a budget
		// of EmbedSourcesPerTick, so with more corpora than eight full
		// requests the ones past the eighth can get none — not this tick
		// and not any tick, because every tick starts the round at the
		// front. That is the failure this whole file is written against:
		// a source kind silently unsearchable by meaning, with a coverage
		// gauge that sums the corpora reporting the company as merely
		// behind. Refusing says so at the wiring, which is where the
		// extra corpus was added.
		//
		// A CORPUS UNDER THE CEILING IS NOT FREE EITHER, and the bill
		// is in [Embedder.Tick]: every corpus is selected at the whole
		// tick's ceiling, so N of them read N x 1 024 documents — each
		// its title and the opening of its body — to embed 1 024. Read
		// that paragraph before adding the third.
		return nil, fmt.Errorf("search: the embed duty has %d corpora and a "+
			"tick embeds %d sources at most %d a request — every corpus must be "+
			"guaranteed at least one full request or the ones at the back of "+
			"EmbedDeps.Corpora are never embedded at all; raise "+
			"EmbedSourcesPerTick or embed fewer source kinds",
			len(d.Corpora), EmbedSourcesPerTick, EmbedBatch)
	}
	if d.Logger == nil {
		d.Logger = log
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Embedder{deps: d}, nil
}

// Tick embeds one tick's worth of sources and reports how many records it
// published.
//
// # The unit of work is a REQUEST, and the duty forms every one
//
// Each corpus's stale sources are cut into requests by the model's own packing
// rule ([embeddings.Limits.Requests], capped at [EmbedBatch] sources), and
// each request is sent through a call of its own — so a request the model
// would refuse for its size is never formed, and one it refuses all the same
// is a request whose inputs this duty knows. What follows from an answer is in
// the section above [pending]: a refusal is split until the input it refuses
// is alone, and that input is held back ([Refusals]).
//
// # A source whose text did not move is RESTAMPED, never sent
//
// A task's every record bumps its version — a status, an assignee, a label, a
// due date, a move to another project — and most of those leave the embedded
// text exactly as it was. The selection carries the digest of the vector each
// source already has in the space being embedded ([Document.StoredSHA]), and
// where it equals the digest of the text this tick would send, that stored
// vector is republished under the source's current version and container
// ([Embedder.restamp]): one record, no provider call. What costs a provider
// call is the TEXT changing, which is what the digest every record carries
// was always documented to decide. A digest stored under the older definition
// — over the raw cut, before the opening was prepared first — never equals
// one taken now, so each such source is embedded once more the first time it
// is selected: money, not correctness.
//
// # Why a failure is counted rather than returned
//
// A REFUSAL costs the input refused and nothing else. ANY OTHER FAILURE — a
// rate limit, a timeout, a server down, a credential refused, an answer this
// package could not read — is a fact about the provider rather than about an
// input, and the next request would meet it too: it ends the tick's requests,
// is logged, and the tick after it is the retry. Neither is returned, because
// returning the first one would abandon every corpus after this one — the
// corpus stops being embedded because of one document in it, and the symptom
// is a search that quietly stops improving. A PUBLISH that fails is
// different, and does stop the tick and is returned: an evicted node, an
// unreachable broker or a store that refuses the snapshot fails the next
// publish identically, so carrying it would spend the provider's answers on
// vectors nothing can write.
//
// # How the tick's requests are divided between the corpora
//
// ROUND ROBIN, ONE REQUEST AT A TIME, and the shape it replaced is why: a
// single budget spent corpus by corpus in order hands the whole tick to
// whoever is first in [EmbedDeps.Corpora] whenever that corpus has a tick's
// worth of backlog. A company writing tasks faster than 1 024 a minute — or
// simply one still cold-filling a hundred thousand of them — never reached
// its pages at all: not one page embedded, not one trashed page's vector
// withdrawn, for as long as the task backlog refilled. And the symptom is the
// one this file exists to prevent: page searches silently answering with the
// lexical half only, while a coverage gauge that SUMS the corpora reports a
// company that is merely behind.
//
// Round robin IS a reserved share plus the redistribution of what nobody used,
// expressed as one rule rather than two. With every corpus behind, each gets
// ⌊budget/N⌋ of [EmbedRequestsPerTick]'s requests, and the remainder goes to
// the ones at the front, an advantage bounded by a single request. With any of
// them caught up or empty, its turn is skipped and the rest take the requests
// it did not need, so the ceilings are still spent in full and a corpus with
// nothing to do wastes nothing.
//
// THE SOURCES NEED A RESERVATION OF THEIR OWN, because turns do not spend
// them evenly: an accepted request takes up to [EmbedBatch] and a refused one
// none, so one corpus isolating a refusal and its neighbour embedding full
// requests take the same turns while only the neighbour drains the ceiling.
// Each corpus that can still work therefore holds ⌊[EmbedSourcesPerTick]/N⌋
// of them in reserve ([tickRequests.room]), released the moment it has nothing
// left to do.
//
// The two alternatives, and what each costs:
//
//   - PROPORTIONAL TO BACKLOG is the other honest reading of "fair", and it
//     re-creates the starvation with arithmetic in place of ordering: at a
//     hundred tasks to one page, a page written this minute waits out the
//     entire task backlog. Equal shares bound a corpus's worst-case staleness
//     by ITS OWN size, which is the property a founder searching the wiki
//     actually has to be able to reason about.
//   - A ROTATING START INDEX fixes the starvation too, and makes each
//     corpus's progress bursty — a whole tick for one, then a whole tick for
//     the other — for identical throughput and a worse worst-case latency on
//     both. It would also be one more thing kept across ticks, which the
//     engine rebuilds this duty for on every one, beside the only memory
//     that has to be there ([Refusals]).
func (e *Embedder) Tick(ctx context.Context) (int, error) {
	dim := e.deps.Embedder.Width()
	published := 0
	var failed []error

	// THE INDEX FIRST, before this tick publishes anything. Its step runs
	// only on a node that has applied the whole vector log ([LogStanding]),
	// and a tick that embedded first would have put its own node behind by
	// every record it had just published — on a company whose corpus moves
	// every minute, every tick, so the index would never be kept at all. The
	// previous tick's records have had the interval to apply. A failure of
	// it costs the index and never the tick's embeddings: a step that cannot
	// run this tick runs on the next, over the rows as they are then.
	n, err := e.maintainIndex(ctx, dim)
	published += n
	if err != nil {
		e.deps.Logger.WarnContext(ctx, "search_index_step_failed",
			"error", err.Error())
		failed = append(failed, fmt.Errorf("search: the semantic index: %w", err))
	}

	bound := e.inputBound()
	now := e.deps.Now()

	// ONE SELECTION PER CORPUS PER TICK, ASKED FOR THE WHOLE TICK'S
	// CEILING. Every request a corpus is granted is formed out of this one
	// list and the corpus is never re-queried, because a record this tick
	// published is not applied yet: a second selection inside the same tick
	// returns the documents just embedded and pays the provider for them
	// again. Asking for the ceiling rather than for the fair share is what
	// lets a corpus take the requests its neighbours did not use — how many
	// that is cannot be known until every corpus has answered.
	//
	// THE PRICE IS N SELECTIONS' WORTH OF ROWS TO SPEND ONE, and it is
	// written as a function of N rather than of today's corpora because N
	// is the term a later source kind moves: each corpus is asked for
	// EmbedSourcesPerTick rows while the tick can embed that many in TOTAL,
	// so with every corpus behind at once (N-1)/N of what was read is
	// discarded — 1 024 documents at the two corpora shipped, 7 168 at the
	// eight [NewEmbedder] allows. It is not only SQL work: every selected
	// [Document] holds its body's first [embedReadChars] characters — up to
	// 64 KiB, 16 KiB of prose — so N x 1 024 of those are live for the
	// length of the tick, up to 64 MiB a corpus, and a third corpus is a
	// 50 % rise in this duty's peak footprint before it embeds anything
	// new. That is the figure to weigh when adding one. None of it is lost
	// WORK — the selection is derived from the rows, so the next tick asks
	// again. A held refusal adds one row of keys to its corpus's read and
	// no body ([Held]).
	//
	// TWO SHAPES THAT WOULD BOUND THE READ WERE WEIGHED AND NOT TAKEN,
	// because each buys it back with something load-bearing:
	//
	//   - A FAIR-SHARE FIRST PASS — ask each corpus for ceil(budget/N)
	//     requests' worth and top up whoever came back full — rations the
	//     FORGET path by the same limit, because `gone` rides on this one
	//     selection. Withdrawals are deliberately OUTSIDE the provider
	//     budget (see the loop below), so rationing them by 1/N is exactly
	//     the trade this file refuses: a page somebody deleted would stay
	//     findable by meaning N times as long, to save rows on a tick whose
	//     wall clock is provider round trips.
	//   - A TOP-UP SELECTION for a corpus that exhausted its slice needs a
	//     cursor the [Corpus] seam does not have. It would re-read the rows
	//     it already took and filter them against an id set, since the
	//     selection order is not promised to be total — a smaller bounded
	//     waste plus a second query and an invariant nothing else here
	//     depends on, rather than no waste.
	queues := make([]*corpusQueue, 0, len(e.deps.Corpora))
	for _, corpus := range e.deps.Corpora {
		refused := e.deps.Refusals.plan(corpus.Source(), now)
		docs, gone, err := corpus.Stale(ctx, e.deps.Model, dim, EmbedSourcesPerTick, refused.held)
		if err != nil {
			// A SELECTION FAILURE COSTS ITS OWN CORPUS AND NOT THE
			// TICK, for the same reason a refused input costs only
			// itself — and here the reason is sharper: the corpora are
			// visited in order, so returning on the first failure
			// would let one corpus whose query cannot run starve every
			// corpus behind it, tick after tick. That is the same
			// starvation the round robin below exists to remove, and
			// an ordering hazard is not less of one for arriving as an
			// error. It is still reported: the tick returns every
			// failure it carried.
			e.deps.Logger.WarnContext(ctx, "search_embed_select_failed",
				"source", string(corpus.Source()), "error", err.Error())
			failed = append(failed, fmt.Errorf(
				"search: select stale %s sources: %w", corpus.Source(), err))
			continue
		}
		q := &corpusQueue{source: corpus.Source()}
		restated := 0
		offered := map[string]bool{}
		for _, doc := range docs {
			p := pendingOf(doc, bound)
			entry, wasRefused := refused.entries[doc.ID]
			switch {
			case doc.StoredSHA != "" && doc.StoredSHA == p.sha:
				// THE STORED VECTOR IS THIS TEXT'S VECTOR — same model,
				// same width, same bytes sent — so the source is
				// restamped, never sent again ([Embedder.restamp]); and
				// a text with a vector is not one the provider refuses.
				if wasRefused {
					e.deps.Refusals.forget(q.source, doc.ID)
				}
				q.restamps = append(q.restamps, p)
				continue
			case !wasRefused:
			case entry.sha != p.sha:
				// THE TEXT MOVED: a different input from the one the
				// provider refused, offered at once with its neighbours.
				e.deps.Refusals.forget(q.source, doc.ID)
			case refused.retry[doc.ID]:
				// DUE, and one of this tick's retries: offered ALONE,
				// so its neighbours are not refused with it.
				p.alone = true
				offered[doc.ID] = true
			default:
				// HELD, and selected only because its version or title
				// moved while its text did not — a status, a move — so
				// it is held again under the new ones and the next
				// selection passes over it ([Held]).
				e.deps.Refusals.restate(q.source, doc)
				restated++
				continue
			}
			q.waiting = append(q.waiting, p)
		}
		for id := range refused.retry {
			if !offered[id] {
				// A RETRY THE SELECTION DID NOT RETURN is forgotten:
				// its source was removed, has a vector by now, or reads
				// another text — the cases where nothing refused is left
				// to remember — and that is how the memory of a removed
				// source ends. A retry passed over only because the
				// selection filled first with older sources would be
				// forgotten too; it is then offered with its neighbours
				// once and isolated again, which is the bounded cost of
				// having no clock of its own deciding what to forget.
				e.deps.Refusals.forget(q.source, id)
			}
		}
		// THE ISOLATION THE LAST TICK DID NOT FINISH goes first, where
		// it stopped ([Refusals.suspend]).
		q.resume(e.deps.Refusals.resume(q.source))
		if held := refused.held.Len(); held > 0 || len(refused.retry) > 0 {
			e.deps.Logger.DebugContext(ctx, "search_embed_inputs_held",
				"source", string(q.source), "held", held,
				"restated", restated, "retried", len(offered),
				"detail", "inputs the provider refused alone: the selection "+
					"passes over each until its retry is due, and a tick "+
					"offers a corpus's due ones again alone, a few at a time")
		}
		queues = append(queues, q)

		// A FORGET COSTS NO PROVIDER CALL, so it is outside the budget
		// the requests are rationed by: withdrawing the vector of a task
		// somebody deleted is a correction the company has already paid
		// for, and rationing it would leave deleted work findable by
		// meaning for exactly as long as the corpus stayed behind. It is
		// bounded all the same, by the selection limit above.
		for _, id := range gone {
			if err := e.forget(ctx, corpus.Source(), id); err != nil {
				// AND THIS ONE DOES STOP THE TICK, because it is
				// not a fact about the corpus: an evicted node, an
				// unreachable broker or a store that refuses the
				// snapshot fails the next corpus's publishes
				// identically, so carrying it would buy nothing
				// and spend the provider's requests to find out.
				failed = append(failed, err)
				return published, errors.Join(failed...)
			}
			published++
			// A WITHDRAWAL PUBLISHED is progress ([Budget]): one
			// publish, bounded as every publish is. There can be a
			// selection's worth of them a corpus — 1 024 after a bulk
			// purge — and unreported they were the longest stretch a
			// live tick went silent, a tick's publishes with no
			// provider call between them to show for it, which a slow
			// broker could stretch past the bound.
			e.deps.Budget.Advanced()
		}
	}

	limits := e.deps.Embedder.Limits()
	limits.BatchInputs = min(limits.BatchInputs, EmbedBatch)
	t := tickRequests{
		requests: EmbedRequestsPerTick, sources: EmbedSourcesPerTick,
		share: EmbedSourcesPerTick / max(1, len(queues)),
	}
	// WHAT EACH CORPUS'S ISOLATION HAS NOT REACHED is kept for its next
	// tick, however this one ends ([Refusals.suspend]).
	defer func() {
		for _, q := range queues {
			e.deps.Refusals.suspend(q.source, q.split)
		}
	}()
	for t.sources > 0 {
		// ONE PASS OVER THE CORPORA, ONE TURN EACH: a run of restamps, or
		// one request. A pass that hands out nothing means no corpus has
		// work left it may do, which is the only way out of this loop
		// other than the sources ceiling: a caught-up company must cost
		// one selection per corpus and stop.
		spent := false
		for _, q := range queues {
			room := t.room(q, queues)
			if room == 0 {
				continue
			}
			if len(q.restamps) > 0 {
				// A RESTAMP TURN costs no request, so it is taken
				// whatever the provider answered this tick; it costs
				// records, so it is a turn in the same round robin.
				spent = true
				n, err := e.restamp(ctx, q, dim, room, &t)
				published += n
				switch {
				case errors.Is(err, errStoredUnreadable):
					failed = append(failed, err)
				case err != nil:
					failed = append(failed, err)
					return published, errors.Join(failed...)
				}
				continue
			}
			if !t.open() {
				continue
			}
			group, err := q.next(limits, room)
			if err != nil {
				// UNREACHABLE: every text is cut inside the model's
				// own bound ([Embedder.inputBound]) and the limits were
				// validated when the provider was built. Said rather
				// than assumed, and it ends the requests like any
				// failure that is not about an input.
				e.deps.Logger.WarnContext(ctx, "search_embed_unplannable",
					"source", string(q.source), "error", err.Error())
				t.stopped = true
				continue
			}
			if len(group) == 0 {
				continue
			}
			spent = true
			n, err := e.request(ctx, q, dim, group, &t)
			published += n
			if err != nil {
				failed = append(failed, err)
				return published, errors.Join(failed...)
			}
		}
		if !spent {
			break
		}
	}
	if t.refusedAlone > 0 && t.accepted == 0 && !t.stopped {
		// EVERY REQUEST REFUSED, single inputs included, which is the
		// shape of a provider refusing the CONFIGURATION rather than an
		// input — a parameter it does not take, a model it does not serve
		// at that width. Each input it refused alone is named above; this
		// says what they have in common.
		//
		// ONLY WHEN THAT IS WHAT HAPPENED: an input refused alone, nothing
		// accepted, and no other failure. A tick that met one refusal and
		// was then stopped by a rate limit sent requests that were not
		// refused at all, and blaming the configuration for it would send
		// an operator to fix a setting that is fine.
		e.deps.Logger.WarnContext(ctx, "search_embed_every_request_refused",
			"model", e.deps.Model, "requests", t.refused,
			"detail", "the provider refused every request this tick, inputs "+
				"sent alone included — when no input is ever accepted the "+
				"refusal is the configuration's (providers.embeddings), not "+
				"any document's")
	}
	return published, errors.Join(failed...)
}

// tickRequests is what a tick may still send, and what its answers were.
type tickRequests struct {
	// requests and sources are what is left of [EmbedRequestsPerTick]
	// and [EmbedSourcesPerTick].
	requests, sources int

	// share is each corpus's reserved part of the sources: the ceiling
	// divided between the corpora ([tickRequests.room]).
	share int

	// accepted and refused count the requests the provider embedded and
	// refused, and refusedAlone the refused ones that carried one input.
	accepted, refused, refusedAlone int

	// stopped says a failure that is not about an input ended the tick's
	// requests.
	stopped bool
}

// open reports whether the tick may send another request.
func (t *tickRequests) open() bool {
	return !t.stopped && t.requests > 0 && t.sources > 0
}

// room is how many of the tick's sources q may still take: what is left,
// less what every OTHER corpus that can still work has not yet used of its
// share.
//
// # Why the sources are reserved, and the requests are not
//
// The requests are divided by turns — one each, round robin — and that alone
// shares them, because every turn costs one. The sources are not spent by
// turns: an accepted request spends up to [EmbedBatch] of them and a refused
// one spends none, so a corpus isolating a refused input — sending halves that
// are refused, which publish nothing — and a neighbour embedding full requests
// of short sources take the same turns while only the neighbour drains the
// shared ceiling. Shared first come, the neighbour emptied it in eight turns,
// the round robin ended at the ceiling, and the isolation was cut off seven
// halves deep, the input it was closing on never sent alone; the next tick
// selected it first again and met the same end, so that corpus embedded
// nothing for as long as its neighbour's backlog lasted — the starvation the
// round robin exists to prevent, arriving through the other ceiling.
//
// So each corpus that can still work holds ⌊sources / N⌋ in reserve, and a
// neighbour takes only what a corpus has no work for: the reservation goes
// the moment the corpus has nothing left it may do ([corpusQueue.canWork]),
// which within a tick never comes back. The remainder of the division, and
// the share of a corpus with nothing to do, are anybody's.
func (t *tickRequests) room(q *corpusQueue, queues []*corpusQueue) int {
	reserved := 0
	for _, other := range queues {
		if other != q && other.canWork(t) {
			reserved += max(0, t.share-other.used)
		}
	}
	return max(0, t.sources-reserved)
}

// inputBound is how many bytes of a source this duty sends: [EmbedInputBytes],
// or the model's own per-input bound where that is smaller.
//
// THE MODEL'S BOUND IS A FACT the provider enforces — it refuses an input past
// it before sending anything — so an opening longer than the model accepts is
// not a longer opening but a request refused on every tick. Where the model
// takes 8 KiB or more, which OpenAI's do at exactly 8 192, this is
// EmbedInputBytes unchanged.
func (e *Embedder) inputBound() int {
	return min(EmbedInputBytes, e.deps.Embedder.Limits().InputBytes)
}

// request sends one request — a group the model's limits admit — and acts on
// the answer: publishes what it embedded, splits what it refused, holds back
// an input refused alone, or ends the tick's requests.
//
// It returns the records it published, and an error only for a publish that
// failed, which stops the tick ([Embedder.Tick]).
func (e *Embedder) request(ctx context.Context, q *corpusQueue, dim int, group []pending, t *tickRequests) (int, error) {
	texts := make([]string, len(group))
	for i, p := range group {
		texts[i] = p.text
	}
	vectors, err := e.deps.Embedder.EmbedBatch(ctx, texts)
	t.requests--
	// THE PROVIDER ANSWERED, WHATEVER IT ANSWERED, which is progress
	// ([Budget]): one request, bounded by its own timeout.
	e.deps.Budget.Advanced()
	switch {
	case err == nil && len(vectors) != len(group):
		// A SHORT ANSWER IS A RE-FILING, never a partial result: vectors
		// are matched to sources by position. The provider's contract
		// rules it out, so an implementation that broke it is not one to
		// send the rest of the tick to.
		e.deps.Logger.WarnContext(ctx, "search_embed_request_failed",
			"source", string(q.source), "model", e.deps.Model,
			"inputs", len(group), "error", fmt.Sprintf("the provider returned "+
				"%d vectors for %d inputs", len(vectors), len(group)))
		t.stopped = true
		return 0, nil
	case err == nil:
		t.accepted++
		for _, p := range group {
			if p.alone {
				// ACCEPTED AFTER ALL — a provider fixed, a rule relaxed —
				// so the memory of its refusal goes with it, whatever
				// becomes of the vector: it is no longer refused.
				e.deps.Refusals.forget(q.source, p.doc.ID)
			}
		}
		return e.publishAll(ctx, q, dim, group, vectors, t)
	case errors.Is(err, embeddings.ErrRefused) && len(group) > 1:
		// SPLIT, AND THE HALVES GO FIRST: the corpus's next turns send
		// them before anything else it holds, so the input the provider
		// refuses is alone within log₂ of the request's size, and every
		// half it accepts on the way publishes as it goes.
		t.refused++
		first, second := halves(group)
		q.split = append([][]pending{first, second}, q.split...)
		return 0, nil
	case errors.Is(err, embeddings.ErrRefused):
		// REFUSED ALONE: this input is what the provider will not take.
		// It costs itself, and is held back until its retry is due.
		t.refused++
		t.refusedAlone++
		p := group[0]
		e.deps.Refusals.refuse(q.source, p, e.deps.Now())
		e.deps.Logger.WarnContext(ctx, "search_embed_input_refused",
			"source", string(q.source), "id", p.doc.ID, "model", e.deps.Model,
			"bytes", len(p.text),
			"input_limit_bytes", e.deps.Embedder.Limits().InputBytes,
			"retry_in", EmbedRefusalRetry.String(), "error", err.Error(),
			"detail", "the provider refused this source's text sent alone; it "+
				"is not embedded, and is offered again alone when the retry is "+
				"due — a refusal of text inside input_limit_bytes says the "+
				"model's limits are declared wider than the endpoint enforces "+
				"(providers.embeddings.max_input_tokens), or that the endpoint "+
				"refuses this text for what it says")
		return 0, nil
	}
	// NOT ABOUT AN INPUT — transient, the configuration, a cancellation or
	// an answer this package could not read — so the next request would
	// meet it too.
	t.stopped = true
	e.deps.Logger.WarnContext(ctx, "search_embed_request_failed",
		"source", string(q.source), "model", e.deps.Model,
		"inputs", len(group), "error", err.Error(),
		"detail", "this tick sends no more requests; the next tick asks again, "+
			"and nothing is lost — the selection is derived from the rows")
	return 0, nil
}

// publishAll publishes a record per vector an accepted request returned.
func (e *Embedder) publishAll(ctx context.Context, q *corpusQueue, dim int, group []pending, vectors [][]float32, t *tickRequests) (int, error) {
	source := q.source
	published := 0
	for i, vector := range vectors {
		p := group[i]
		if len(vector) == 0 {
			// A DOCUMENT WITH NOTHING TO EMBED, which is a real state
			// — an empty page, a task that is a title somebody
			// deleted — and not a failure. It keeps no vector and is
			// selected again next pass, never costing a provider call,
			// because an empty input is not sent.
			continue
		}
		err := e.publish(ctx, source, dim, p, vector)
		// EACH PUBLISH IS PROGRESS ([Budget]), as a withdrawal's is: one
		// publish, bounded as every publish is. Counted as one stretch
		// with the request before it, a request's hundred and
		// twenty-eight were the longest a live tick went silent, and a
		// broker slow enough to take its bound's length over them cut off
		// a tick that was publishing steadily — the slow-but-advancing
		// tick the bound exists NOT to cut off. A refused vector
		// published nothing, and its refusal is an answer all the same.
		e.deps.Budget.Advanced()
		if err != nil {
			// A VECTOR THIS DUTY REFUSES COSTS ITS OWN DOCUMENT AND
			// NOT THE REQUEST, which is the same rule one level down:
			// the refusal is about one vector, and dropping the other
			// 127 would make one poisoned component cost a hundred
			// provider calls' worth of work. It is not silent — the
			// log names the document — and it is not lost, because
			// the selection is over the rows and picks it up again.
			if errors.Is(err, errUnusableVector) {
				e.deps.Logger.WarnContext(ctx, "search_embed_vector_refused",
					"source", string(source), "id", p.doc.ID,
					"error", err.Error())
				continue
			}
			return published, err
		}
		published++
		t.sources--
		q.used++
	}
	return published, nil
}

// publish writes one embed record for p, its digest over the text sent.
func (e *Embedder) publish(ctx context.Context, source Source, dim int, p pending, vector []float32) error {
	packed, err := pack(vector, dim)
	if err != nil {
		return fmt.Errorf("search: %s: %w: %w", Subject{Source: source, ID: p.doc.ID},
			errUnusableVector, err)
	}
	return e.publishPacked(ctx, source, dim, p, packed)
}

// restamp publishes, for one turn's run of the corpus's restamps, the vector
// each source ALREADY HAS — read back from the estate — under the source's
// current version and container: no provider call, one record each.
//
// # Why a restamp is an embed record
//
// What it says is exactly what an embed says — this source, at this version,
// in this container, has this vector computed from this text in this space —
// and the vector really is the one its text produces, because the digest the
// stored row carries is of the same bytes this text would be sent as, at the
// same model and width. So it is an [OpEmbed] at the version that has always
// carried one: every build applies it, writes both rows exactly as a fresh
// embed would (the sign code and the index filing are functions of the same
// bytes), and needs no reader gate. Its operation id differs from the record
// it replaces because the source version is in it, so the broker's duplicate
// window does not collapse it.
//
// # Why the vector is read again rather than carried from the selection
//
// The selection names the stored digest so the duty knows which sources to
// restamp; the VECTOR is read here, with its digest, by primary key in one
// statement, and published only where that digest still matches. Read in a
// separate transaction from the selection, a vector paired with the
// selection's digest could be a newer one written in between — a record
// asserting a text for a vector computed from another.
func (e *Embedder) restamp(ctx context.Context, q *corpusQueue, dim, room int, t *tickRequests) (int, error) {
	run := q.restamps[:min(len(q.restamps), EmbedBatch, room)]
	q.restamps = q.restamps[len(run):]
	stored, err := e.storedVectors(ctx, q.source, run, dim)
	if err != nil {
		// A STORE THAT CANNOT BE READ costs this corpus's restamps and
		// not the tick, as a selection that fails does: they are
		// selected again next tick.
		q.restamps = nil
		e.deps.Logger.WarnContext(ctx, "search_embed_restamp_unreadable",
			"source", string(q.source), "error", err.Error())
		return 0, err
	}
	published := 0
	for _, p := range run {
		vector, ok := stored[p.doc.ID]
		if !ok {
			// MOVED SINCE THE SELECTION: a newer vector, or none. The
			// next tick's selection decides afresh.
			continue
		}
		err := e.publishPacked(ctx, q.source, dim, p, vector)
		// EACH PUBLISH IS PROGRESS ([Budget]), as an embed's is.
		e.deps.Budget.Advanced()
		if err != nil {
			return published, err
		}
		published++
		t.sources--
		q.used++
	}
	return published, nil
}

// errStoredUnreadable marks a restamp's read of the stored vectors failing,
// which costs the corpus's restamps this tick and not the tick.
var errStoredUnreadable = errors.New("the stored vectors could not be read")

// storedVectors reads the vectors run's sources already have in the space
// being embedded, keyed by source id — only those whose stored digest is still
// the one this tick would send them under.
func (e *Embedder) storedVectors(ctx context.Context, source Source, run []pending, dim int) (map[string][]byte, error) {
	out := make(map[string][]byte, len(run))
	err := e.deps.Estate.Read(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, storedVectorStatement)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, p := range run {
			var sha string
			var embedding []byte
			err := stmt.QueryRowContext(ctx, string(source), p.doc.ID,
				e.deps.Model, dim).Scan(&sha, &embedding)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return err
			}
			if sha == p.sha && len(embedding) == 4*dim {
				out[p.doc.ID] = embedding
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w for %d %s source(s): %w",
			errStoredUnreadable, len(run), source, err)
	}
	return out, nil
}

// storedVectorStatement reads one source's stored vector by PRIMARY KEY, in
// one space. Bound: the source, its id, the model, the width.
//
// ONE SOURCE A STATEMENT, prepared once a run, never one statement over a
// list: handed `source_id IN (?, …)`, or a join from a JSON list of ids, this
// engine's planner seeks the key on `source` alone and reads every row of that
// source — every twelve-kilobyte vector of half the corpus, to find a hundred
// and twenty-eight — where this seeks each one (the plan gate holds it there).
const storedVectorStatement = `
	SELECT text_sha, embedding FROM kb_vectors
	WHERE source = ? AND source_id = ? AND model = ? AND dim = ?`

// publishPacked writes one embed record for p over an already packed vector.
func (e *Embedder) publishPacked(ctx context.Context, source Source, dim int, p pending, packed []byte) error {
	subject := Subject{Source: source, ID: p.doc.ID}
	rec := VectorRecord{
		RecordEnvelope: RecordEnvelope{
			Subject:   subject,
			Op:        OpEmbed,
			CreatedAt: e.deps.Now().UTC(),
			Scope: statelog.ScopeSet{
				Paths: []string{ScopePath(p.doc.Container, subject)},
			},
		},
		Container: p.doc.Container,
		Model:     e.deps.Model,
		Dim:       dim,
		SourceRev: p.doc.Version,
		TextSHA:   p.sha,
		Title:     p.doc.Title,
		Embedding: packed,
	}
	return e.append(ctx, subject, rec)
}

// forget withdraws a vector whose source is gone.
func (e *Embedder) forget(ctx context.Context, source Source, id string) error {
	subject := Subject{Source: source, ID: id}
	return e.append(ctx, subject, VectorRecord{
		RecordEnvelope: RecordEnvelope{
			Subject:   subject,
			Op:        OpForget,
			CreatedAt: e.deps.Now().UTC(),
			Scope: statelog.ScopeSet{
				// THE DOMAIN ROOT'S CONTAINER LEVEL IS UNKNOWN
				// HERE, because the source row is gone — so the
				// scope is the widest honest one for this
				// document rather than a container guessed from a
				// row nobody can read.
				Paths: []string{ScopePath(UnfiledContainer, subject)},
			},
		},
	})
}

// append publishes one record additively.
//
// PatternAdditive, and it is the pattern rather than a shortcut: this domain
// has exactly one writer per subject — a fleet singleton — so there is nothing
// to arbitrate against, and an expectation would be a per-subject anchor row
// written for a race that cannot happen. What makes a redelivery safe is the
// applier's own position guard.
func (e *Embedder) append(ctx context.Context, subject Subject, rec VectorRecord) error {
	// THE OP ID GOES ON THE RECORD BEFORE IT IS ENCODED, not beside it in
	// the request. The broker's duplicate window keys on the message id the
	// framework takes from the envelope, and this domain has no operation
	// ledger behind it — so an id that lived only in the request would
	// travel on the message and be absent from every copy of the record any
	// node ever reads back.
	rec.OpID = opIDFor(rec)
	// THE REQUEST IS FORMED FROM THE RECORD'S OWN ENVELOPE, as the one every
	// applier reads — encoded once here, before the stamp exists, only so
	// the subject, the scope and the op id are the record's rather than a
	// second spelling of them. The stamp does not move any of the three.
	unstamped, err := rec.Encode()
	if err != nil {
		return fmt.Errorf("search: encode the vector record for %s: %w", subject, err)
	}
	env, err := Domain{}.Envelope(unstamped)
	if err != nil {
		return fmt.Errorf("search: the record this duty just wrote for %s does "+
			"not decode: %w", subject, err)
	}
	opID := env.OpID
	res, err := e.deps.Publisher.Publish(ctx, statelog.Request{
		Subject: env.Subject,
		Scope:   env.Scope,
		OpID:    opID,
		Pattern: statelog.PatternAdditive,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE FRAMEWORK'S STAMP, on this record as on every other
			// domain's. This domain's gates are open, so nothing here
			// reads the writer today — but it is the envelope's
			// declared field, the publisher refuses a record without
			// it, and a gate added later must not find a corpus of
			// records that name nobody.
			stamped := rec
			stamped.Gen, stamped.Writer = stamp.Gen, stamp.Writer
			payload, encodeErr := stamped.Encode()
			if encodeErr != nil {
				return statelog.Decision{}, fmt.Errorf("search: encode the "+
					"vector record for %s: %w", subject, encodeErr)
			}
			return statelog.Decision{Payload: payload}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("search: publish the vector record for %s: %w", subject, err)
	}
	// AN UNKNOWN OUTCOME IS NOT AN ERROR HERE, and this is the one domain
	// where that is honestly true: nothing is waiting on the answer, the
	// apply is idempotent under its position guard, and the selection that
	// produced this document is derived from the rows — so a record that
	// landed is not re-sent and one that did not is simply selected again.
	if res.Outcome == statelog.OutcomeUnknown {
		e.deps.Logger.InfoContext(ctx, "search_embed_unresolved",
			"subject", subject.String(), "op_id", opID)
	}
	return nil
}

// opIDFor is the record's own idempotency key, DERIVED rather than minted.
//
// A uuid7 per attempt would make every retry a new operation, and the broker's
// duplicate window — the only dedupe this domain has, since it keeps no
// operation ledger — would collapse nothing. Derived from what the record
// says, a retry of the same work carries the same id and is collapsed; a
// genuinely newer vector for the same source carries a different one, because
// the source version and the text digest are both in it.
//
// AND THE CONTAINER AND THE TITLE, because a page's restamp moves neither of
// the other two: a page moved to another container keeps its edit number and
// its text, so without them its restamp carried the very id of the embed it
// replaces and the broker collapsed it — dropped with an acknowledgement, the
// vector left filed where the page no longer is. A page moved and moved back
// inside the broker's window repeats an id that window still holds; its rows
// then lag the page until the window passes, when the next tick selects it
// again and the restamp lands.
func opIDFor(rec VectorRecord) string {
	parts := []string{
		rec.Subject.String(), string(rec.Op), rec.Model,
		fmt.Sprint(rec.Dim), fmt.Sprint(rec.SourceRev), rec.TextSHA,
		rec.Container, rec.Title,
	}
	// AN INDEX RECORD IS NAMED BY WHAT IT INSTALLS, or two trainings inside
	// the broker's duplicate window — an index and the verdict that replaces
	// it, or two indexes a minute apart — would share one id and the second
	// would be COLLAPSED into the first, dropped with an acknowledgement.
	if x := rec.Index; x != nil {
		parts = append(parts, x.Log, fmt.Sprint(x.Basis), fmt.Sprint(x.Seed),
			fmt.Sprint(x.Lists), fmt.Sprint(x.Probes), fmt.Sprint(x.TrainedOn),
			fmt.Sprint(x.Largest), string(x.Why), digestOf(x.Centroids),
			fmt.Sprint(len(x.Rollout)))
		if m := x.Measurement; m != nil {
			parts = append(parts, fmt.Sprintf("%+v", *m))
		}
	}
	// A BATCH IS NAMED BY ITS INDEX AND NUMBER, which fix its range, so every
	// publication of it is one operation and the duplicate window collapses a
	// repeat inside it.
	if r := rec.Reassign; r != nil {
		parts = append(parts, fmt.Sprint(r.Index), fmt.Sprint(r.Batch))
	}
	// A MEASUREMENT BY WHAT IT FOUND, so a second one inside the window that
	// found something different is published rather than collapsed.
	if m := rec.Measure; m != nil {
		parts = append(parts, fmt.Sprint(m.Index), fmt.Sprint(m.Probes),
			fmt.Sprintf("%+v", m.Measurement))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "vec-" + hex.EncodeToString(sum[:16])
}

// errUnusableVector marks a vector this duty will not publish, which its
// caller treats as costing that document and not the request it arrived in.
var errUnusableVector = errors.New("the provider's vector cannot be published")

// pack renders a vector in the layout the column holds.
//
// THE FINITENESS CHECK IS HERE rather than at the store's boundary, and it is
// not duplication: this value is published to every node in the fleet before
// any of them writes it, so a NaN refused at the write boundary would be a
// record every applier fails on for ever — a poison message on a stream with
// no dead-letter path. A vector_distance_cos of NaN answers 0, a PERFECT
// match, so one poisoned row outranks every genuine hit in every search.
func pack(v []float32, dim int) ([]byte, error) {
	if len(v) != dim {
		return nil, fmt.Errorf("the provider returned a %d-wide vector and the "+
			"corpus is embedded at %d", len(v), dim)
	}
	out := make([]byte, 4*len(v))
	for i, f := range v {
		f64 := float64(f)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			return nil, fmt.Errorf("component %d of %d is %v — a non-finite "+
				"component scores as a perfect match against everything",
				i, len(v), f)
		}
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out, nil
}

// TaskCorpus embeds the native tracker's tasks.
//
// ONE STATEMENT, because `tracker_tasks` and `kb_vectors` are both in the
// replicated estate: the selection is an anti-join between the rows and their
// own vectors, and there is no second read to keep in step.
//
// A TASK IS KEYED ON ITS VERSION, which every task record moves — a status, an
// assignee, a label, a move to another project as well as an edit of its text.
// That selects more than the text needs, and it is still the right key: what
// is selected without a change of text is restamped rather than embedded
// ([Embedder.Tick]), at the same one record a re-embed cost before, and it
// carries the new project with it. Nothing on the task row moves only when the
// embedded text does — `body_version` moves with the body and not the title —
// and a key that did would be one the tracker's applier maintains: a change
// to identity-claimed rows, across a rolling upgrade, for what a restamp
// inside this domain already gives.
type TaskCorpus struct{ DB store.ReplicatedReader }

// Source implements [Corpus].
func (TaskCorpus) Source() Source { return SourceTask }

// taskLive is the population the task corpus embeds: every task not removed.
//
// ONE SPELLING for every statement that reads or counts it — the selection,
// the withdrawals, the coverage and the window report ([Window]) — because
// two ideas of which sources a corpus holds is a coverage over one population
// and a backlog over another. Spelled out rather than implied, too: a
// tracker index carries this predicate, and the planner reaches a partial
// index only for a statement that states it.
const taskLive = `t.removed_at IS NULL`

// taskBody is a task's body as a statement reads it — out of the encoded
// document, where the tracker keeps it ([Document.Body]) — and taskOpening the
// part of it an opening can need: its first [embedReadChars] characters, bound
// as the statement's next argument.
//
// ONE SPELLING for the selection and the window report ([Window]), because
// the report's claim is that it measures what the duty sends: a report that
// cut the body its own way — or not at all, which is what it did — counts as
// embedded text the duty never read.
const (
	taskBody    = `COALESCE(json_extract(t.document, '$.body'), '')`
	taskOpening = `substr(` + taskBody + `, 1, ?)`
)

// TaskSelection is the statement [TaskCorpus.Stale] selects stale tasks with,
// and its arguments; [TaskOpeningRead], [TaskWithdrawals] and
// [TaskCoverageCount] are the others it runs.
//
// EXPORTED FOR THE TRACKER'S INDEX GATE, which plans every statement that reads
// `tracker_tasks` against the indexes the tracker declares
// (internal/tracker's TestEveryIndexServesARegisteredQuery). A copy written
// for the gate certifies a statement nothing runs, which is how an index on a
// column nothing ever wrote was kept for this duty's "selection" while the
// selection the duty does run was certified by nothing.
func TaskSelection(model string, dim, limit int) (string, []any) {
	return taskSelectionStatement, []any{model, dim, model, dim, limit}
}

// taskSelectionStatement: the tasks with no current vector in the asked space,
// oldest first, each with its key, its title and the digest of the vector it
// has there — and NOT its body, which [TaskOpeningRead] reads for only the
// tasks the selection keeps ([selectStale]).
const taskSelectionStatement = `
	SELECT t.id, t.project_key, t.version, t.title,
	       CASE WHEN v.model = ? AND v.dim = ? THEN v.text_sha ELSE '' END
	FROM tracker_tasks t
	LEFT JOIN kb_vectors v
	  ON v.source = 'task' AND v.source_id = t.id
	WHERE ` + taskLive + `
	  AND (v.source_id IS NULL
	       OR v.source_rev <> t.version
	       OR v.model <> ? OR v.dim <> ?)
	ORDER BY t.updated_at
	LIMIT ?`

// TaskOpeningRead is the statement [TaskCorpus.Stale] reads one selected task's
// opening with — by its primary key — and its arguments. See [TaskSelection].
func TaskOpeningRead(id string) (string, []any) {
	return taskOpeningStatement, []any{embedReadChars, id}
}

const taskOpeningStatement = `
	SELECT ` + taskOpening + `
	FROM tracker_tasks t
	WHERE t.id = ?`

// TaskWithdrawals is the statement [TaskCorpus.Stale] finds the vectors of
// removed or purged tasks with, and its arguments. See [TaskSelection].
func TaskWithdrawals(limit int) (string, []any) {
	return taskWithdrawalsStatement, []any{limit}
}

const taskWithdrawalsStatement = `
	SELECT v.source_id
	FROM kb_vectors v
	LEFT JOIN tracker_tasks t
	  ON t.id = v.source_id AND ` + taskLive + `
	WHERE v.source = 'task' AND t.id IS NULL
	LIMIT ?`

// TaskCoverageCount is the statement [TaskCorpus.Coverage] counts with, and
// its arguments. See [TaskSelection].
func TaskCoverageCount(model string, dim int) (string, []any) {
	return taskCoverageStatement, []any{model, dim}
}

// taskCoverageStatement: [taskSelectionStatement]'s predicate, inverted and
// counted.
const taskCoverageStatement = `
	SELECT COUNT(*),
	       COUNT(CASE WHEN v.source_id IS NOT NULL
	                   AND v.source_rev = t.version
	                   AND v.model = ? AND v.dim = ?
	                  THEN 1 END)
	FROM tracker_tasks t
	LEFT JOIN kb_vectors v
	  ON v.source = 'task' AND v.source_id = t.id
	WHERE ` + taskLive

// Stale implements [Corpus].
func (c TaskCorpus) Stale(ctx context.Context, model string, dim, limit int, held Held) ([]Document, []string, error) {
	var stale []Document
	var gone []string
	err := c.DB.Read(ctx, func(tx *sql.Tx) error {
		statement, args := TaskSelection(model, dim, limit+held.Len())
		var err error
		if stale, err = selectStale(ctx, tx, statement, args, limit, held,
			taskOpeningStatement); err != nil {
			return err
		}

		// AND THE OTHER DIRECTION: a vector whose task has been removed
		// or purged. Without it a deleted task stays findable by meaning
		// for ever — the row it came from is gone, so nothing else will
		// ever select it.
		statement, args = TaskWithdrawals(limit)
		gone, err = selectGone(ctx, tx, statement, args)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return stale, gone, nil
}

// selectStale runs a corpus's selection — statement, which yields each stale
// source's id, container, version, title and stored digest, oldest first, and
// is bound to read up to limit + held.Len() of them — keeps the first limit
// that held does not hold, and then reads each kept source's opening with
// opening, by its primary key, inside the same transaction.
//
// TWO STATEMENTS RATHER THAN ONE, and the pass-over is why. A held source is
// recognised by its key ([Held]), so it is passed over before its body is
// read; read in the selection itself, every held source's body — up to
// [embedReadChars] characters of a page that may hold 512 KiB, and refused
// sources are disproportionately the long ones — would be read on every tick
// to be thrown away. And not a filter inside the selection either: handed the
// held set as a list (`NOT IN (SELECT value FROM json_each(?))`), this
// engine's planner compares every row it scans against the WHOLE list —
// measured over five thousand tasks, 250 ms against 10 ms for the plain
// selection with four thousand held, and rising with the list — where
// recognising them here costs a map lookup a row.
//
// ONE TRANSACTION, so the openings are of the rows the selection read: a body
// read in another snapshot could be a newer one than the version and digest
// the source was selected at, and a vector would be published as one text's
// while computed from another's.
func selectStale(ctx context.Context, tx *sql.Tx, statement string, args []any,
	limit int, held Held, opening string) ([]Document, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	var stale []Document
	for len(stale) < limit && rows.Next() {
		var doc Document
		var version int64
		if err = rows.Scan(&doc.ID, &doc.Container, &version,
			&doc.Title, &doc.StoredSHA); err != nil {
			_ = rows.Close()
			return nil, err
		}
		doc.Version = uint64(version)
		if held.Holds(doc) {
			continue
		}
		stale = append(stale, doc)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(stale) == 0 {
		return nil, nil
	}
	read, err := tx.PrepareContext(ctx, opening)
	if err != nil {
		return nil, err
	}
	defer func() { _ = read.Close() }()
	for i := range stale {
		if err := read.QueryRowContext(ctx, embedReadChars, stale[i].ID).Scan(&stale[i].Body); err != nil {
			// NOT EVEN sql.ErrNoRows is an answer here: the row was
			// read a moment ago in this same snapshot.
			return nil, fmt.Errorf("read the opening of %s: %w", stale[i].ID, err)
		}
	}
	return stale, nil
}

// selectGone runs a corpus's withdrawals statement: the ids of vectors whose
// source is gone.
func selectGone(ctx context.Context, tx *sql.Tx, statement string, args []any) ([]string, error) {
	dead, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dead.Close() }()
	var gone []string
	for dead.Next() {
		var id string
		if err := dead.Scan(&id); err != nil {
			return nil, err
		}
		gone = append(gone, id)
	}
	return gone, dead.Err()
}

// Coverage implements [Corpus].
//
// ONE STATEMENT, so the numerator and the denominator describe the same
// instant. Two counts would let a write land between them and report a
// coverage this corpus never had — which on a company writing tasks steadily
// is not a rare race but the ordinary case.
//
// THE PREDICATE IS [TaskCorpus.Stale]'s, INVERTED. A source is covered when it
// has a vector at this model and width whose `source_rev` still matches the
// task's version; every other row is exactly what the duty would select next.
func (c TaskCorpus) Coverage(ctx context.Context, model string, dim int) (int, int, error) {
	var current, total int
	err := c.DB.Read(ctx, func(tx *sql.Tx) error {
		statement, args := TaskCoverageCount(model, dim)
		return tx.QueryRowContext(ctx, statement, args...).Scan(&total, &current)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("search: count the task corpus's vector "+
			"coverage: %w", err)
	}
	return current, total, nil
}
