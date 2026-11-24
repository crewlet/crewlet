package executor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// CodexSteerInstruction is the standing instruction about notes a Codex run is
// given. The note helper steers each note into the running turn
// (`turn/steer`), and Codex 0.171.0 adds it to the next request as a user
// message after the tool output of the step in flight (measured).
const CodexSteerInstruction = steerInstructionHead +
	"Codex adds it to the conversation between your steps, as a message from the " +
	"user after the tool results of a step. " + steerInstructionTail

// NoteInstruction is the standing instruction about notes a Codex run is given:
// it takes a person's notes between its steps through the note helper its
// script starts beside it ([Codex.Command]).
func (Codex) NoteInstruction() string { return CodexSteerInstruction }

// inboxID is the id the note helper admits a note under for a job, so a
// repeat serve is steered once: `note_` and 32 hex digits of a hash of the job
// and the note. Codex records no id of the engine's, so the confirm matches by
// the item's digest instead ([codexNotesHeld]).
func (Codex) inboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "note_" + hex.EncodeToString(sum[:])[:32]
}

// pollCeiling is the most UTF-16 units one message the note helper steers can
// carry: [codexSteerMaxCodePoints], since a text never holds more code points
// than UTF-16 units, so a message held to as many units is within turn/steer's
// bound. The model's window, not the call, is the real bound: a steer past it
// is compacted (ADR-0177's "Confirmed").
func (Codex) pollCeiling() int { return codexSteerMaxCodePoints }

// A CODEX NOTE IS CONFIRMED BY ITS RECORD IN THE RUN AND THE REPLY AFTER IT,
// as Qwen Code's is ([qwenNotesHeld]). The rollout of a continued session
// holds every earlier launch's messages, so only lines past where this run
// began count ([Paths.CodexRunStart]). Two takes of one text need two
// records, matched in serve order. The record alone is no proof: Codex writes
// a steered message before the request that carries it, so a run killed, or a
// request refused, after the write leaves a record no request carried. A
// resume handing such a trailing message back was not measured on 0.171.0, so
// the model's next output, its words, reasoning or a call, settles a match
// (ADR-0177 §3). A compaction before that reply drops the match: the next
// request may carry the note only inside the summary.

// notesHeld is the inbox ids of takes, in serve order, that Codex's rollout of
// session shows reached the model ([codexNotesHeld]): the helper never
// confirms, so the record is the one confirm path ([recordConfirmer]).
func (Codex) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {

	find := codexRolloutPath(PathsFor(box), session.ID)
	if find == "" {
		return nil, fmt.Errorf("executor: %q is no Codex session the run reported", session.ID)
	}
	from, err := codexRunFrom(ctx, box, session.ID)
	if err != nil {
		return nil, err
	}
	found, err := box.Exec(ctx, find, sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: finding Codex's rollout of session %s: %w", session.ID, err)
	}
	path := strings.TrimSpace(found.Stdout)
	if path == "" {
		return nil, fmt.Errorf("executor: the box holds no Codex rollout of session %s", session.ID)
	}
	body, err := box.OpenFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("executor: opening Codex's rollout of session %s: %w", session.ID, err)
	}
	defer func() { _ = body.Close() }()
	held, err := codexNotesHeld(ctx, body, takes, from)
	if err != nil {
		return nil, fmt.Errorf("executor: reading Codex's rollout of session %s: %w", session.ID, err)
	}
	return held, nil
}

// codexRunFrom is how many lines of session's rollout came before the box's
// last run, off the box's record of where it began ([Paths.CodexRunStart]). A
// record that cannot be read, does not parse or names another session is an
// error, never 0: from 0, an earlier launch's message of the same text would
// confirm a note this run never delivered, so the take stays undecided.
func codexRunFrom(ctx context.Context, box sandbox.Sandbox, session string) (int, error) {
	path := PathsFor(box).CodexRunStart()
	raw, err := box.ReadFile(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("executor: reading where the run began in its session (%s): %w", path, err)
	}
	id, count, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	n, atoiErr := strconv.Atoi(count)
	switch {
	case !ok || atoiErr != nil || n < 0:
		return 0, fmt.Errorf("executor: %s says %q, not a session and a count of its lines", path, raw)
	case id != session:
		return 0, fmt.Errorf("executor: %s names the session %q, and the run's is %s: the box ran another "+
			"session since", path, id, session)
	}
	return n, nil
}

// codexNotesHeld is the inbox ids of takes, in serve order, whose items the
// rollout r holds past its first from lines as a person's message that a reply
// of the model followed with no compaction between ([codexConversation]): each
// such message matches the first unmatched take after the last one matched
// with its digest, as Gemini CLI's do ([geminiConfirmed]), so a take Codex
// never recorded leaves the later ones to be matched, unheld itself. Whether
// 0.171.0 can accept a turn/steer and not record it was not measured; the
// helper takes an accepted steer as delivered, so the match never assumes.
func codexNotesHeld(ctx context.Context, r io.Reader, takes []ServedTake, from int) ([]string, error) {
	var settled, pending []string
	next := 0
	_, err := codexConversation(ctx, r, from, func(kind codexTurnKind, digest string) {
		switch kind {
		case codexPersonsWords:
			for i := next; i < len(takes); i++ {
				if takes[i].Digest == digest {
					pending, next = append(pending, takes[i].InboxID), i+1
					break
				}
			}
		case codexModelReply:
			settled = append(settled, pending...)
			pending = pending[:0]
		case codexCompacted:
			pending = pending[:0]
		}
	})
	if err != nil {
		return nil, err
	}
	return settled, nil
}

// codexTurnKind is what a rollout line is to the conversation the model reads.
type codexTurnKind int

const (
	// codexPersonsWords is a person's message ([codexPersonsMessage]): a
	// brief or a steered note, its text the input_text parts joined.
	codexPersonsWords codexTurnKind = iota + 1
	// codexModelReply is the model's output answering a request
	// ([codexReply]), never its answer to a compaction.
	codexModelReply
	// codexCompacted is a `compacted` line: what came before may reach the
	// model only inside its summary, never read as a person's words.
	codexCompacted
)

// codexConversation hands each the lines of the rollout r past its first from
// that make the conversation the model reads, in order, a person's words by
// their digest ([ItemDigest]), on the notes walk ([lineWalk.setupNotes]): no
// line is held. It reports whether the last line was torn, which is never
// read; a fragment is passed over as Codex passes it, and a line past its
// bound fails the read ([errCodexRolloutLine]).
func codexConversation(ctx context.Context, r io.Reader, from int,
	each func(kind codexTurnKind, digest string)) (bool, error) {
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes), codexWalkBound)
	var l codexNoteLine
	for n := 1; ; n++ {
		l.start()
		line, err := w.noteLine(ctx, func() error { return l.read(&w) })
		switch {
		case err != nil:
			return false, fmt.Errorf("executor: reading codex rollout line %d: %w", n, err)
		case line.none:
			return false, nil
		case line.torn:
			return true, nil
		case line.past:
			return false, fmt.Errorf("%w: line %d is over %d bytes", errCodexRolloutLine, n, codexRolloutLineMaxBytes)
		}
		if n > from && line.object && !l.fragment {
			l.tell(each)
		}
		if line.ended {
			return false, nil
		}
	}
}

// codexNoteLine is what a collection's read keeps of one rollout line: the
// envelope a note's confirm reads ([codexRecordOf]'s fields), and the item its
// payload decodes as ([codexItemWire]'s), its person's words digested as read.
type codexNoteLine struct {
	typ                        string
	compactionOutput, fragment bool
	itemTook                   bool
	itemType, role             string
	meta                       codexMeta
	// words digests the input_text parts read; part a part's text, from
	// words as it stood before it, kept where the part's type picks it.
	words, part hash.Hash
	state       []byte
}

// start starts the line.
func (l *codexNoteLine) start() {
	l.typ, l.compactionOutput, l.fragment = "", false, false
	l.payloadStart()
	if l.words == nil {
		l.words, l.part = sha256.New(), sha256.New()
	}
	l.words.Reset()
}

// payloadStart starts the line's payload: one written again replaces it.
func (l *codexNoteLine) payloadStart() {
	l.itemTook, l.itemType, l.role, l.meta = true, "", "", codexMeta{userKinds: true}
	if l.words != nil {
		l.words.Reset()
	}
}

// read reads the line's envelope, its opening brace current.
func (l *codexNoteLine) read(w *lineWalk) error {
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "timestamp":
			ok, err = w.typedField(k, fieldString)
		case "type":
			l.typ, ok, err = w.wordField(k)
		case "payload":
			ok, err = true, l.payload(w, k)
		case "metadata":
			l.compactionOutput = false
			ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "compaction_output" {
					return w.scan.Skip()
				}
				flag, took, err := w.flagField(k)
				l.compactionOutput, l.fragment = flag, l.fragment || !took
				return err
			})
		default:
			return w.scan.Skip()
		}
		l.fragment = l.fragment || !ok
		return err
	})
}

// payload reads the payload as a response item: its type, role, input_text
// parts and metadata.
func (l *codexNoteLine) payload(w *lineWalk, k jsonscan.Kind) error {
	l.payloadStart()
	took, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.itemType, ok, err = w.wordField(k)
		case "role":
			l.role, ok, err = w.wordField(k)
		case "content":
			l.words.Reset()
			ok, err = w.listField(k, func(k jsonscan.Kind) error {
				part, err := l.contentPart(w, k)
				l.itemTook = l.itemTook && part
				return err
			})
		case "internal_chat_message_metadata_passthrough":
			ok, err = l.meta.read(w, k)
		default:
			return w.scan.Skip()
		}
		l.itemTook = l.itemTook && ok
		return err
	})
	l.itemTook = l.itemTook && took
	return err
}

// contentPart reads one part of a message's content, its text digested into
// words where its type, read before or after it, is input_text; it reports
// whether the part's struct takes it.
func (l *codexNoteLine) contentPart(w *lineWalk, k jsonscan.Kind) (bool, error) {
	var kind string
	text := false
	took := true
	ok, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
		switch name {
		case "type":
			var ok bool
			var err error
			kind, ok, err = w.wordField(k)
			took = took && ok
			return err
		case "text":
			switch k {
			case jsonscan.String:
			case jsonscan.Null:
				text = false
				return nil
			default:
				took = false
				return w.scan.Skip()
			}
			state, err := l.words.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				return err
			}
			if err := l.part.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
				return err
			}
			text = true
			return w.read(l.part)
		}
		return w.scan.Skip()
	})
	if err == nil && ok && took && text && kind == "input_text" {
		state, mErr := l.part.(encoding.BinaryMarshaler).MarshalBinary()
		if mErr != nil {
			return false, mErr
		}
		err = l.words.(encoding.BinaryUnmarshaler).UnmarshalBinary(state)
	}
	return ok && took, err
}

// tell hands each what the line is to the conversation the model reads.
func (l *codexNoteLine) tell(each func(kind codexTurnKind, digest string)) {
	if l.typ == "compacted" {
		each(codexCompacted, "")
		return
	}
	if l.typ != "response_item" || !l.itemTook {
		return
	}
	switch {
	case l.itemType == "message" && l.meta.persons(l.role):
		each(codexPersonsWords, hex.EncodeToString(l.words.Sum(nil)))
	case codexReply(l.itemType, l.role) && !l.compactionOutput:
		each(codexModelReply, "")
	}
}

// codexReply reports whether an item of kind, by role, is the model's output
// answering a request: its words, its reasoning or a call it made (as the
// transcript reads them).
func codexReply(kind, role string) bool {
	switch kind {
	case "message":
		return role == "assistant"
	case "reasoning", "function_call", "custom_tool_call", "web_search_call", "local_shell_call":
		return true
	}
	return false
}

// codexMeta is what Codex records beside a message of who wrote it
// ([codexItemMeta]) as a walk reads it: how many part kinds it records,
// whether each is a user's text, and whether a part's provenance is the
// harness's.
type codexMeta struct {
	kinds              int
	userKinds, harness bool
}

// persons is [codexPersonsMessage] of a message by role with this metadata.
func (m codexMeta) persons(role string) bool {
	return role == "user" && (m.kinds > 0 && m.userKinds || m.kinds == 0 && !m.harness)
}

// read reads a message's metadata passthrough, the value k begins, and
// reports whether its decode takes it.
func (m *codexMeta) read(w *lineWalk, k jsonscan.Kind) (bool, error) {
	*m = codexMeta{userKinds: true}
	took := true
	ok, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "content_item_kinds":
			m.kinds, m.userKinds = 0, true
			ok, err = w.listField(k, func(k jsonscan.Kind) error {
				kind, word, err := w.wordField(k)
				m.kinds++
				m.userKinds = m.userKinds && kind == "user.text"
				took = took && word
				return err
			})
		case "content_item_metadata":
			m.harness = false
			ok, err = w.listField(k, func(k jsonscan.Kind) error {
				part, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
					if name != "provenance" {
						return w.scan.Skip()
					}
					prov, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
						if name != "type" {
							return w.scan.Skip()
						}
						typ, word, err := w.wordField(k)
						m.harness = m.harness || typ == "harness"
						took = took && word
						return err
					})
					took = took && prov
					return err
				})
				took = took && part
				return err
			})
		default:
			return w.scan.Skip()
		}
		took = took && ok
		return err
	})
	return ok && took, err
}
