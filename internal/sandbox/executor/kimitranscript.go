package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// kimiWireLine is the part of one wire.jsonl line the confirm reader reads: its
// type, a message's id and role, and a usage record's scope.
type kimiWireLine struct {
	Type    string `json:"type"`
	Message struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"message"`
	UsageScope string `json:"usageScope"`
}

// kimiNotesHeld is the inbox ids of takes, in serve order and once each, whose
// note the main agent's wire.jsonl shows reached the model as a current
// message: its `context.append_message`, a user message under its prompt_id
// ([KimiNoteID]), then a `usage.record` of `usageScope: turn` (a request of the
// agent's loop answered) with no compaction, undo or clear between. Measured on
// 2.7.0 and 2.7.2: the line is on disk before its request; killed between the
// two, the record holds the line alone; and past a compaction, whose record
// names no message it kept, a request may carry the note only inside its
// summary (ADR-0177), so such a note is told again (code-sandbox.md, "Kimi
// Code"). A last line with no newline is a write cut off; a line past the
// bound is no line.
func kimiNotesHeld(ctx context.Context, wire io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]string, len(takes))
	for _, take := range takes {
		asked[KimiNoteID(take.InboxID)] = take.InboxID
	}
	pending := map[string]bool{}
	held := map[string]bool{}
	// moved is what the line read last moved to held, taken back where that
	// line turns out to be the file's unended last.
	var moved []string
	src := &lastByte{r: wire}
	err := readLines(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		moved = moved[:0]
		var l kimiWireLine
		if json.Unmarshal(line, &l) != nil {
			return nil
		}
		switch l.Type {
		case "context.append_message":
			if inbox, ours := asked[l.Message.ID]; ours && l.Message.Role == "user" {
				pending[inbox] = true
			}
		case "usage.record":
			if l.UsageScope != "turn" {
				return nil
			}
			for inbox := range pending {
				if !held[inbox] {
					held[inbox] = true
					moved = append(moved, inbox)
				}
			}
			clear(pending)
		case "context.apply_compaction", "context.undo", "context.clear":
			clear(pending)
		}
		return nil
	}, func(int64) error { moved = moved[:0]; return nil })
	if err != nil {
		return nil, fmt.Errorf("executor: reading which notes the Kimi session holds: %w", err)
	}
	if src.read && src.last != '\n' {
		for _, inbox := range moved {
			delete(held, inbox)
		}
	}
	var out []string
	for _, take := range takes {
		if held[take.InboxID] && !slices.Contains(out, take.InboxID) {
			out = append(out, take.InboxID)
		}
	}
	return out, nil
}

// lastByte is r, remembering the last byte it handed out.
type lastByte struct {
	r    io.Reader
	last byte
	read bool
}

func (l *lastByte) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.last, l.read = p[n-1], true
	}
	return n, err
}

// notesHeld is the inbox ids of takes ([Kimi.inboxID]) whose note the box's
// copy of session's main agent record shows reached the model
// ([kimiNotesHeld]): its [recordConfirmer], as its driver confirms nothing.
// The record is found under any workspace directory, since Kimi names that
// directory by the run's working directory: an id matching none, or more than
// one, is a record that could not be read, which is neither held nor not.
func (Kimi) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {
	if !strings.HasPrefix(session.ID, "session_") || !sandbox.ValidSessionID(session.ID) {
		return nil, fmt.Errorf("executor: %q is no Kimi session id", session.ID)
	}
	paths := PathsFor(box)
	found, err := box.Exec(ctx, `for w in `+sandbox.ShellQuote(paths.KimiSessions())+`/*/`+session.ID+
		`/agents/main/wire.jsonl; do [ -f "$w" ] && printf '%s\n' "$w"; done; :`, sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: finding the Kimi session's record: %w", err)
	}
	records := strings.Fields(strings.TrimSpace(found.Stdout))
	if len(records) != 1 {
		return nil, fmt.Errorf("executor: the box holds %d records of the Kimi session %s, not one", len(records),
			session.ID)
	}
	wire, err := box.OpenFile(ctx, records[0])
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Kimi session's record: %w", err)
	}
	defer func() { _ = wire.Close() }()
	return kimiNotesHeld(ctx, wire, takes)
}
