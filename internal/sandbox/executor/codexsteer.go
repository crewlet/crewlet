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
	takes []ServedTake, summary *summaryRead) ([]string, error) {

	find := codexRolloutPath(PathsFor(box), session.ID)
	if find == "" {
		return nil, summary.failedBy(fmt.Errorf("executor: %q is no Codex session the run reported", session.ID))
	}
	// A RUN'S START THAT DOES NOT READ decides no take, and the walk reads the
	// summary alone: it needs no start.
	from, fromErr := codexRunFrom(ctx, box, session.ID)
	if fromErr != nil && summary == nil {
		return nil, fromErr
	}
	if fromErr != nil {
		takes = nil
	}
	found, err := box.Exec(ctx, find, sandbox.ExecOptions{})
	if err != nil {
		return nil, summary.failedBy(fmt.Errorf("executor: finding Codex's rollout of session %s: %w", session.ID,
			err))
	}
	path := strings.TrimSpace(found.Stdout)
	if path == "" {
		return nil, summary.failedBy(fmt.Errorf("executor: the box holds no Codex rollout of session %s",
			session.ID))
	}
	body, err := box.OpenFile(ctx, path)
	if err != nil {
		return nil, summary.failedBy(fmt.Errorf("executor: opening Codex's rollout of session %s: %w",
			session.ID, err))
	}
	defer func() { _ = body.Close() }()
	held, err := codexNotesHeld(body, takes, from, summary)
	switch {
	case err != nil:
		return nil, summary.failedBy(fmt.Errorf("executor: reading Codex's rollout of session %s: %w",
			session.ID, err))
	case fromErr != nil:
		return nil, fromErr
	}
	return held, nil
}

// recordSummary reports whether the receipts' walk of the rollout reads ask's
// summary: on the release its summary was read on, where no count or next
// request is asked, whose walk of the session's archive reads it instead.
func (Codex) recordSummary(ask sandbox.MainThreadAsk) bool {
	return summaryReleaseWhy(ask.Release, codexCarryRelease) == "" && !ask.Count && !ask.Next
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
func codexNotesHeld(r io.Reader, takes []ServedTake, from int, summary *summaryRead) ([]string, error) {
	var settled, pending []string
	next := 0
	_, err := codexConversation(r, from, summary, func(kind codexTurnKind, text string) {
		switch kind {
		case codexPersonsWords:
			digest := ItemDigest(text)
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
// that make the conversation the model reads, in order. It reports whether
// the last line was torn, which is never read; a fragment is passed over as
// Codex passes it, and a line past its bound fails the read
// ([errCodexRolloutLine]). Every compaction, from the first line on, is read
// into summary where it is not nil.
func codexConversation(r io.Reader, from int, summary *summaryRead,
	each func(kind codexTurnKind, text string)) (bool, error) {
	return codexLines(r, func(n int, body []byte) error {
		line, record := codexRecordOf(body)
		if summary != nil && record && line.Type == "compacted" {
			summary.codexCompacted(line.Payload)
		}
		if n <= from || !record {
			return nil
		}
		if line.Type == "compacted" {
			each(codexCompacted, "")
			return nil
		}
		var item codexItemWire
		if line.Type == "response_item" && wireDecode(line.Payload, &item) == nil {
			switch {
			case item.Type == "message" && codexPersonsMessage(item.Role, item.Meta):
				each(codexPersonsWords, item.text())
			case codexReply(item) && !line.Metadata.CompactionOutput:
				each(codexModelReply, "")
			}
		}
		return nil
	}, func(n int) error {
		return fmt.Errorf("%w: line %d is over %d bytes", errCodexRolloutLine, n, codexRolloutLineMaxBytes)
	})
}

// codexReply reports whether item is the model's output answering a request:
// its words, its reasoning or a call it made (as the transcript reads them).
func codexReply(item codexItemWire) bool {
	switch item.Type {
	case "message":
		return item.Role == "assistant"
	case "reasoning", "function_call", "custom_tool_call", "web_search_call", "local_shell_call":
		return true
	}
	return false
}
