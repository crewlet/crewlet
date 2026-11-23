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

// A Grok Build take's RECEIPT (ADR-0177's hook receipts, measured on 1.0.92):
// the session's record (chat_history.jsonl) keeps each hook answer Grok handed
// its model whole, as a line of its own: a PostToolUse or PostToolUseFailure
// answer as a system reminder naming the run's hook, a Stop block as the stop
// hook's feedback. A take is held where its token ends such a line and a
// model's answer follows it: the request that carried it completed. A hook
// Grok killed or cut short hands nothing over and leaves no token.

var (
	_ mainThreadHarness = Grok{}
	_ threadHarness     = Grok{}
)

// threadTraits is what a Grok Build run's main-thread read settles: its hooks'
// receipts, read off the session's record. It counts nothing.
func (Grok) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{Receipts: true} }

// readMainThread answers the receipts of the takes ask asks: held where the
// record shows each delivered and sent; absent where the job is over and it
// does not; undecided otherwise.
func (Grok) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if len(ask.Takes) == 0 {
		return sandbox.MainThreadReading{}
	}
	undecided := func(why string) sandbox.MainThreadReading {
		return sandbox.MainThreadReading{Receipts: sandbox.TakeReceipts{Why: why}}
	}
	if !sandbox.ValidSessionID(ask.Session) {
		return undecided(fmt.Sprintf("the run's Grok Build session %q is no session id its record is named by",
			ask.Session))
	}
	record, err := box.OpenFile(ctx, paths.GrokSessionsKey()+"/"+ask.Session+"/chat_history.jsonl")
	if err != nil {
		return undecided(fmt.Sprintf("the run's Grok Build session %s has no record in the box: %v", ask.Session, err))
	}
	held, err := grokHeld(record)
	_ = record.Close()
	if err != nil {
		return undecided("the run's session record could not be read whole: " + err.Error())
	}
	var receipts sandbox.TakeReceipts
	for _, asked := range ask.Takes {
		switch {
		case held[claudeTakeToken(asked.Take)]:
			receipts.Held = append(receipts.Held, asked.Take)
		case ask.Ended:
			receipts.Absent = append(receipts.Absent, asked.Take)
		}
	}
	if len(receipts.Held) == 0 && len(receipts.Absent) == 0 {
		receipts.Why = "the run's session record does not show the takes yet, and its job is not over"
	}
	return sandbox.MainThreadReading{Receipts: receipts}
}

// grokHookHeads open the system reminders Grok Build hands its model a hook of
// the run's settings answered with, PostToolUse's and PostToolUseFailure's:
// the hook named by its scope, `user` for the home's settings, its event and
// its place (measured on 1.0.92). A repository's hook would be named under
// another scope.
var grokHookHeads = []string{
	"<system-reminder>\nContext from PostToolUse hook 'user:post_tool_use[0].hooks[0]':\n",
	"<system-reminder>\nContext from PostToolUseFailure hook 'user:post_tool_use_failure[0].hooks[0]':\n",
}

// grokReminderTail closes a system reminder.
const grokReminderTail = "\n</system-reminder>"

// grokStopHead opens a Stop hook's feedback as the record keeps it, `Stop hook
// feedback:\n- <reason>\n` (measured on 1.0.92).
const grokStopHead = "Stop hook feedback:\n- "

// grokLine is a line of a session's record as its readers read it.
type grokLine struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
	Reason  string          `json:"synthetic_reason"`
	Prompt  *int            `json:"prompt_index"`
	Calls   []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
	CallID string `json:"tool_call_id"`
}

// decodeGrokLine is line as the record's readers read it, false where it is no
// JSON object.
func decodeGrokLine(line []byte) (grokLine, bool) {
	var l grokLine
	ok := json.Unmarshal(line, &l) == nil
	return l, ok
}

// text is a line's words: a string content, or its parts' text joined.
func (l grokLine) text() string {
	var s string
	if json.Unmarshal(l.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(l.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// grokHookWords is what a hook of the run's settings handed the model on line,
// without its framing, and true; false where line is no such hook's.
func grokHookWords(l grokLine) (string, bool) {
	if l.Type != "user" {
		return "", false
	}
	text := l.text()
	switch l.Reason {
	case "system_reminder":
		for _, head := range grokHookHeads {
			if inner, ok := strings.CutPrefix(text, head); ok {
				return strings.TrimSuffix(inner, grokReminderTail), true
			}
		}
	case "stop_hook_feedback":
		if inner, ok := strings.CutPrefix(text, grokStopHead); ok {
			return strings.TrimSuffix(inner, "\n"), true
		}
	}
	return "", false
}

// grokTokenOf is the take token words end with, "" for none.
func grokTokenOf(words string) string {
	words = strings.TrimRight(words, " \n")
	if len(words) < claudeTakeTokenLen {
		return ""
	}
	if token := words[len(words)-claudeTakeTokenLen:]; museIsToken(token) {
		return token
	}
	return ""
}

// grokHeld is the take tokens r, a session's record, shows held, as the receipt
// above says. A line it cannot read with a later line after it makes the
// reading fail, since it may be the one that held or dropped a take.
func grokHeld(r io.Reader) (map[string]bool, error) {
	held := map[string]bool{}
	pending := map[string]bool{}
	var unread *museUnreadLine
	n := 0
	next := func() error {
		n++
		if unread != nil {
			return unread
		}
		return nil
	}
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := next(); err != nil {
			return err
		}
		l, ok := decodeGrokLine(line)
		if !ok {
			unread = &museUnreadLine{at: n, why: "is not a record Grok Build writes"}
			return nil
		}
		if words, ok := grokHookWords(l); ok {
			if token := grokTokenOf(words); token != "" {
				pending[token] = true
			}
			return nil
		}
		if l.Type == "assistant" {
			for token := range pending {
				held[token] = true
			}
			clear(pending)
		}
		return nil
	}, func(size int64) error {
		if err := next(); err != nil {
			return err
		}
		unread = &museUnreadLine{at: n, why: fmt.Sprintf("is %d bytes, past the %d read of a line", size, maxLineBytes)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return held, nil
}
