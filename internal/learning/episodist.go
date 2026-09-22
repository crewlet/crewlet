package learning

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// EpisodistSource names the worker in a pass result and in its logs.
const EpisodistSource = "episodist"

// A TASK SUMMARY IS EMBEDDED WHOLE, IN WINDOWS.
//
// It used to be cut at its first 8 000 bytes, by a call to textcut.Bytes that
// appended no marker, wrote nothing to the row and logged nothing — under a
// comment claiming the cap was in CHARACTERS while the function it called
// counted BYTES. A summary is not bounded anywhere upstream (a coalesced
// trigger merges N messages, a vendor's subject line is whatever the vendor
// sent), so everything past that cut reached no vector, and therefore no
// `query_episodes` result and no `## Similar prior work` prefetch. What was
// lost was SEARCHABILITY and never the text: `task_summary` holds the whole
// summary, and [Episodes.Recent], [Episodes.ForConversation] and the recent
// and conversation modes of `query_episodes` all return it in full.
//
// The fix is the one internal/search made for the knowledge corpus — split
// into overlapping windows, embed every one, score the row as its NEAREST
// window — and the reasoning transfers whole, because the failure did: see
// search.EmbedChunkBytes and schema/replicated/0015. What is NOT shared is the
// numbers themselves. They are a judgement about a corpus, this one is a
// seat's own turn summaries rather than a company's handbooks, and a
// dependency from a seat's private memory onto the knowledge-search estate
// would be a far larger thing to carry than two constants that are allowed to
// disagree.
const (
	// EpisodeWindowBytes is the window one vector represents, in BYTES —
	// which is what this layer can count, and what the constant that
	// preceded it claimed not to be.
	//
	// FOUR KIBIBYTES, bounded from both sides. A window is one vector and a
	// vector represents its window's SUBJECT, so a window holding four
	// unrelated messages of a coalesced trigger represents none of them,
	// which is the failure a bigger one has. A smaller one costs vectors
	// and provider calls: an ordinary trigger summary is a line or two and
	// stays exactly ONE window, so the bill and the row size rise only for
	// the long summaries that were not searchable past their first 8 000
	// bytes at all. It is also far inside every embedding model's own input
	// limit — roughly a thousand tokens of English against the eight
	// thousand the narrowest takes — so nothing here has to know which
	// model a company wired.
	EpisodeWindowBytes = 4 << 10

	// EpisodeWindowOverlap is how much of the previous window each one
	// repeats.
	//
	// A sentence boundary does not fall where a byte count says. Without an
	// overlap a thought straddling two windows is represented well by
	// neither — half of it in each vector — and a query about it matches
	// neither. 512 bytes is about a long paragraph, and an eighth of the
	// window is the proportion that costs one extra window per 8 KiB of
	// summary rather than a meaningful share of the bill.
	EpisodeWindowOverlap = 512
)

// AN OVERLAP AT OR PAST HALF THE WINDOW IS A COMPILE ERROR, not a run-time
// guard, because that is what it takes for episodeWindows to terminate: the
// stride has to outrun both the overlap the word-edge walk may give back and
// the three bytes rune alignment may give back after it. Written as a check
// somebody could forget to run, this would be an infinite loop inside a
// background worker holding a seat's reflect pass — found in production, by
// the seat that stopped answering. A negative constant expression cannot be
// converted to uint, so this line simply fails to build.
const _ = uint(EpisodeWindowBytes - 2*EpisodeWindowOverlap - 4)

// Embed turns text into a vector, or reports that it cannot.
//
// A FUNCTION rather than the provider interface: it is the one thing this
// worker wants from embeddings, and taking the interface would make every
// test that writes an episode implement a Width it never reads.
type Embed func(ctx context.Context, text string) ([]float32, error)

// Episodist records one completed turn as an episode.
//
// # Why this is a worker and not a write at the end of the turn
//
// An episode is what a seat DID, and the turn engine already knows that as
// the turn ends — so writing it there would be one fewer hop. It is a worker
// because the write must be gated on the same questions the other learning
// passes are gated on (did the turn settle, did the agent engage), and a
// gate that lives in the turn engine is a gate no operator can turn off and
// no pass result reports on. The episode table is the substrate the whole
// read side runs on; a company whose recall is empty needs to be able to see
// WHICH gate closed.
//
// # It writes even when there is no vector
//
// [Episode.Embeddings] is nil when the embedder was unreachable, and the row
// still lands: recall skips such rows while the time-window and outcome
// queries still surface them, and the lifecycle worker's clustering falls
// back to its token overlap. A transient embedding outage must never cost an
// episode — the row cannot be reconstructed later, and the vector can, by
// nothing more than a re-embed.
type Episodist struct {
	episodes *Episodes
	embed    Embed
	timeout  time.Duration
	now      func() time.Time
	newID    func() string
}

// EpisodistOptions configures the worker.
type EpisodistOptions struct {
	// Embed is the vector backend, or nil for a company with none.
	Embed Embed

	// EmbedTimeout bounds one episode's whole embedding, across every
	// window of its summary. Zero takes [DefaultEmbedTimeout].
	EmbedTimeout time.Duration

	Now   func() time.Time
	NewID func() string
}

// DefaultEmbedTimeout bounds one EPISODE's embedding, however many windows it
// takes.
//
// The same 15 seconds the provider defaults to, applied again HERE because
// this caller has its own reason for it: an episode write must not sit on a
// slow provider while the reflect dispatcher's other workers wait behind it,
// and a nil vector is a supported outcome. The bound is what makes "no
// vector" reachable instead of "no episode".
//
// THE BUDGET IS THE EPISODE'S, not one provider call's, which is the unit that
// sentence was always about — and it is the only bound on how much work one
// summary can ask for, since the window count deliberately has no ceiling. At
// the 100–400 ms an embedding round trip costs, 15 s covers something like
// forty to a hundred and fifty sequential windows, which is 160 KiB to 600 KiB
// of summary: orders of magnitude past any trigger this engine has seen, and
// the arithmetic is written here so the next reader can redo it rather than
// re-guess the number.
//
// What a spent budget costs is the windows not yet reached, recorded as the
// row's window count and named in `episode_embedding_partial`. That is a
// degradation where the single-vector shape had a cliff: the whole vector was
// lost, and with it the seat's every similarity hit on that turn.
const DefaultEmbedTimeout = 15 * time.Second

// NewEpisodist builds the worker over an episode store.
func NewEpisodist(e *Episodes, opts EpisodistOptions) (*Episodist, error) {
	if e == nil {
		return nil, fmt.Errorf("learning: the episodist needs an episode store to write to")
	}
	w := &Episodist{
		episodes: e, embed: opts.Embed,
		timeout: opts.EmbedTimeout, now: opts.Now, newID: opts.NewID,
	}
	if w.timeout <= 0 {
		w.timeout = DefaultEmbedTimeout
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.newID == nil {
		w.newID = uuid.NewString
	}
	return w, nil
}

// Name implements [Worker].
func (w *Episodist) Name() string { return EpisodistSource }

// Skip implements [Worker].
//
// The two gates the persist decider does NOT share are the interesting part.
// It skips a self-persisted turn because the fact is already in the diary;
// an episode is a different record of a different thing — what the seat did,
// not what it concluded — so a turn that wrote its own memory still earns
// one. It runs on a FAILED turn for the same reason: an episode of work that
// did not land is exactly what recall should surface next time the seat is
// asked to do it again.
func (w *Episodist) Skip(t Turn) string {
	if !t.Settled() {
		// A self_iterate round is work the agent itself judged
		// incomplete; the turn will reattempt, and the reattempt is the
		// episode.
		return "non_terminal"
	}
	if !t.Engaged() {
		// The turn opted out, or was coerced to direct and called
		// nothing. There is no work here to remember, and a row saying
		// otherwise would weight every later recall with a turn in which
		// the seat did nothing.
		return "no_engagement"
	}
	if t.Event.AgentHandle == "" {
		// Episodes are keyed on the seat. An unkeyed row is one no
		// recall can ever find, so it is not worth writing.
		return "no_handle"
	}
	return ""
}

// Reflect implements [Worker].
func (w *Episodist) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	ep := w.episodeOf(t)
	ep.Embeddings = w.vector(ctx, ep.TaskSummary)

	written, err := w.episodes.Append(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("learning: append episode for %s: %w", ep.Handle, err)
	}
	if !written {
		// A DUPLICATE IS NOT AN EVENT. The work key already had a row, so
		// nothing was written, and publishing episode_written for it
		// would report the same turn twice to every surface that counts
		// them — which is the exact miscount the unique index exists to
		// prevent.
		log.DebugContext(ctx, "episode_already_recorded", "seat", ep.Handle, "work_key", ep.WorkKey)
		return nil, nil
	}
	return []events.Payload{types.EpisodeWritten{
		Agent:         t.Event.Agent,
		AgentHandle:   t.Event.AgentHandle,
		RoleName:      t.Event.RoleName,
		TurnID:        t.Event.TurnID,
		WorkKey:       t.WorkKey(),
		ReviewOutcome: t.Event.ReviewOutcome,
		DurationMS:    t.Event.DurationMS,
		ToolCount:     len(t.Event.ToolSequence),
	}}, nil
}

// episodeOf projects a completed turn onto a raw episode row.
func (w *Episodist) episodeOf(t Turn) Episode {
	ended := t.Event.EndedAt
	if ended.IsZero() {
		ended = w.now()
	}
	started := t.Event.StartedAt
	if started.IsZero() {
		started = ended.Add(-time.Duration(t.Event.DurationMS) * time.Millisecond)
	}
	return Episode{
		ID:     w.newID(),
		Handle: t.Event.AgentHandle,
		Role:   t.Event.RoleName,
		TaskID: t.Event.TaskID,
		// TWO DIFFERENT IDENTITIES, and they stopped being the same
		// value when a turn id started naming one RUN: TurnID is the
		// execution that produced this row, and WorkKey is the unit of
		// work it did — which is what the unique index collapses, so a
		// redelivered trigger that runs twice still leaves one episode.
		// See [Turn.WorkKey] and ADR-0017. (A COMPACTED row has a work
		// key derived from the episodes it folded and no turn at all.)
		TurnID:          t.Event.TurnID,
		WorkKey:         t.WorkKey(),
		StartedAt:       started.UTC(),
		EndedAt:         ended.UTC(),
		Duration:        time.Duration(t.Event.DurationMS) * time.Millisecond,
		PlanSummary:     t.Event.PlanSummary,
		TaskSummary:     t.Event.TaskSummary,
		ToolSequence:    t.Event.ToolSequence,
		SkillsUsed:      t.Event.SkillsUsed,
		ReviewOutcome:   t.Event.ReviewOutcome,
		ConversationKey: t.Event.ConversationKey,
		Kind:            KindRaw,
		Count:           1,
	}
}

// vector embeds the task summary WHOLE, one vector per window, or reports
// none.
//
// NEVER an error: see the type comment. The failure is logged where it
// happens and the row is written with whatever vectors were made — none
// included.
//
// A FAILED WINDOW DOES NOT COST THE OTHERS. The provider fails per call, and
// the failures that reach here are transient by nature (a rate limit, a
// timeout, one bad response), so abandoning the set on the first one would
// throw away the windows already paid for and hand the row the same empty
// recall the cut used to. What it must not do is hide it: the count of what
// landed is written to the row and `episode_embedding_partial` names both
// numbers, so a partly-embedded summary is a fact a reader can see rather than
// a result they cannot explain.
func (w *Episodist) vector(ctx context.Context, summary string) [][]float32 {
	if w.embed == nil || summary == "" {
		return nil
	}
	windows := episodeWindows(summary)
	// ONE DEADLINE FOR THE WHOLE SET — see [DefaultEmbedTimeout] for why
	// the episode rather than the call is the unit it bounds.
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	out := make([][]float32, 0, len(windows))
	var failure error
	for _, window := range windows {
		vector, err := w.embed(ctx, window)
		if err != nil {
			failure = err
			if ctx.Err() != nil {
				// The budget is spent, so every window after this
				// one would fail the same way and pay a round trip
				// to find out.
				break
			}
			continue
		}
		out = append(out, vector)
	}
	if len(out) == 0 {
		if failure != nil {
			log.WarnContext(ctx, "episode_embedding_failed", "error", failure.Error(),
				"windows", len(windows),
				"detail", "the episode is written without a vector; recall skips "+
					"it while the time-window queries still surface it")
		}
		// NIL RATHER THAN AN EMPTY SLICE, which is the difference between
		// "there is no embedding" and "there is an embedding of nothing":
		// only the first is what [Episode.Embeddings] documents and what
		// every reader of it tests for. A summary that is nothing but
		// whitespace reaches here without a failure at all.
		return nil
	}
	if failure != nil {
		log.WarnContext(ctx, "episode_embedding_partial", "error", failure.Error(),
			"windows_embedded", len(out), "windows", len(windows),
			"detail", "recall reaches the part of task_summary that was "+
				"embedded; the whole summary is still on the row and in "+
				"query_episodes' recent and conversation modes")
	}
	return out
}

// episodeWindows splits a summary into the windows one vector each represents.
//
// NOTHING IS CUT: the windows COVER the text, so every byte reaches some
// vector. That is the invariant, and it is the one the tests are written
// against rather than against this arithmetic.
//
// CUT ON A WORD BOUNDARY where one is near, and always on a RUNE boundary. A
// plain slice splits a multi-byte rune, and the invalid UTF-8 that produces is
// substituted by the JSON encoder and reaches the provider as a replacement
// character inside the text it is meant to represent — which is the rule
// internal/textcut exists to stop nine call sites re-deriving, and this is the
// one shape it does not offer, because a window is not a cut.
//
// A summary that FITS is one window, returned verbatim with no overlap — which
// is byte-for-byte what this worker sent before windowing existed, on the
// short summaries that are almost all of them.
//
// NOTHING LEADS A WINDOW, which is the one thing internal/search does that this
// does not, and the difference is in the corpus rather than in the judgement.
// There a document is a TITLE and a BODY, so a vector for page four of a
// runbook would otherwise have no idea which runbook it is page four of, and
// repeating the title is what keeps a deep window about the document it is in.
// An episode's embedded text is ONE field: the trigger's summary, prose with no
// title beside it and no field anywhere on the row that names its subject. The
// nearest thing would be its own first line, and promoting that to a heading
// repeated on every window would be a guess about a structure the value does
// not have — every window then partly about the guess. What carries across a
// boundary here is [EpisodeWindowOverlap] and nothing else.
//
// THERE IS NO CEILING ON THE NUMBER OF WINDOWS, deliberately: a cap here would
// be the silent cut this replaced, one magnitude further out where nobody would
// find it. What bounds the work is [DefaultEmbedTimeout], which bounds TIME
// rather than TEXT — and a spent budget costs the last windows of one
// pathological summary rather than a fixed prefix of every long one, records
// how many landed on the row, and says so in the log.
func episodeWindows(summary string) []string {
	if summary == "" {
		return nil
	}
	if len(summary) <= EpisodeWindowBytes {
		return []string{summary}
	}
	// THE LOOP ALWAYS ADVANCES, and it is arithmetic rather than a guard:
	// episodeWordEdge never walks a window's end back further than
	// EpisodeWindowOverlap, and [alignRuneStart] gives back at most
	// runeAlignBack bytes after it, so each pass moves `at` forward by at
	// least EpisodeWindowBytes - 2*EpisodeWindowOverlap - runeAlignBack —
	// 3 069 bytes at these constants. Both halves of that have to be BOUNDED
	// for it to be arithmetic at all: an unbounded "walk back to a rune
	// start" is what made this loop stand still on a summary that was not
	// valid UTF-8, and the assertion above is a compile error about the
	// stride rather than a guard anybody has to run.
	stride := EpisodeWindowBytes - EpisodeWindowOverlap
	var out []string
	for at := 0; at < len(summary); {
		end := min(at+EpisodeWindowBytes, len(summary))
		end = episodeWordEdge(summary, at, end)
		if window := strings.TrimSpace(summary[at:end]); window != "" {
			out = append(out, window)
		}
		if end >= len(summary) {
			break
		}
		next := at + stride
		// NEVER SKIPS. A word edge that walked this window's end back
		// past the stride would otherwise start the next one AFTER it,
		// and the bytes in between would be in no window at all — the
		// cut this replaced, wearing a different shape.
		if next > end {
			next = end
		}
		// A WINDOW STARTS ON A RUNE BOUNDARY as well as ending on one,
		// and only the end is obvious. The stride is a byte count, so a
		// start derived from it lands inside a multi-byte character
		// about two times in three on text that is not ASCII — and
		// text[at:] from there is invalid UTF-8 exactly as a naive cut
		// would be, which is the whole failure this windowing exists to
		// avoid reintroducing. Backing up rather than forward is what
		// keeps the guarantee: the bytes it re-covers are inside the
		// previous window, where skipping forward could step over the
		// last ones it did not reach.
		at = alignRuneStart(summary, next, at)
	}
	return out
}

// episodeWordEdge walks a window's end back to the nearest space, and never
// past a rune.
//
// The lookback is bounded so a window with no whitespace in it — a base64
// blob, a minified line, a stack trace with no spaces — is cut where it was
// asked to be rather than collapsing to nothing.
func episodeWordEdge(text string, from, to int) int {
	if to >= len(text) {
		return len(text)
	}
	limit := max(from+1, to-EpisodeWindowOverlap)
	for at := to; at > limit; at-- {
		if text[at] == ' ' || text[at] == '\n' {
			return at
		}
	}
	// No word boundary in reach: back up to a rune boundary, which is not
	// optional — invalid UTF-8 reaches the provider as a replacement
	// character inside the text it is meant to represent.
	return alignRuneStart(text, to, from)
}

// runeAlignBack is how far either end of a window may be walked back to land
// on a rune boundary: [utf8.UTFMax] - 1 bytes, which is the longest run of
// continuation bytes a VALID rune can put in front of one.
//
// A BOUND AND NOT A SEARCH, and the difference is whether this package
// terminates. Written as "walk back until RuneStart", the walk has no floor of
// its own on text that is not valid UTF-8 — a run of continuation bytes longer
// than a window (a raw vendor body, a latin-1 subject, anything binary that
// reached a trigger summary) hands the whole window back, episodeWindows'
// no-skip clamp then pins the next window's start to the current one's, and
// the loop spins for ever inside the reflect pass that holds a seat's episode
// write. That is not hypothetical: it is what this did before the bound, found
// by pointing a test at 5 000 bytes of 0x80.
//
// Past this many bytes there is no rune to preserve — the text is ALREADY
// invalid there, whatever this function does — so the honest answer is to keep
// the byte arithmetic and let the invalid sequence reach the provider exactly
// as it arrived, rather than to keep looking for a boundary that is not in the
// string. Nothing is lost by that: every byte is still inside some window.
const runeAlignBack = utf8.UTFMax - 1

// alignRuneStart moves an index back onto the rune boundary at or below it,
// giving up after [runeAlignBack] bytes and answering the index it was given.
//
// `floor` is how far back the caller can afford to go; callers pass the
// window's own start, so an alignment can never cross into the window before
// it. The index is an index INTO text and below its length, which both callers
// guarantee — a window end past the string has already been answered whole.
func alignRuneStart(text string, at, floor int) int {
	for back := at; back >= floor && at-back <= runeAlignBack; back-- {
		if utf8.RuneStart(text[back]) {
			return back
		}
	}
	return at
}
