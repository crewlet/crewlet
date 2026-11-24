package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// HermesSteerInstruction is the standing instruction about notes a Hermes run
// is given. The runner's plugin steers each note into the running turn
// (`session.steer`), and Hermes 0.23.4 adds it after the newest tool result as
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
// Hermes 0.23.4 records it as (measured): the note's text between them, byte
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
	held, err := hermesNotesHeld(body, takes, from)
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
// r holds as notes the model was sent. Each user row of this launch (above
// from, no copy of an earlier row) in the conversation ([hermesMessage.current]),
// shown as a steer and framed, holds the notes steered before one request,
// joined by newlines (measured on 0.23.4); its parts match takes as an
// in-order subsequence, so a take the record lacks stays unheld, and two takes
// of one note's words need two parts. A match is settled only by a finished
// reply stored after it ([hermesMessage.replied]; the note rides the request
// after the tool result, testdata request-steered.json); one no reply follows
// is not held, so it is served again. A row past [hermesRowMaxBytes] drops
// every pending match.
func hermesNotesHeld(r io.Reader, takes []ServedTake, from int64) ([]string, error) {
	type match struct {
		inbox string
		row   int64
	}
	var settled []string
	var pending []match
	next := 0
	// A COMPACTION'S REWRITTEN TAIL keeps each row's message_uid under a new
	// id (measured on 0.23.4), in place or in a rotated child whose parent's
	// rows stay current, so a copy of a row read or of an earlier launch's is
	// passed over, however high its id.
	earlier := map[string]bool{}
	err := hermesRows(r, func(row hermesRow) error {
		m := row.Message
		if row.Table != "messages" {
			return nil
		}
		if m.ID <= from {
			if m.UID != "" {
				earlier[m.UID] = true
			}
			return nil
		}
		if !m.current() || earlier[m.UID] {
			return nil
		}
		if m.UID != "" {
			earlier[m.UID] = true
		}
		switch {
		case m.replied():
			kept := pending[:0]
			for _, p := range pending {
				if p.row < m.ID {
					settled = append(settled, p.inbox)
				} else {
					kept = append(kept, p)
				}
			}
			pending = kept
		case m.Role == "user" && m.DisplayKind != nil && *m.DisplayKind == "steer":
			if inner, ok := hermesSteered(m.text()); ok && inner != "" {
				base := next
				for _, at := range hermesRowNotes(inner, takes[base:]) {
					pending = append(pending, match{takes[base+at].InboxID, m.ID})
					next = base + at + 1
				}
			}
		}
		return nil
	}, func() error {
		pending = pending[:0]
		return nil
	})
	return settled, err
}

// hermesRowNotes parses joined (one row's notes, joined by newlines) into
// parts, each ending at a newline or at its end, matched to takes as an
// in-order subsequence, and returns the matched takes' indexes. A parse of the
// whole row wins over one of a prefix, and among equals the earliest takes win:
// a note's own newlines are tried past, so takes ["fix\nthen push", "fix"] read
// "fix\nthen push" as the first, never as "fix" with an unread tail. Where no
// parse takes the whole row, the longest parsed prefix holds what it matched.
func hermesRowNotes(joined string, takes []ServedTake) []int {
	type parse struct {
		end int // the bytes of joined the parse consumes
		at  []int
	}
	// ends[pos] maps the digest ([ItemDigest], hashed on as the part grows) of
	// each part beginning at pos to its end.
	ends := map[int]map[string]int{}
	partsAt := func(pos int) map[string]int {
		if got, ok := ends[pos]; ok {
			return got
		}
		got := map[string]int{}
		h := sha256.New()
		for from, e := pos, pos; e <= len(joined); e++ {
			if e < len(joined) && joined[e] != '\n' {
				continue
			}
			h.Write([]byte(joined[from:e]))
			got[hex.EncodeToString(h.Sum(nil))] = e
			from = e
		}
		ends[pos] = got
		return got
	}
	type state struct{ pos, from int }
	memo := map[state]parse{}
	var best func(pos, from int) parse
	best = func(pos, from int) parse {
		if p, ok := memo[state{pos, from}]; ok {
			return p
		}
		out := parse{end: pos}
		for at := from; at < len(takes) && out.end < len(joined); at++ {
			e, ok := partsAt(pos)[takes[at].Digest]
			if !ok {
				continue
			}
			rest := parse{end: e}
			if e < len(joined) {
				rest = best(e+1, at+1)
			}
			if rest.end > out.end {
				out = parse{end: rest.end, at: append([]int{at}, rest.at...)}
			}
		}
		memo[state{pos, from}] = out
		return out
	}
	return best(0, 0).at
}
