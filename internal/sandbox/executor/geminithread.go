package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A Gemini CLI take's receipt: the take script records each take it handed
// Gemini CLI, its token and its rendered text's digest ([ItemDigest]), in
// [Paths.GeminiTakes], since the token rides the hook's answer in a field
// Gemini CLI drops ([geminiTakeField]); the run's main session then says which
// it delivered ([geminiConfirmed]).

var _ mainThreadHarness = GeminiCLI{}

// threadTraits is what a Gemini CLI run's main-thread read settles: its hooks'
// receipts, read off the session's own words. It counts nothing: this build
// reads no count of a Gemini CLI session's next request.
func (GeminiCLI) threadTraits() sandbox.ThreadTraits {
	return sandbox.ThreadTraits{Receipts: true}
}

// geminiRecordedTake is one line of [Paths.GeminiTakes]: a take the script
// handed Gemini CLI, or the record's start line, From, the count of the
// session's lines as a run started ([geminiScript]), which every take after
// it is confirmed past.
type geminiRecordedTake struct {
	Take   string `json:"take"`
	Digest string `json:"digest"`
	From   *int   `json:"from"`
}

// readMainThread answers the receipts of the takes ask asks: held where the
// main session shows the take's text delivered in order; absent where the job
// is over and it does not, its whole thread read; undecided otherwise. A take
// the script never recorded was never handed over. No count is read.
func (GeminiCLI) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if len(ask.Takes) == 0 {
		return sandbox.MainThreadReading{}
	}
	undecided := func(why string) sandbox.MainThreadReading {
		return sandbox.MainThreadReading{Receipts: sandbox.TakeReceipts{Why: why}}
	}
	if !sandbox.ValidSessionID(ask.Session) {
		return undecided(fmt.Sprintf("the run's Gemini CLI session %q is no session id its record is named by", ask.Session))
	}
	raw, err := box.ReadFile(ctx, paths.GeminiTakes(ask.Session))
	if err != nil {
		return undecided("the run's record of its takes could not be read: " + err.Error())
	}
	var recorded []geminiRecordedTake
	var asked []geminiAskedTake
	from := 0
	started := false
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		var t geminiRecordedTake
		switch {
		case json.Unmarshal(line, &t) != nil:
		case t.From != nil && *t.From >= 0:
			// A LATER RUN'S START ENDS THE EARLIER RUNS' LINES: their takes
			// are confirmed only before it ([geminiAskedTake]).
			for i := range asked {
				if asked[i].until < 0 {
					asked[i].until = *t.From
				}
			}
			from = *t.From
			started = true
		case t.Take != "":
			recorded = append(recorded, t)
			asked = append(asked, geminiAskedTake{digest: t.Digest, from: from, until: -1})
		}
	}
	// A RECORD WITHOUT ITS START LINE was lost with its box (a box reads a
	// missing file as empty), so a take it may have held stays undecided.
	if !started {
		return undecided("the run's record of its takes holds no start line, so the box lost it")
	}
	held := map[string]bool{}
	if len(recorded) > 0 {
		session, err := geminiSessionFile(ctx, box, paths, ask.Session)
		if err != nil {
			return undecided(err.Error())
		}
		confirmed, err := geminiConfirmed(session, asked, maxLineBytes)
		_ = session.Close()
		if err != nil {
			return undecided("the run's session could not be read whole: " + err.Error())
		}
		for i, ok := range confirmed {
			if ok {
				held[recorded[i].Take] = true
			}
		}
	}
	var receipts sandbox.TakeReceipts
	for _, asked := range ask.Takes {
		switch {
		case held[asked.Take]:
			receipts.Held = append(receipts.Held, asked.Take)
		case ask.Ended:
			receipts.Absent = append(receipts.Absent, asked.Take)
		}
	}
	if len(receipts.Held) == 0 && len(receipts.Absent) == 0 {
		receipts.Why = "the run's session does not show the takes yet, and its job is not over"
	}
	return sandbox.MainThreadReading{Receipts: receipts}
}

// geminiSessionFile opens the main session of session in box: the chat file
// under [Paths.GeminiChats] named after its first eight characters whose first
// line names it whole, as two sessions may share the eight.
func geminiSessionFile(ctx context.Context, box sandbox.Sandbox, paths Paths, session string) (io.ReadCloser, error) {
	if len(session) < 8 || !sandbox.ValidSessionID(session) {
		return nil, fmt.Errorf("the run's Gemini CLI session %q is no session id Gemini CLI names a file by", session)
	}
	res, err := box.Exec(ctx, "ls -1 "+shellword.Quote(paths.GeminiChats()), sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("the run's Gemini CLI session %s has no chat directory to read", session)
	}
	tail := "-" + session[:8] + ".jsonl"
	for name := range strings.Lines(res.Stdout) {
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, tail) || strings.Contains(name, "/") {
			continue
		}
		file, err := box.OpenFile(ctx, paths.GeminiChats()+"/"+name)
		if err != nil {
			continue
		}
		first, _ := bufio.NewReader(io.LimitReader(file, int64(maxLineBytes))).ReadBytes('\n')
		_ = file.Close()
		var head struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(first, &head) != nil || head.SessionID != session {
			continue
		}
		return box.OpenFile(ctx, paths.GeminiChats()+"/"+name)
	}
	return nil, fmt.Errorf("the run's Gemini CLI session %s has no chat file in the box", session)
}
