package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A Qwen Code note is CONFIRMED BY ITS RECORD: Qwen Code writes a drained
// message to the session's chat file as a `type: user`, subtype
// `mid_turn_user_message` record, after the request that carried it has gone
// out (measured on 0.28.3). THE RECORD REFRAMES the message: its first part is
// the message as the helper handed it behind Qwen Code's framing
// ("\n" + [qwenNoteFraming]), so the framing is cut, exactly, before the text
// is held to a serve's digest ([ItemDigest]). The helper confirms on the notes
// route from these records as the run goes (qwen_helper.mjs); a read of the
// job's session confirms from them here ([recordConfirmer]). The model writes
// no user record, and a tool's output sits in a tool_result record, which is
// never read.

// notesHeld is the inbox ids of takes the session's chat file holds, matched
// in serve order ([recordConfirmer]). No chat file holds none.
func (QwenCode) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {

	if !sandbox.ValidSessionID(session.ID) {
		return nil, fmt.Errorf("%q is no session id Qwen Code is created under", session.ID)
	}
	file, err := qwenChatFile(ctx, box, session.ID)
	if err != nil || file == "" {
		return nil, err
	}
	r, err := box.OpenFile(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", file, err)
	}
	defer r.Close()
	return qwenNotesHeld(r, takes, maxLineBytes)
}

// qwenChatFile is the chat file Qwen Code records session in, "" for none:
// under its home's projects directory, in the one keyed by the run's working
// directory, which the runner does not know, so it is found by the session's
// id, which names one file.
func qwenChatFile(ctx context.Context, box sandbox.Sandbox, session string) (string, error) {
	glob := sandbox.ShellQuote(PathsFor(box).QwenDir()+"/projects") + "/*/chats/" + sandbox.ShellQuote(session+".jsonl")
	res, err := box.Exec(ctx, `for f in `+glob+`; do [ -f "$f" ] && printf '%s\n' "$f"; done; true`,
		sandbox.ExecOptions{})
	if err != nil {
		return "", fmt.Errorf("finding the session's chat file: %w", err)
	}
	files := strings.Fields(res.Stdout)
	switch len(files) {
	case 0:
		return "", nil
	case 1:
		return files[0], nil
	}
	return "", fmt.Errorf("%d chat files name the session %s", len(files), session)
}

// qwenNotesHeld is the inbox ids of takes, in serve order, whose items the
// session read from r holds as a current instruction: each record whose
// handed text's digest is the next take's matches it, so two takes of
// identical text need two records. A match is settled by the next assistant
// record, a response to a request that carried the note plain; a compaction
// before it keeps the match only where the note survives plain in the history
// Qwen Code goes on with, past the summary (ADR-0177: a note read only inside
// a compaction summary is not read); one neither follows is held, the
// request that carried it already sent. A line that does not decode is passed
// over, as a torn last line is.
func qwenNotesHeld(r io.Reader, takes []ServedTake, bound int) ([]string, error) {
	type match struct{ id, digest string }
	var settled []string
	var pending []match
	next := 0
	framing := "\n" + qwenNoteFraming
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), bound, func(line []byte) error {
		var record struct {
			Type          string          `json:"type"`
			Subtype       string          `json:"subtype"`
			SystemPayload json.RawMessage `json:"systemPayload"`
			Message       struct {
				Parts []struct {
					Text *string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &record) != nil {
			return nil
		}
		switch {
		case record.Type == "assistant":
			for _, m := range pending {
				settled = append(settled, m.id)
			}
			pending = pending[:0]
		case record.Type == "system" && record.Subtype == "chat_compression":
			tail := qwenPreserved(record.SystemPayload)
			kept := pending[:0]
			for _, m := range pending {
				if tail[m.digest] > 0 {
					tail[m.digest]--
					kept = append(kept, m)
				}
			}
			pending = kept
		case record.Type == "user" && record.Subtype == "mid_turn_user_message" && next < len(takes) &&
			len(record.Message.Parts) > 0 && record.Message.Parts[0].Text != nil:
			handed, framed := strings.CutPrefix(*record.Message.Parts[0].Text, framing)
			if digest := ItemDigest(handed); framed && digest == takes[next].Digest {
				pending = append(pending, match{takes[next].InboxID, digest})
				next++
			}
		}
		return nil
	}, func(int64) error { return nil })
	for _, m := range pending {
		settled = append(settled, m.id)
	}
	return settled, err
}

// qwenPreserved counts, by digest, the handed texts a chat_compression record
// keeps plain: each text part of the history Qwen Code goes on with, past its
// summary prefix (summaryPrefixLength: the summary and the model's
// acknowledgement, measured on 0.28.3), its framing cut. A record whose prefix
// does not read keeps nothing, so no note it may have summarized is held.
func qwenPreserved(payload json.RawMessage) map[string]int {
	var p struct {
		SummaryPrefixLength *int `json:"summaryPrefixLength"`
		CompressedHistory   []struct {
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"compressedHistory"`
	}
	kept := map[string]int{}
	if json.Unmarshal(payload, &p) != nil || p.SummaryPrefixLength == nil || *p.SummaryPrefixLength < 0 {
		return kept
	}
	for i := *p.SummaryPrefixLength; i < len(p.CompressedHistory); i++ {
		for _, part := range p.CompressedHistory[i].Parts {
			if part.Text != nil {
				text, _ := strings.CutPrefix(*part.Text, "\n"+qwenNoteFraming)
				kept[ItemDigest(text)]++
			}
		}
	}
	return kept
}
