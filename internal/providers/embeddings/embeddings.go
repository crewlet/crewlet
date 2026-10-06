// Package embeddings is the vector backend: the one place a text becomes a
// vector, for every caller that ranks by meaning.
//
// # Who asks for one, and what each does with it
//
// Its callers, in two kinds — the ones that send BATCHES (the knowledge corpus,
// a diary's fill) and the ones that embed one text while something waits:
//
//   - The knowledge corpus duty (internal/search) embeds the company's own
//     pages and work items, once for the fleet, and publishes each vector on
//     a replicated log every data node applies. It sends BATCHES, and a
//     failure there is not a degraded answer but a backlog: a document that
//     is never embedded is never found by meaning.
//   - A knowledge search's QUERY VECTOR (internal/search) puts the query in
//     the corpus's space, under a two-second budget; a failure serves the
//     keyword half and says so.
//   - Diary and episode recall (internal/agent/prefetch, internal/learning)
//     embed a turn's ask and each completed turn, and rank a seat's own
//     memories against it; a failure is "no similarity search this turn".
//   - A seat's diary (internal/learning, internal/engine) embeds each note
//     as it is written, and the node holding the seat fills, in batches, the
//     notes left with no vector of the current model; a failure is a note
//     the next fill asks about again.
//
// # Nothing here retries, and what it does instead
//
// What a failure COSTS differs by caller — relevance for a turn starting, a
// backlog for the corpus — and so does the right answer to one: a turn cannot
// wait for a retry, and the corpus retries every minute on its own tick. A
// retry inside this package would spend the first caller's latency and
// duplicate the second's schedule, so there is none, and the SDK's own is
// switched off. What this package owes every caller instead is the FACT of
// which failure it was: [ErrRefused] (this request will be refused again
// unchanged), [ErrTransient] (asking again later may succeed) and
// [ErrConfiguration] (no request will succeed until the operator fixes
// something) are errors.Is-comparable, so the corpus duty can set aside the
// one input a provider will never accept rather than resend its whole batch
// for ever, and a turn can tell "nothing similar" from "could not ask".
//
// # The limits are the model's, and they are enforced HERE
//
// Every model refuses an input past its window and a request past its input
// count or its token total, and those limits are facts about the model
// (config.EmbeddingModels, where the vendor citations live). This package
// holds them as [Limits] — in BYTES of prepared text, which bound tokens
// without a tokenizer — and enforces them before anything is sent: an input
// past the bound is refused locally ([ErrTooLong]), and [BatchEmbedder]'s
// EmbedBatch packs a batch into as many requests as the limits need. A
// caller decides what a text too long for one input MEANS — the corpus
// embeds its [Opening], a turn's ask is represented [EmbedWhole] — and this
// package owns only the fact of where the bound is.
//
// # One preparation, and a digest is over it
//
// What is sent is [Prepare]'s output: whitespace collapsed, which is not
// cosmetic — the same sentence formatted two ways would otherwise be two
// vectors — and always valid UTF-8, each stray byte replaced by U+FFFD as the
// request's JSON encoding replaces it, so the prepared text is the text the
// server decodes and not merely the text handed to the encoder. It is
// exported so a caller sees exactly the bytes a vector was computed from: a
// digest stored beside a vector, to tell later whether the text moved, must
// be taken over the PREPARED text that was sent, or it answers a question
// about bytes the provider never saw.
//
// # The width is a contract with the STORE, not with the model
//
// The vector columns are sized once, at open, from providers.embeddings
// .dimensions. A model that produces a different width does not degrade a
// search — it writes rows that cannot be read back. So the width is checked
// against what the provider actually returns, on every call, and a mismatch
// is refused loudly rather than stored. It is also ASKED FOR, in the request's
// `dimensions`, wherever the endpoint takes that parameter; which endpoint
// does is the configuration's knowledge, cited per model there, and reaches
// this package as [Config.OmitDimensions].
package embeddings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
)

// Embedder turns text into a vector.
//
// One method that embeds, and text-at-a-time rather than a batch: most callers
// embed exactly one thing — a query, a turn's ask, an episode — and a batch
// API would have each build a slice of one ([BatchEmbedder] is the other
// shape).
//
// # Why the model and the limits are on the narrowest interface
//
// Every holder of an embedder needs both, which is what puts them here rather
// than on a wider interface beside it: a caller storing a vector must tag it
// with the space it is in — two models at one width are two spaces, and a
// vector compared across them ranks nothing — and a caller sending text must
// know where the bound is, to represent a longer text by its own policy
// rather than have it refused.
type Embedder interface {
	// Embed returns the vector for text. An input whose prepared bytes
	// exceed [Limits.InputBytes] is refused before any request
	// ([ErrTooLong]); every failure is classified ([ErrRefused],
	// [ErrTransient], [ErrConfiguration]) where it can be.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Width is the vector width this embedder produces, as configured.
	Width() int

	// Model is the id of the model the vectors come from — the SPACE they
	// are in, which a stored vector is tagged with.
	Model() string

	// Limits is what the model accepts, in bytes of prepared text.
	Limits() Limits
}

// BatchEmbedder embeds many texts in one call, in the order they were given.
//
// # Why this is a second interface rather than a wider Embedder
//
// Most callers embed exactly one thing, and a batch API would have them build a
// slice of one. The knowledge corpus is the caller that changed the arithmetic:
// filling a company's 110 000 sources one at a time is 110 000 round trips,
// which at a tenth of a second apiece does not fit in the tick it runs on, let
// alone in the eight hours a cold fill is budgeted at. Batched it is ≈ 860
// calls covering the same inputs, billed identically because the provider
// bills per input TOKEN.
//
// Kept apart from [Embedder] so a backend that cannot batch is still a
// complete embedder: the caller asks for this interface and falls back to the
// one-at-a-time path, rather than every provider growing a method most of them
// would implement as a loop.
type BatchEmbedder interface {
	Embedder

	// EmbedBatch returns one vector per input, IN ORDER and one-to-one:
	// a caller matches results to inputs positionally, so a provider that
	// dropped an empty input would silently re-file every vector after it
	// onto the wrong document. An input with nothing to embed keeps its
	// slot as a nil vector and is never sent.
	//
	// The inputs are sent in as many requests as the model's limits need
	// ([Limits.Requests]), one after another, and the answer is one
	// ordered result or an error: a request that fails fails the call,
	// and no vector the call already paid for is returned beside the
	// error. Every input is checked against [Limits.InputBytes] BEFORE the
	// first request, so an input past the bound costs no request at all
	// ([TooLongError] names it). A caller that needs to see each request —
	// to report progress between them, or to set aside the one input a
	// provider refuses without losing its neighbours — plans them with
	// [Limits.Requests] and sends each through its own call.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Limits is what one model accepts, in the unit this package counts without a
// tokenizer: BYTES of [Prepare]d text.
//
// A vendor documents its limits in tokens; config.EmbeddingProvider.Limits
// resolves these from them through config.EmbeddingProvider.InputBound, the one
// conversion, and its argument is that every tokenizer these models use
// emits at most one token per byte of its input plus the special tokens a
// server wraps it in — so a text of N bytes is at most N tokens plus
// [Limits.InputOverhead], with no tokenizer anywhere.
//
// THE ZERO VALUE IS REFUSED, never read as "unlimited": [New] will not build
// a provider over it, and [Limits.Validate] says which field is missing. A
// limit of zero would refuse every input; an absent one would send whatever a
// caller had, which is the provider refusing it instead.
type Limits struct {
	// InputBytes is the most prepared bytes one input may hold.
	InputBytes int

	// BatchInputs is the most inputs one request may carry.
	BatchInputs int

	// BatchBytes is the most one request may carry, counting each input as
	// its prepared bytes plus InputOverhead.
	BatchBytes int

	// InputOverhead is what each input costs a request beyond its own
	// bytes: the tokens a server may wrap it in.
	InputOverhead int
}

// Validate refuses limits nothing can be sent under, naming the field.
//
// An input bound below [utf8.UTFMax] cannot hold every character, and one
// input at its bound must fit one request — otherwise the bound admits an input
// no request can carry.
func (l Limits) Validate() error {
	switch {
	case l.InputBytes < utf8.UTFMax:
		return fmt.Errorf("embeddings: an input bound of %d bytes cannot hold "+
			"one character of every script (%d bytes) — the limits are "+
			"incomplete", l.InputBytes, utf8.UTFMax)
	case l.BatchInputs < 1:
		return fmt.Errorf("embeddings: a request must carry at least one input, "+
			"got a limit of %d", l.BatchInputs)
	case l.InputOverhead < 0:
		return fmt.Errorf("embeddings: a negative per-input overhead (%d) is "+
			"not an allowance", l.InputOverhead)
	case l.InputBytes+l.InputOverhead > l.BatchBytes:
		return fmt.Errorf("embeddings: one input at its %d-byte bound costs %d "+
			"of a request that takes %d — no request could carry it",
			l.InputBytes, l.InputBytes+l.InputOverhead, l.BatchBytes)
	}
	return nil
}

// Requests plans how texts are sent: the positions of the inputs with anything
// to embed, in order, grouped so that each group fits one request.
//
// It is how [BatchEmbedder.EmbedBatch] sends a batch, exported for the caller
// that sends each request through its own call — and so it is the ONE packing
// rule rather than a second idea of it beside the provider's.
//
// The groups keep the inputs' order and are cut greedily, which for groups
// that must stay in order is the fewest requests there can be. An input whose
// prepared text is empty is in no group: it is not sent and not billed. An
// input past [Limits.InputBytes] is refused before any group is formed, as a
// [TooLongError] carrying its position.
func (l Limits) Requests(texts []string) ([][]int, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	sizes := make([]int, len(texts))
	for i, text := range texts {
		sizes[i] = len(Prepare(text))
	}
	return l.requests(sizes)
}

// requests is [Limits.Requests] over the prepared sizes, for the callers here
// that have prepared the texts already.
func (l Limits) requests(sizes []int) ([][]int, error) {
	for i, size := range sizes {
		if size > l.InputBytes {
			return nil, &TooLongError{Index: i, Bytes: size, Limit: l.InputBytes}
		}
	}
	var (
		groups [][]int
		group  []int
		used   int
	)
	for i, size := range sizes {
		if size == 0 {
			continue
		}
		cost := size + l.InputOverhead
		if len(group) > 0 && (len(group) == l.BatchInputs || used+cost > l.BatchBytes) {
			groups = append(groups, group)
			group, used = nil, 0
		}
		group = append(group, i)
		used += cost
	}
	if len(group) > 0 {
		groups = append(groups, group)
	}
	return groups, nil
}

// Prepare is the text an embedder is sent for text: every run of whitespace
// collapsed to one space, none at either end, and every byte that is not part
// of a valid UTF-8 encoding replaced by U+FFFD — so the result is always valid
// UTF-8, and it is exactly the text the server decodes from the request.
//
// Collapsing is not cosmetic: the callers pass rendered tasks, pages and
// summaries, all of which carry the newlines and indentation of whatever
// produced them, and an embedding of the same sentence formatted two ways is
// two different vectors. It is the same rule the provider has always applied at
// send time, unchanged to the byte — so a vector already stored for an
// unchanged text is the vector this would compute again.
//
// THE REPLACEMENT IS WHAT THE WIRE ALREADY DID, made visible here. A request
// is JSON, and the SDK encodes every string with encoding/json (its vendored
// copy, openai-go's internal/encoding/json, appendString), which writes
// � in place of each byte that does not begin a valid encoding — so a
// server was never sent an invalid byte, it was sent three bytes of U+FFFD
// for each one. Left to the encoder, every measurement here was of a
// different text from the one embedded: a run of 300 stray continuation
// bytes passed a 400-byte bound as 300 bytes and arrived as 900, a digest was
// of bytes no server saw, and [Chunks] cut a run longer than its bound into
// empty pieces for ever, because no prefix of it is a whole character. ONE
// REPLACEMENT PER BYTE, as the encoder makes them, rather than one per run as
// strings.ToValidUTF8 would: anything else is again not the text sent. Valid
// text — everything that reached the engine through a JSON body, which
// encoding/json had already made valid on the way in — is unchanged, so no
// stored vector and no digest moves.
//
// Exported so a caller can see what is sent: a digest stored beside a vector
// must be over these bytes, and a bound is measured on them ([Limits]).
// Idempotent: Prepare(Prepare(s)) == Prepare(s).
func Prepare(text string) string {
	// Collapsing first and replacing second gives the same text as the
	// other order: an invalid byte is not whitespace to strings.Fields
	// (it decodes as U+FFFD, which is not a space), and neither is the
	// U+FFFD that replaces it.
	return asSent(strings.Join(strings.Fields(text), " "))
}

// asSent is s as encoding/json carries it: each byte that does not begin a
// valid UTF-8 encoding replaced by U+FFFD, one for one, and everything else as
// it is — the mapping of encoding/json's appendString, so that what a bound or
// a digest measures is what the server decodes.
func asSent(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2*utf8.UTFMax)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// A literal U+FFFD in the text decodes with size 3 and
			// is kept; size 1 is a byte that begins no encoding.
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Opening is the prepared opening of text that fits maxBytes: [Prepare]d,
// cut on a rune boundary, and with the space a cut can leave at its end
// removed — so the result is itself prepared, and is EXACTLY the bytes an
// embedder sends for it.
//
// That last property is the reason this exists rather than a cut at each
// caller: a prepared text cut between two words ends in a space that the
// embedder's own preparation then drops, so a digest taken over the cut would
// be of one byte more than was embedded — and two openings that differ only by
// that space would be two digests of one vector.
//
// Whether a text too long for one input is represented by its opening is a
// caller's POLICY — the knowledge corpus does, because a document's opening is
// what a search for it is about, and a turn's ask does not ([EmbedWhole]).
// Unmarked: what this produces is embedding input no reader sees, and a marker
// would be a token in the vector rather than a note about it.
func Opening(text string, maxBytes int) string {
	prepared := Prepare(text)
	if len(prepared) <= maxBytes {
		return prepared
	}
	return strings.TrimRight(textcut.Bytes(prepared, maxBytes), " ")
}

// ErrEmpty reports text with nothing in it.
//
// Its own error because the caller's answer differs: an empty task is not a
// provider problem and must not be logged as one, but it is still no vector.
var ErrEmpty = errors.New("embeddings: nothing to embed")

// The failure classes. Every error the provider returns for a request it
// sent, and every refusal this package makes on its behalf, is errors.Is one
// of these — or none of them, and then it says nothing about the input
// (a response this package refused as malformed, a caller's own cancellation).
var (
	// ErrRefused is a request the provider will refuse again unchanged:
	// HTTP 400, 413 or 422, or an input past the bound ([ErrTooLong]).
	//
	// It says the REQUEST is unacceptable and not which part of it: one
	// input the model cannot take, a request larger than the server
	// accepts, or a setting it rejects whatever the inputs are. Only a
	// smaller request can tell those apart — a caller isolating a poison
	// input confirms it by its neighbours succeeding without it.
	ErrRefused = errors.New("embeddings: the provider refuses this request")

	// ErrTransient is a failure that asking again later may not repeat:
	// HTTP 408, 409, 425, 429 and 5xx, a timeout, a network failure.
	ErrTransient = errors.New("embeddings: the provider could not answer now")

	// ErrConfiguration is a failure no request will avoid until the
	// operator fixes providers.embeddings: HTTP 401, 402, 403, 404 and
	// every other 4xx — a credential, an account, a model or an endpoint
	// — and a vector of a width the store was not sized for.
	ErrConfiguration = errors.New("embeddings: the provider refuses this configuration")

	// ErrTooLong is an input past [Limits.InputBytes], refused here before
	// any request. It is also [ErrRefused]: sent unchanged it would be
	// refused again, by this package instead of the provider.
	ErrTooLong = errors.New("embeddings: input past the model's bound")
)

// TooLongError is an input past the bound, with what a caller needs to act on
// it: which input, how long, and where the bound is.
type TooLongError struct {
	// Model is the model whose bound it is.
	Model string

	// Index is the input's position in the batch, 0 for [Embedder.Embed].
	Index int

	// Bytes is the input's length after [Prepare].
	Bytes int

	// Limit is [Limits.InputBytes].
	Limit int
}

// Error names the bound, the model and the way out.
func (e *TooLongError) Error() string {
	return fmt.Sprintf("embeddings: %s: input %d is %d bytes as it would be "+
		"sent, past the %d one input to this model may hold "+
		"— nothing was sent; embed its opening (embeddings.Opening) or the "+
		"whole of it in pieces (embeddings.EmbedWhole)",
		e.Model, e.Index, e.Bytes, e.Limit)
}

// Is makes a TooLongError both [ErrTooLong] and [ErrRefused].
func (e *TooLongError) Is(target error) bool {
	return target == ErrTooLong || target == ErrRefused
}

// Error is a provider failure, classified.
type Error struct {
	// Model is the model the request was for.
	Model string

	// Status is the HTTP status the provider answered with, or 0 when it
	// answered none (a timeout, a network failure).
	Status int

	// Class is [ErrRefused], [ErrTransient] or [ErrConfiguration].
	Class error

	// Err is the underlying failure.
	Err error
}

// Error says which class and what to do about it.
func (e *Error) Error() string {
	status := ""
	if e.Status != 0 {
		status = fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	var advice string
	switch {
	case errors.Is(e.Class, ErrRefused):
		advice = "it will refuse this request again unchanged — one of its " +
			"inputs, or its size, is what it cannot take, and only a smaller " +
			"request tells which"
	case errors.Is(e.Class, ErrTransient):
		advice = "asking again later may succeed"
	case errors.Is(e.Class, ErrConfiguration):
		advice = "no request will succeed until providers.embeddings is fixed " +
			"(api_key, model, base_url or the account behind them)"
	}
	return fmt.Sprintf("embeddings: %s%s: %v — %s", e.Model, status, e.Err, advice)
}

// Unwrap exposes both the class and the cause to errors.Is and errors.As.
func (e *Error) Unwrap() []error { return []error{e.Class, e.Err} }

// classForStatus is the class of an HTTP status the provider answered with.
//
// Its own table rather than the chat contract's (llm.KindForStatus), because
// the question differs: that one asks whether to bench a credential or fall
// back to another model, and so files a 400 with a 404 as fatal; this one asks
// whether the INPUT is what the provider objects to, which is exactly the
// difference between those two.
func classForStatus(status int) error {
	switch {
	case status == 400 || status == 413 || status == 422:
		return ErrRefused
	case status == 408 || status == 409 || status == 425 || status == 429:
		return ErrTransient
	case status >= 500 && status < 600:
		return ErrTransient
	case status >= 400 && status < 500:
		return ErrConfiguration
	}
	return nil
}

// checkedWidth verifies a returned vector against the configured width.
//
// ON EVERY CALL rather than only the first. A provider behind an aggregator
// can change model mid-deployment, and the failure that catches — rows
// written at the wrong width — is silent and permanent: the store accepts
// the write and the read never matches. One length comparison per call is
// nothing beside the round trip that produced it.
func checkedWidth(vector []float32, want int, model string) ([]float32, error) {
	if len(vector) != want {
		return nil, fmt.Errorf("%w: %s returned a %d-wide vector but "+
			"providers.embeddings.dimensions says %d — the store's columns are "+
			"sized from the config, so these rows could be written and never "+
			"read back", ErrConfiguration, model, len(vector), want)
	}
	return vector, nil
}
