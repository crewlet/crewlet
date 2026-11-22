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

// museUnreadLine is a line of a session's record museHeld could not read: no
// JSON, or past the reader's bound. One a later line follows makes the whole
// reading undecided, since it may be the compaction or the restart that
// leaves a take unconfirmed; the last line alone is one Muse is still writing,
// and nothing after it can confirm a take.
type museUnreadLine struct {
	at  int
	why string
}

func (e *museUnreadLine) Error() string {
	return fmt.Sprintf("its line %d %s, and a later line follows it", e.at, e.why)
}

// museRuntime is a `runtime.session` record's event as museHeld reads it:
// its kind, and a hook run's end where it is one.
type museRuntime struct {
	Kind    string   `json:"kind"`
	HookKey string   `json:"hook_key"`
	Status  string   `json:"status"`
	Effects []string `json:"effects"`
	Stderr  string   `json:"stderr"`
}

// decodeMuseLine is line as museHeld reads it: its payload type and, for a
// `runtime.session` record, its event; ok false where line is no JSON, or a
// runtime record's event is there and no object (a record of another kind,
// `agent_tree_initialized`, carries none, measured on 1.4.25).
func decodeMuseLine(line []byte) (payloadType string, ev museRuntime, ok bool) {
	var env struct {
		PayloadType string `json:"payload_type"`
		Payload     struct {
			Event json.RawMessage `json:"event"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &env) != nil {
		return "", museRuntime{}, false
	}
	if env.PayloadType != "runtime.session" || len(env.Payload.Event) == 0 {
		return env.PayloadType, museRuntime{}, true
	}
	if json.Unmarshal(env.Payload.Event, &ev) != nil {
		return env.PayloadType, museRuntime{}, false
	}
	return env.PayloadType, ev, true
}

// museHeld is the take tokens r, a session's record, shows held: each on the
// stderr of one of the plugin's note hooks whose answer Muse took, followed by
// a main-session model request before any compaction is installed, and, for
// the finish's, before the session restarts. A line it cannot read with a
// later line after it is a [museUnreadLine], and nothing is held.
func museHeld(r io.Reader) (map[string]bool, error) {
	held := map[string]bool{}
	pending := map[string]bool{} // token → taken at the finish
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
		payloadType, ev, ok := decodeMuseLine(line)
		switch {
		case !ok:
			unread = &museUnreadLine{at: n, why: "is not a record Muse Code writes"}
		case payloadType == "session.started":
			for token, finish := range pending {
				if finish {
					delete(pending, token)
				}
			}
		case payloadType != "runtime.session":
		case ev.Kind == "hook_run_terminal":
			finish, ours := museNoteHookOf(ev.HookKey)
			taken := (!finish && ev.Status == "completed" && slices.Contains(ev.Effects, "context")) ||
				(finish && ev.Status == "blocked" && slices.Contains(ev.Effects, "blocked"))
			if ours && taken {
				for _, token := range strings.Split(ev.Stderr, "\n") {
					if museIsToken(token) {
						pending[token] = finish
					}
				}
			}
		case ev.Kind == "context_compaction_installed":
			clear(pending)
		case ev.Kind == "model_completed":
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
