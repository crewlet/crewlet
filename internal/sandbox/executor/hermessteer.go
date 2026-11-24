package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// HermesSteerInstruction is the standing instruction about notes a Hermes run
// is given. The runner's plugin steers each note into the running turn
// (`session.steer`), and Hermes 0.23.3 adds it after the newest tool result as
// a user message it frames as out of band (measured).
const HermesSteerInstruction = steerInstructionHead +
	"Hermes adds it to the conversation between your steps, as a message from the user after the " +
	"tool results of a step, framed as an out-of-band message. " + steerInstructionTail

// NoteInstruction is the standing instruction about notes a Hermes run is
// given: it takes a person's notes between its steps through the runner's
// plugin ([hermesPluginSource]).
func (Hermes) NoteInstruction() string { return HermesSteerInstruction }

// notesOffWhy is what can switch a Hermes run's reading of notes off.
func (Hermes) notesOffWhy() string {
	return "a plugin that could not reach the run's notes route or the run's gateway, which the box's " +
		"Hermes gateway log names"
}

// inboxID is the id a note is admitted under for a job: `note_` and 32 hex
// digits of a hash of the job and the note. Hermes records no id of the
// engine's, so the confirm matches by the item's digest instead
// ([Hermes.notesHeld]).
func (Hermes) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "note_" + hex.EncodeToString(sum[:])[:32]
}

// hermesPollCeiling is the most UTF-16 units one note the plugin steers may
// carry: 4,505, the engine's item bound (internal/engine's maxLateItemUnits)
// the note contract holds every poll runner to.
const hermesPollCeiling = 4505

// pollCeiling is [hermesPollCeiling].
func (Hermes) pollCeiling() int { return hermesPollCeiling }

// hermesNoteOpen and hermesNoteClose frame a steered note in the user message
// Hermes 0.23.3 records it as (measured): the note's text between them, byte
// for byte.
const (
	hermesNoteOpen = "[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this " +
		"position; not tool output and not a new delivery when replayed from conversation history]\n"
	hermesNoteClose = "\n[/OUT-OF-BAND USER MESSAGE]"
)

// hermesSteered is the note a user message of the record carries, and false
// for a message Hermes did not frame as a steered note.
func hermesSteered(text string) (string, bool) {
	inner, ok := strings.CutPrefix(text, hermesNoteOpen)
	if !ok {
		return "", false
	}
	if note, closed := strings.CutSuffix(inner, hermesNoteClose); closed {
		return note, true
	}
	return "", false
}

// notesHeld is the inbox ids of takes, in serve order, that Hermes's record
// of session shows the model was sent in this launch ([hermesNotesHeld]). The
// record is the box's session store, dumped as the pack dumps it
// ([hermesCarryScript]); where the launch began is the box's record of it
// ([Paths.HermesRunStart]). The plugin never confirms, so the record is the
// one confirm path ([recordConfirmer]).
func (Hermes) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {
	if !sandbox.ValidSessionID(session.ID) {
		return nil, fmt.Errorf("executor: %q is no Hermes session the run named", session.ID)
	}
	from, err := hermesRunFrom(ctx, box, session.ID)
	if err != nil {
		return nil, err
	}
	paths := PathsFor(box)
	q := shellword.Quote
	read := paths.HermesRecordRead()
	res, err := box.Exec(ctx, hermesCarryRun(paths)+fmt.Sprintf("hcarry dump %s %s /dev/null %s",
		q(paths.HermesStateDB()), q(session.ID), q(read)), sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: reading Hermes's record of session %s: %w", session.ID, err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "crewlet-carry ok" {
		return nil, fmt.Errorf("executor: reading Hermes's record of session %s answered %q: %s", session.ID,
			firstLine(got, transcriptDetailLimit), firstLine(res.Stderr, transcriptDetailLimit))
	}
	body, err := box.OpenFile(ctx, read)
	if err != nil {
		return nil, fmt.Errorf("executor: opening Hermes's record of session %s: %w", session.ID, err)
	}
	defer func() { _ = body.Close() }()
	held, err := hermesNotesHeld(ctx, body, takes, from)
	if err != nil {
		return nil, fmt.Errorf("executor: reading Hermes's record of session %s: %w", session.ID, err)
	}
	return held, nil
}

// hermesRunFrom is the newest message id the store held as the box's last
// launch began, off its record ([Paths.HermesRunStart]). One that cannot be
// read, does not parse or names another session is an error, never 0: from 0,
// an earlier launch's rows would confirm a note this launch lost, so the take
// stays undecided instead.
func hermesRunFrom(ctx context.Context, box sandbox.Sandbox, session string) (int64, error) {
	path := PathsFor(box).HermesRunStart()
	raw, err := box.ReadFile(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("executor: reading where the run began in its Hermes session (%s): %w", path, err)
	}
	id, count, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	n, atoiErr := strconv.ParseInt(count, 10, 64)
	switch {
	case !ok || atoiErr != nil || n < 0:
		return 0, fmt.Errorf("executor: %s says %q, not a session and a message id", path, raw)
	case id != session:
		return 0, fmt.Errorf("executor: %s names the session %q, and the run's is %s: the box ran another "+
			"session since", path, id, session)
	}
	return n, nil
}

// hermesNotesHeld is the inbox ids of takes (the launch's serves in order,
// read notes' included, [sandbox.MainThreadAsk.Served]) whose items the record
// r holds as notes the model was sent, read on the notes walk, holding no row.
// Each user row of this launch (above from, no copy of an earlier row) in the
// conversation ([hermesNoteRow.current]), shown as a steer and framed, holds
// the notes steered before one request, joined by newlines (measured on
// 0.23.3), matched to takes in order ([hermesSteerParse]). A match is settled
// only by a finished reply stored after it ([hermesNoteRow.replied]; the note
// rides the request after the tool result, testdata request-steered.json);
// one no reply follows is not held, so it is served again. A row past
// [hermesRowMaxBytes] drops every pending match.
func hermesNotesHeld(ctx context.Context, r io.Reader, takes []ServedTake, from int64) ([]string, error) {
	type match struct {
		inbox string
		row   int64
	}
	var settled []string
	var pending []match
	next := 0
	// A COMPACTION'S REWRITTEN TAIL keeps each row's message_uid under a new
	// id (measured on 0.23.3), in place or in a rotated child whose parent's
	// rows stay current, so a copy of a row read or of an earlier launch's is
	// passed over, however high its id: each uid kept as a 16-byte digest.
	earlier := map[[16]byte]struct{}{}
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes), hermesRowMaxBytes)
	var l hermesNoteRow
	l.parse.setup(takes)
	for {
		l.start(next)
		line, err := w.noteLine(ctx, func() error { return l.read(&w) })
		switch {
		case err != nil:
			return settled, err
		case line.none:
			return settled, nil
		case line.past:
			pending = pending[:0]
		case line.object && l.row():
			switch {
			case l.id <= from:
				hermesCopy(earlier, l.uid)
			case !l.current() || hermesCopy(earlier, l.uid):
			case l.replied():
				kept := pending[:0]
				for _, p := range pending {
					if p.row < l.id {
						settled = append(settled, p.inbox)
					} else {
						kept = append(kept, p)
					}
				}
				pending = kept
			case l.role == "user" && l.display == "steer":
				for _, at := range l.parse.notes() {
					pending = append(pending, match{takes[at].InboxID, l.id})
					next = at + 1
				}
			}
		}
		if line.ended {
			return settled, nil
		}
	}
}

// hermesNoteRow is what a collection's read keeps of one line of the record:
// its table, and the `messages` row's members a note's receipt reads, each by
// its field's kind ([fieldKind]); a line one refuses is no row.
type hermesNoteRow struct {
	table               string
	mistyped            bool
	rowSet, rowAny      bool
	rowMistyped         bool
	id                  int64
	inactive, compacted bool
	summary             bool
	finished            bool
	display, role       string
	uid                 recordWord
	parse               hermesSteerParse
	// The content list's part being read: its kind, how many kinds and
	// texts it wrote, and whether it wrote a string text.
	partKind             string
	partKinds, partTexts int
	partFed              bool
}

// start starts the line, its notes matched from the take at base.
func (l *hermesNoteRow) start(base int) {
	l.table, l.mistyped, l.rowSet, l.rowAny = "", false, false, false
	l.rowStart()
	l.parse.reset(base)
}

// rowStart starts the line's row: one written again replaces it.
func (l *hermesNoteRow) rowStart() {
	l.rowMistyped, l.id, l.inactive, l.compacted, l.summary = false, 0, false, false, false
	l.finished, l.display, l.role, l.uid = false, "", "", recordWord{}
	l.parse.reset(l.parse.base)
}

// row reports whether the line is a `messages` row its decode takes.
func (l *hermesNoteRow) row() bool {
	return !l.mistyped && l.table == "messages" && l.rowSet && l.rowAny && !l.rowMistyped
}

// current is whether the row is one of the conversation Hermes goes on with,
// as it was said: not a row it set inactive or compacted (an in-place
// compaction deactivates the rows it folds and writes their kept tail again
// under new ids, measured on 0.23.3, testdata session-compacted.jsonl), nor its
// summary, a user row of its own. A row with no flags is.
func (l *hermesNoteRow) current() bool { return !l.inactive && !l.compacted && !l.summary }

// replied is whether the row is a reply the model finished: an assistant row
// with a finish reason, never a summary or one cut off.
func (l *hermesNoteRow) replied() bool { return l.role == "assistant" && l.finished }

// read reads a line's envelope, its opening brace current.
func (l *hermesNoteRow) read(w *lineWalk) error {
	return w.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "t":
			table, ok, err := w.wordField(k)
			l.table, l.mistyped = table, l.mistyped || !ok
			return err
		case "r":
			l.rowStart()
			l.rowSet, l.rowAny = true, k == jsonscan.ObjectStart || k == jsonscan.Null
			if k != jsonscan.ObjectStart || l.table != "" && l.table != "messages" {
				return w.scan.Skip()
			}
			return w.members(func(name string, k jsonscan.Kind) error { return l.member(w, name, k) })
		}
		return w.scan.Skip()
	})
}

// member reads one member of a `messages` row.
func (l *hermesNoteRow) member(w *lineWalk, name string, k jsonscan.Kind) error {
	var ok bool
	var err error
	var n int64
	switch name {
	case "id":
		l.id, ok, err = w.intField(k)
	case "active":
		n, ok, err = w.intField(k)
		l.inactive = k == jsonscan.Number && n == 0
	case "compacted":
		n, ok, err = w.intField(k)
		l.compacted = n != 0
	case "_compressed_summary":
		n, ok, err = w.intField(k)
		l.summary = n != 0
	case "finish_reason":
		var reason recordWord
		reason, ok, err = w.idField(k)
		l.finished = k == jsonscan.String && !reason.empty()
	case "display_kind":
		l.display, ok, err = w.wordField(k)
	case "role":
		l.role, ok, err = w.wordField(k)
	case "message_uid":
		l.uid, ok, err = w.idField(k)
	case "content":
		ok, err = true, l.content(w, k)
	case "tool_calls", "timestamp":
		return w.scan.Skip()
	case "tool_call_id", "tool_name":
		ok, err = w.typedField(k, fieldString)
	default:
		return w.scan.Skip()
	}
	l.rowMistyped = l.rowMistyped || !ok
	return err
}

// content reads a row's content into the steer's parse: a string's words, or
// a list's `text` parts; a list its decode refuses is no words, and one with
// a part whose string text is not written once after its one type is parsed
// as none, which holds no note.
func (l *hermesNoteRow) content(w *lineWalk, k jsonscan.Kind) error {
	p := &l.parse
	p.reset(p.base)
	switch k {
	case jsonscan.String:
		if err := w.read(p); err != nil {
			return err
		}
		p.end()
		return nil
	case jsonscan.ArrayStart:
	default:
		p.none = true
		return w.scan.Skip()
	}
	took := true
	err := w.elements(func(k jsonscan.Kind) (bool, error) {
		l.partKind, l.partKinds, l.partTexts, l.partFed = "", 0, 0, false
		ok, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
			switch name {
			case "type":
				kind, ok, err := w.wordField(k)
				l.partKind, l.partKinds, took = kind, l.partKinds+1, took && ok
				return err
			case "text":
				l.partTexts++
				switch k {
				case jsonscan.String:
				case jsonscan.Null:
					return nil
				default:
					took = false
					return w.scan.Skip()
				}
				l.partFed = true
				switch {
				case l.partKinds != 1:
					p.unsure = true
				case l.partKind == "text" && l.partTexts == 1:
					return w.read(p)
				}
				return w.scan.Skip()
			}
			return w.scan.Skip()
		})
		took = took && ok
		if l.partFed && (l.partKinds != 1 || l.partTexts != 1) {
			p.unsure = true
		}
		return true, err
	})
	p.none = !took
	p.end()
	return err
}

// hermesNoteMaxBytes is the longest note a steered row's part can hold:
// [hermesPollCeiling] UTF-16 units, at most three bytes a unit in UTF-8 (a
// pair of units is one character of four), so a longer part matches no take.
const hermesNoteMaxBytes = 3 * hermesPollCeiling

// hermesSteerParse matches one steered row's notes to takes as its words are
// read, holding none of them: the words are a note framed
// ([hermesSteered]), its inner text parts joined by newlines, each ending at
// a newline or at its end. Parts are matched as an in-order subsequence of
// takes: a parse of the whole text wins over one of a prefix, and among
// equals the earliest takes win, so a note's own newlines are tried past and
// takes ["fix\nthen push", "fix"] read "fix\nthen push" as the first, never as
// "fix" with an unread tail; where no parse takes the whole text, the longest
// parsed prefix holds what it matched. Only a part beginning where a parse
// can reach is digested, and only to [hermesNoteMaxBytes].
type hermesSteerParse struct {
	digests map[[sha256.Size]byte][]int
	takes   int
	base    int
	// The framing: its opening read so far, the tail held back for its
	// close, and whether the words are steered at all.
	head, tail      []byte
	opened, refused bool
	// none says the row's words are none (a list its decode refused), and
	// unsure that a part was written so its text could not be parsed.
	none, unsure bool
	// pos is how much of the inner text was read, starts the parts being
	// digested, reach the least take each reachable start may match, and
	// ends each match's end by its start and take.
	pos    int64
	starts []hermesPartStart
	reach  map[int64]int
	ends   map[hermesPartKey]int64
	length int64
	ended  bool
}

// hermesPartStart is one part being digested: where it begins, and its
// digest so far.
type hermesPartStart struct {
	at int64
	h  hash.Hash
}

// hermesPartKey is a part's start and the take it matched.
type hermesPartKey struct {
	at   int64
	take int
}

// setup makes p a parse against takes.
func (p *hermesSteerParse) setup(takes []ServedTake) {
	p.digests, p.takes = map[[sha256.Size]byte][]int{}, len(takes)
	for i, t := range takes {
		var sum [sha256.Size]byte
		if raw, err := hex.DecodeString(t.Digest); err == nil && len(raw) == sha256.Size {
			copy(sum[:], raw)
			p.digests[sum] = append(p.digests[sum], i)
		}
	}
	p.reach, p.ends = map[int64]int{}, map[hermesPartKey]int64{}
}

// reset starts a parse of a row's words, its first part matched from the take
// at base.
func (p *hermesSteerParse) reset(base int) {
	p.base, p.head, p.tail = base, p.head[:0], p.tail[:0]
	p.opened, p.refused, p.none, p.unsure, p.ended = false, false, false, false, false
	p.pos, p.length, p.starts = 0, 0, p.starts[:0]
	clear(p.reach)
	clear(p.ends)
}

// Write reads the words' next bytes: the framing's opening, then the inner
// text, its last bytes held back for the close.
func (p *hermesSteerParse) Write(b []byte) (int, error) {
	n := len(b)
	if p.refused {
		return n, nil
	}
	if !p.opened {
		k := min(len(hermesNoteOpen)-len(p.head), len(b))
		p.head, b = append(p.head, b[:k]...), b[k:]
		switch {
		case hermesNoteOpen[:len(p.head)] != string(p.head):
			p.refused = true
			return n, nil
		case len(p.head) < len(hermesNoteOpen):
			return n, nil
		}
		p.opened = true
		p.begin(0, p.base)
	}
	p.tail = append(p.tail, b...)
	if cut := len(p.tail) - len(hermesNoteClose); cut > 0 {
		p.inner(p.tail[:cut])
		p.tail = p.tail[:copy(p.tail, p.tail[cut:])]
	}
	return n, nil
}

// begin starts a part at the inner text's position at, reached by a parse
// that matches from take from on: digested where a take is left to match.
func (p *hermesSteerParse) begin(at int64, from int) {
	p.reach[at] = from
	if from < p.takes {
		p.starts = append(p.starts, hermesPartStart{at: at, h: sha256.New()})
	}
}

// inner reads the inner text's next bytes into every part being digested,
// each part ending at a newline, where a part matched begins the next.
func (p *hermesSteerParse) inner(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.digest(b)
			return
		}
		p.digest(b[:i])
		next, matched := p.partsEnd()
		p.digest(b[i : i+1])
		if matched {
			p.begin(p.pos, next)
		}
		b = b[i+1:]
	}
}

// digest writes b into every part being digested, letting go of each that
// would run past [hermesNoteMaxBytes].
func (p *hermesSteerParse) digest(b []byte) {
	p.pos += int64(len(b))
	kept := p.starts[:0]
	for _, s := range p.starts {
		if p.pos-s.at > hermesNoteMaxBytes {
			continue
		}
		_, _ = s.h.Write(b)
		kept = append(kept, s)
	}
	p.starts = kept
}

// partsEnd ends every part being digested at the inner text's position,
// keeping each take it matches that a parse reaching its start may match,
// and reports the least take a part beginning past it may match, and whether
// any matched.
func (p *hermesSteerParse) partsEnd() (int, bool) {
	next, matched := p.takes, false
	for _, s := range p.starts {
		var sum [sha256.Size]byte
		s.h.Sum(sum[:0])
		for _, take := range p.digests[sum] {
			if take < p.reach[s.at] {
				continue
			}
			p.ends[hermesPartKey{s.at, take}] = p.pos
			next, matched = min(next, take+1), true
		}
	}
	return next, matched
}

// end ends the words: they are a steered note where the framing's opening
// begins them and its close ends them, the inner text's last part ending
// with it.
func (p *hermesSteerParse) end() {
	if !p.opened || p.refused || string(p.tail) != hermesNoteClose {
		p.refused = true
		return
	}
	p.length, p.ended = p.pos, true
	p.partsEnd()
}

// notes is the takes the row's parse matched, in order: none where its words
// are no steered note, or an empty one.
func (p *hermesSteerParse) notes() []int {
	if !p.ended || p.none || p.unsure || p.length == 0 {
		return nil
	}
	type state struct {
		at   int64
		from int
	}
	type parse struct {
		end   int64
		takes []int
	}
	memo := map[state]parse{}
	var best func(at int64, from int) parse
	best = func(at int64, from int) parse {
		if got, ok := memo[state{at, from}]; ok {
			return got
		}
		out := parse{end: at}
		for take := from; take < p.takes && out.end < p.length; take++ {
			e, ok := p.ends[hermesPartKey{at, take}]
			if !ok {
				continue
			}
			rest := parse{end: e}
			if e < p.length {
				rest = best(e+1, take+1)
			}
			if rest.end > out.end {
				out = parse{end: rest.end, takes: append([]int{take}, rest.takes...)}
			}
		}
		memo[state{at, from}] = out
		return out
	}
	return best(0, p.base).takes
}
