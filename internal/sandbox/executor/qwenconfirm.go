package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A Qwen Code note is CONFIRMED BY ITS RECORD AND THE REPLY AFTER IT. Qwen Code
// records a drained message (`type: user`, subtype `mid_turn_user_message`)
// BEFORE the request carrying it goes out (0.28.4: ahead of its
// request_lifecycle start), so only the next assistant record shows the model
// read it. Not a request's successful end: the first after a note can be a
// compaction's own summary call (chat-compact.jsonl). THE RECORD REFRAMES the
// message behind Qwen Code's framing ("\n" + [qwenNoteFraming]), cut exactly
// before the text is held to a serve's digest ([ItemDigest]). The helper
// confirms on the notes route as the run goes (qwen_helper.mjs), a read of the
// job's session here ([recordConfirmer]). The model writes no user record, and a
// tool's output sits in a tool_result record, never read.

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
	from, err := qwenRunFrom(ctx, box, session.ID)
	if err != nil {
		return nil, err
	}
	r, err := box.OpenFile(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", file, err)
	}
	defer r.Close()
	return qwenNotesHeld(r, takes, from, maxLineBytes)
}

// qwenRunFrom is the first line of session's chat file the box's last run
// wrote, off the box's record of where it began ([Paths.QwenRunStart]). A
// record that cannot be read, does not parse or names another session is an
// error, never 0: from 0, a continued session's earlier records would
// confirm a note this run lost, so the take stays undecided instead.
func qwenRunFrom(ctx context.Context, box sandbox.Sandbox, session string) (int, error) {
	path := PathsFor(box).QwenRunStart()
	raw, err := box.ReadFile(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("reading where the run began in its session (%s): %w", path, err)
	}
	id, count, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	n, atoiErr := strconv.Atoi(count)
	switch {
	case !ok || atoiErr != nil || n < 0:
		return 0, fmt.Errorf("%s says %q, not a session and a count of its lines", path, raw)
	case id != session:
		return 0, fmt.Errorf("%s names the session %q, and the run's is %s: the box ran another "+
			"session since", path, id, session)
	}
	return n, nil
}

// qwenChatFile is the chat file Qwen Code records session in, "" for none:
// under its home's projects directory, in the one keyed by the run's working
// directory, which the runner does not know, so it is found by the session's
// id, which names one file.
func qwenChatFile(ctx context.Context, box sandbox.Sandbox, session string) (string, error) {
	glob := shellword.Quote(PathsFor(box).QwenDir()+"/projects") + "/*/chats/" + shellword.Quote(session+".jsonl")
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
// session read from r holds as a current instruction: each record, at or past
// the line from where the takes' run began, whose handed text's digest is the
// next take's matches it, so two takes of identical text need two records. A
// match is settled only by the next assistant record, the reply to a request
// that carried the note plain; a compaction before it keeps the match only
// where the note survives plain in the history Qwen Code goes on with, past
// the summary (ADR-0177). A match no reply follows is not held, so the note is
// served again. A line past bound or one that does not decode drops every
// pending match: it may have been a compaction that summarized them.
func qwenNotesHeld(r io.Reader, takes []ServedTake, from, bound int) ([]string, error) {
	type match struct{ id, digest string }
	var settled []string
	var pending []match
	next, at := 0, -1
	framing := "\n" + qwenNoteFraming
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), bound, func(line []byte) error {
		if at++; at < from {
			return nil
		}
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
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		if !decodes(json.Unmarshal, line, &record) {
			pending = pending[:0]
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
	}, func(int64) error {
		at++
		pending = pending[:0]
		return nil
	})
	return settled, err
}

// qwenPreserved counts, by digest, the handed texts a chat_compression record
// keeps plain: each text part of a user entry of the history Qwen Code goes on
// with, past its prefix, its framing cut (a model entry's words are never a
// note). The prefix is the larger of summaryPrefixLength (the summary and the
// model's acknowledgement, measured on 0.28.4) and contextPrefixLength, which
// no capture tells apart from it: the larger keeps less, so a note the
// compaction summarized is never held. A record whose summary prefix does not
// read keeps nothing.
func qwenPreserved(payload json.RawMessage) map[string]int {
	var p struct {
		SummaryPrefixLength *int `json:"summaryPrefixLength"`
		ContextPrefixLength *int `json:"contextPrefixLength"`
		CompressedHistory   []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"compressedHistory"`
	}
	kept := map[string]int{}
	if json.Unmarshal(payload, &p) != nil || p.SummaryPrefixLength == nil || *p.SummaryPrefixLength < 0 {
		return kept
	}
	prefix := *p.SummaryPrefixLength
	if p.ContextPrefixLength != nil {
		prefix = max(prefix, *p.ContextPrefixLength)
	}
	for i := prefix; i < len(p.CompressedHistory); i++ {
		if p.CompressedHistory[i].Role != "user" {
			continue
		}
		for _, part := range p.CompressedHistory[i].Parts {
			if part.Text != nil {
				text, _ := strings.CutPrefix(*part.Text, "\n"+qwenNoteFraming)
				kept[ItemDigest(text)]++
			}
		}
	}
	return kept
}
