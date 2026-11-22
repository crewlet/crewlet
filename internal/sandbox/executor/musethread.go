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
// the record keeps each hook run whole, its stderr (which the model never
// sees) and the plugin package's digest among it. A take is held where its
// token is on a run of the plugin's note hooks, of the package the script
// installed, that Muse took the answer of; then that run's own context block
// (`hook:post_tool_use:<call>:<n>`, the n-th answer to the call's hooks, or
// `hook:stop:stop:<n>`) is committed; then a main-session request completes.
// A killed or timed-out hook records no effect and no block; a failed request
// confirms nothing. A compaction in between leaves the take unconfirmed, as a
// restart does a finish's, whose block a resumed session does not restore.

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
	raw, err := box.ReadFile(ctx, paths.MuseNotesInstall())
	digest := museInstalledDigest(raw)
	if err != nil || digest == "" {
		return undecided("the run's notes plugin left no digest of its installed package, so no hook's " +
			"run can be told for the plugin's")
	}
	record, err := museRecord(ctx, box, paths, ask.Session)
	if err != nil {
		return undecided(err.Error())
	}
	held, err := museHeld(record, digest)
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

// museLine is a line of a session's record as museHeld reads it: its payload
// type, its runtime event's kind and the event itself, and the call a tool
// batch's effect names.
type museLine struct {
	payloadType string
	kind        string
	event       json.RawMessage
	call        string
}

// decodeMuseLine is line as museHeld reads it; ok false where line is no
// JSON, or a runtime record's event is there and no object (a record of
// another kind, `agent_tree_initialized`, carries none, measured on 1.4.25).
func decodeMuseLine(line []byte) (museLine, bool) {
	var env struct {
		PayloadType string `json:"payload_type"`
		Payload     struct {
			Event  json.RawMessage `json:"event"`
			Record struct {
				Kind   string `json:"kind"`
				CallID string `json:"call_id"`
			} `json:"record"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &env) != nil {
		return museLine{}, false
	}
	l := museLine{payloadType: env.PayloadType, event: env.Payload.Event}
	switch {
	case env.PayloadType == "tool_batch.effect.terminal" && env.Payload.Record.Kind == "terminal":
		l.call = env.Payload.Record.CallID
	case env.PayloadType == "runtime.session" && len(env.Payload.Event) > 0:
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(env.Payload.Event, &kind) != nil {
			return museLine{}, false
		}
		l.kind = kind.Kind
	}
	return l, true
}

// museHookRun is a hook run's end as the record keeps it.
type museHookRun struct {
	Event   string   `json:"event"`
	HookKey string   `json:"hook_key"`
	Status  string   `json:"status"`
	Effects []string `json:"effects"`
	Stderr  string   `json:"stderr"`
	Origin  struct {
		SourceDigest string `json:"source_digest"`
	} `json:"origin"`
}

// museBlock is a context block as the record keeps it.
type museBlock struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

// decodeMuseEvent is event, a runtime record's, as the T its kind names; ok
// false where it is not that object, which museHeld reads as a
// [museUnreadLine].
func decodeMuseEvent[T any](event json.RawMessage) (T, bool) {
	var v T
	return v, json.Unmarshal(event, &v) == nil
}

// museAnswered is the take tokens of one hook run whose answer Muse took, and
// whether the run was the finish's.
type museAnswered struct {
	tokens []string
	finish bool
}

// museHeld is the take tokens r, a session's record, shows held, of the
// plugin package whose digest is digest ([museInstalledDigest]), as the
// receipt above says. A line it cannot read with a later line after it is a
// [museUnreadLine], and nothing is held.
func museHeld(r io.Reader, digest string) (map[string]bool, error) {
	held := map[string]bool{}
	awaiting := map[string]museAnswered{} // block id → the run it answers for
	pending := map[string]bool{}          // token → taken at the finish
	answers := map[string]int{}           // call → its hooks' answers so far
	call, stops := "", 0
	var unread *museUnreadLine
	n := 0
	next := func() error {
		n++
		if unread != nil {
			return unread
		}
		return nil
	}
	// A RESTART drops a finish's pending take, whose block a resumed session
	// does not restore, and every run still awaiting its block, which the new
	// session never commits; a committed PostToolUse block stays in its
	// history (measured on 1.4.25).
	restart := func() {
		for token, finish := range pending {
			if finish {
				delete(pending, token)
			}
		}
		clear(awaiting)
		clear(answers)
		call = ""
	}
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := next(); err != nil {
			return err
		}
		l, ok := decodeMuseLine(line)
		if !ok {
			unread = &museUnreadLine{at: n, why: "is not a record Muse Code writes"}
			return nil
		}
		switch {
		case l.payloadType == "session.started":
			restart()
			stops = 0
		case l.call != "":
			call = l.call
		case l.kind == "hook_run_terminal":
			h, read := decodeMuseEvent[museHookRun](l.event)
			if !read {
				unread = &museUnreadLine{at: n, why: "is a hook run's end Muse Code does not write"}
				return nil
			}
			var id string
			finish, ours := museNoteHookOf(h.HookKey)
			switch {
			case h.Event == "PostToolUse" && h.Status == "completed" && slices.Contains(h.Effects, "context"):
				id = fmt.Sprintf("hook:post_tool_use:%s:%d", call, answers[call])
				answers[call]++
			case h.Event == "Stop" && h.Status == "blocked" && slices.Contains(h.Effects, "blocked"):
				id = fmt.Sprintf("hook:stop:stop:%d", stops)
				stops++
			}
			if id == "" || !ours || h.Origin.SourceDigest != digest {
				return nil
			}
			var tokens []string
			for _, token := range strings.Split(h.Stderr, "\n") {
				if museIsToken(token) {
					tokens = append(tokens, token)
				}
			}
			awaiting[id] = museAnswered{tokens: tokens, finish: finish}
		case l.kind == "context_block_updated":
			b, read := decodeMuseEvent[museBlock](l.event)
			if !read {
				unread = &museUnreadLine{at: n, why: "is a context block Muse Code does not write"}
				return nil
			}
			if a, ok := awaiting[b.ID]; ok && b.Source == "runtime_hook" {
				delete(awaiting, b.ID)
				for _, token := range a.tokens {
					pending[token] = a.finish
				}
			}
		case l.kind == "context_compaction_installed":
			clear(pending)
			clear(awaiting)
		case l.kind == "model_completed":
			for token := range pending {
				held[token] = true
			}
			clear(pending)
			stops = 0
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

// museInstalledDigest is the digest of the package Muse Code installed, read
// off its answer to `muse plugins install --json` ([Paths.MuseNotesInstall]):
// `installed.package_sha256`, as JSON, however it is laid out; "" where the
// answer holds none, the install having failed or never run.
func museInstalledDigest(answer []byte) string {
	var a struct {
		Installed struct {
			PackageSHA256 string `json:"package_sha256"`
		} `json:"installed"`
	}
	if json.Unmarshal(answer, &a) != nil {
		return ""
	}
	return a.Installed.PackageSHA256
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
