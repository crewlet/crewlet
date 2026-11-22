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

// A Muse Code take's RECEIPT (ADR-0177's hook receipts, measured on 1.4.25):
// the session's record keeps each hook run whole, its stderr among it, which
// the model never sees, so the take's token there, on a run of one of the
// plugin's note hooks that Muse took the answer of (a PostToolUse `completed`
// with the effect `context`, a Stop `blocked`), says the answer reached the
// conversation; the next `model_completed` of the main session says the model
// was sent it. A hook killed or timed out records no effect, so its take is
// never held. A compaction installed in between leaves the take unconfirmed,
// told again: the record names what a compaction kept only by its own
// sequence. A finish's answer sits in the leading developer block, which a
// resumed session does not restore, so a restart in between leaves it
// unconfirmed too.

var _ mainThreadHarness = Muse{}

// threadTraits is what a Muse Code run's main-thread read settles: its hooks'
// receipts, read off the session's record. It counts nothing.
func (Muse) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{Receipts: true} }

// readMainThread answers the receipts of the takes ask asks: held where the
// record shows each delivered and sent; absent where the job is over and it
// does not; undecided otherwise.
func (Muse) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if len(ask.Takes) == 0 {
		return sandbox.MainThreadReading{}
	}
	undecided := func(why string) sandbox.MainThreadReading {
		return sandbox.MainThreadReading{Receipts: sandbox.TakeReceipts{Why: why}}
	}
	record, err := museRecord(ctx, box, paths, ask.Session)
	if err != nil {
		return undecided(err.Error())
	}
	held, err := museHeld(record)
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

// museRecord opens session's record in box, under whatever day it is filed.
func museRecord(ctx context.Context, box sandbox.Sandbox, paths Paths, session string) (io.ReadCloser, error) {
	if !sandbox.ValidSessionID(session) {
		return nil, fmt.Errorf("the run's Muse Code session %q is no session id its record is named by", session)
	}
	res, err := box.Exec(ctx, `for y in `+sandbox.ShellQuote(paths.MuseSessions())+`/*/*/*/`+
		sandbox.ShellQuote(session)+`; do [ -f "$y/session.jsonl" ] && printf '%s\n' "$y/session.jsonl"; done`,
		sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("the run's Muse Code session %s could not be looked for: %w", session, err)
	}
	found := strings.Fields(res.Stdout)
	if len(found) != 1 {
		return nil, fmt.Errorf("the box holds %d records of the run's Muse Code session %s, not one", len(found), session)
	}
	return box.OpenFile(ctx, found[0])
}

// museTerminal is a hook run's end as the record keeps it.
type museTerminal struct {
	HookKey string   `json:"hook_key"`
	Status  string   `json:"status"`
	Effects []string `json:"effects"`
	Stderr  string   `json:"stderr"`
}

// museHeld is the take tokens r, a session's record, shows held: each on the
// stderr of one of the plugin's note hooks whose answer Muse took, followed by
// a main-session model request before any compaction is installed, and, for
// the finish's, before the session restarts.
func museHeld(r io.Reader) (map[string]bool, error) {
	held := map[string]bool{}
	pending := map[string]bool{} // token → taken at the finish
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		env, ok := decodeMuseEnvelope(line)
		if !ok {
			return nil
		}
		if env.PayloadType == "session.started" {
			for token, finish := range pending {
				if finish {
					delete(pending, token)
				}
			}
			return nil
		}
		if env.PayloadType != "runtime.session" {
			return nil
		}
		var p struct {
			Event json.RawMessage `json:"event"`
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(env.Payload, &p) != nil || json.Unmarshal(p.Event, &kind) != nil {
			return nil
		}
		switch kind.Kind {
		case "hook_run_terminal":
			var t museTerminal
			if json.Unmarshal(p.Event, &t) != nil {
				return nil
			}
			finish, ours := museNoteHookOf(t.HookKey)
			taken := (!finish && t.Status == "completed" && slices.Contains(t.Effects, "context")) ||
				(finish && t.Status == "blocked" && slices.Contains(t.Effects, "blocked"))
			if !ours || !taken {
				return nil
			}
			for _, token := range strings.Split(t.Stderr, "\n") {
				if museIsToken(token) {
					pending[token] = finish
				}
			}
		case "context_compaction_installed":
			clear(pending)
		case "model_completed":
			for token := range pending {
				held[token] = true
			}
			clear(pending)
		}
		return nil
	}, func(int64) error { return nil })
	return held, err
}

// museNoteHookOf reports whether key names one of the plugin's note hooks, and
// whether it is the finish's.
func museNoteHookOf(key string) (finish, ours bool) {
	for _, h := range museNoteHooks {
		if key == museHookKey(h) {
			return h.event == "Stop", true
		}
	}
	return false, false
}

// museIsToken reports whether line is a take token whole, as
// [claudeTakeToken] writes one.
func museIsToken(line string) bool {
	return len(line) == claudeTakeTokenLen && strings.HasPrefix(line, claudeTakePrefix) && strings.HasSuffix(line, "]")
}
