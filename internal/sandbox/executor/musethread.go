package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
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
// The record is walked for notes ([lineWalk.noteLine]), holding no line.

var _ mainThreadHarness = Muse{}

// threadTraits is what a Muse Code run's main-thread read settles: its hooks'
// receipts, read off the session's record. It counts nothing.
func (Muse) threadTraits() sandbox.ThreadTraits {
	return sandbox.ThreadTraits{Reads: true, Receipts: true, Summaries: true}
}

// readMainThread answers the receipts of the takes ask asks: held where the
// record shows each delivered and sent; absent where the job is over and it
// does not; undecided otherwise. The same walk reads its newest summary.
func (Muse) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	var summary *summaryRead
	if ask.Summary {
		if why := summaryReleaseWhy(ask.Release, museSupported()); why != "" {
			reading.Summary = summaryUnread(why)
		} else {
			summary = &summaryRead{}
		}
	}
	takes := len(ask.Takes) > 0
	var digest string
	if takes {
		raw, err := box.ReadFile(ctx, paths.MuseNotesInstall())
		if digest = museInstalledDigest(raw); err != nil || digest == "" {
			reading.Receipts = sandbox.TakeReceipts{Why: "the run's notes plugin left no digest of its " +
				"installed package, so no hook's run can be told for the plugin's"}
			takes = false
		}
	}
	if !takes && summary == nil {
		return reading
	}
	undecided := func(why string) sandbox.MainThreadReading {
		if takes {
			reading.Receipts = sandbox.TakeReceipts{Why: why}
		}
		if summary != nil {
			reading.Summary = summaryUnread(why)
		}
		return reading
	}
	record, err := museRecord(ctx, box, paths, ask.Session)
	if err != nil {
		return undecided(err.Error())
	}
	asked := make(map[string]bool, len(ask.Takes))
	for _, take := range ask.Takes {
		if takes {
			asked[claudeTakeToken(take.Take)] = true
		}
	}
	held, err := museHeld(ctx, record, digest, asked, summary)
	_ = record.Close()
	if err != nil {
		return undecided("the run's session record could not be read whole: " + err.Error())
	}
	if summary != nil {
		reading.Summary = summary.reading(ask)
	}
	if !takes {
		return reading
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
	reading.Receipts = receipts
	return reading
}

// museRecord opens session's record in box, under whatever day it is filed.
func museRecord(ctx context.Context, box sandbox.Sandbox, paths Paths, session string) (io.ReadCloser, error) {
	if !sandbox.ValidSessionID(session) {
		return nil, fmt.Errorf("the run's Muse Code session %q is no session id its record is named by", session)
	}
	res, err := box.Exec(ctx, `for y in `+shellword.Quote(paths.MuseSessions())+`/*/*/*/`+
		shellword.Quote(session)+`; do [ -f "$y/session.jsonl" ] && printf '%s\n' "$y/session.jsonl"; done`,
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

// museAnswered is the take tokens of one hook run whose answer Muse took, and
// whether the run was the finish's.
type museAnswered struct {
	tokens []string
	finish bool
}

// museHeld is the take tokens of asked r, a session's record, shows held, of
// the plugin package whose digest is digest ([museInstalledDigest]), as the
// receipt above says, reading its newest summary into summary where it is not
// nil ([museNoteLine.summarised]). A line it cannot read with a later line
// after it is a [museUnreadLine], and nothing is held. A token not asked is
// not kept: a hook run's stderr can name any number.
func museHeld(ctx context.Context, r io.Reader, digest string, asked map[string]bool,
	summary *summaryRead) (map[string]bool, error) {
	m := museReceipts{digest: digest, held: map[string]bool{}, awaiting: map[recordWord]museAnswered{},
		pending: map[string]bool{}, answers: map[string]int{}}
	var unread *museUnreadLine
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(r, lineChunkBytes))
	l := museNoteLine{run: museRunRead{stderr: tokenLines{asked: asked}}}
	if summary != nil {
		l.summary = &summaryText{}
	}
	for n := 1; ; n++ {
		line, err := w.noteLine(ctx, func() error { return l.read(&w) })
		switch {
		case err != nil:
			return nil, err
		case line.none:
			return m.held, nil
		case unread != nil:
			return nil, unread
		}
		switch {
		case line.past:
			unread = &museUnreadLine{at: n, why: fmt.Sprintf("is %d bytes, past the %d read of a line", line.size,
				maxLineBytes)}
		case line.null:
		case !line.object || !l.whole:
			unread = &museUnreadLine{at: n, why: "is not a record Muse Code writes"}
		default:
			if why := m.take(&l); why != "" {
				unread = &museUnreadLine{at: n, why: why}
			} else if summary != nil {
				l.summarised(summary)
			}
		}
		if line.ended {
			return m.held, nil
		}
	}
}

// museReceipts is [museHeld]'s reading of a record so far: the tokens held,
// the hook runs awaiting their blocks, the tokens pending a request, each
// call's hooks' answers, the call the record is at and the finish's blocks.
type museReceipts struct {
	digest   string
	held     map[string]bool
	awaiting map[recordWord]museAnswered // block id → the run it answers for
	pending  map[string]bool             // token → taken at the finish
	answers  map[string]int              // call → its hooks' answers so far
	call     recordWord
	stops    int
}

// take reads l, a line read whole, and is why it is no line Muse Code writes,
// "" where it is one. A CALL ID PAST 256 BYTES ([recordWord]), which Muse never
// writes, names no block a hook's run awaits: its hooks' tokens are not held,
// and their notes are served again.
func (m *museReceipts) take(l *museNoteLine) string {
	kind, shaped := l.kind()
	switch {
	case !shaped:
		return "is not a record Muse Code writes"
	case l.payloadType == "session.started":
		m.restart()
		m.stops = 0
	case l.terminal():
		m.call = l.call
	case kind == "hook_run_terminal":
		h := &l.run
		if !h.whole {
			return "is a hook run's end Muse Code does not write"
		}
		var id string
		finish, ours := museNoteHookOf(h.hookKey)
		switch {
		case h.event == "PostToolUse" && h.status == "completed" && h.context && !m.call.long:
			id = fmt.Sprintf("hook:post_tool_use:%s:%d", m.call.text, m.answers[m.call.text])
			m.answers[m.call.text]++
		case h.event == "Stop" && h.status == "blocked" && h.blocked:
			id = fmt.Sprintf("hook:stop:stop:%d", m.stops)
			m.stops++
		}
		if id != "" && ours && h.digest == m.digest {
			m.awaiting[recordWordOf(id)] = museAnswered{tokens: h.stderr.taken(), finish: finish}
		}
	case kind == "context_block_updated":
		b := &l.block
		if !b.whole {
			return "is a context block Muse Code does not write"
		}
		if a, ok := m.awaiting[b.id]; ok && b.source == "runtime_hook" {
			delete(m.awaiting, b.id)
			for _, token := range a.tokens {
				m.pending[token] = a.finish
			}
		}
	case kind == "context_compaction_installed":
		clear(m.pending)
		clear(m.awaiting)
	case kind == "model_completed":
		for token := range m.pending {
			m.held[token] = true
		}
		clear(m.pending)
		m.stops = 0
	}
	return ""
}

// restart is a session's restart: it drops a finish's pending take, whose
// block a resumed session does not restore, and every run still awaiting its
// block, which the new session never commits; a committed PostToolUse block
// stays in its history (measured on 1.4.25).
func (m *museReceipts) restart() {
	for token, finish := range m.pending {
		if finish {
			delete(m.pending, token)
		}
	}
	clear(m.awaiting)
	clear(m.answers)
	m.call = recordWord{}
}

// museNoteLine is what [museHeld] reads of one line of a session's record:
// its payload type, a terminal tool batch effect's call, and its runtime
// event, if any, as a hook run's end and as a context block; whole where each
// member its whole-line decode reads is of its field's kind ([fieldKind]).
type museNoteLine struct {
	payloadType, recordKind string
	call                    recordWord
	whole                   bool
	// event says the payload holds one, eventKind its kind, and kindOK
	// whether it decodes as an object of a kind.
	event, kindOK bool
	eventKind     string
	run           museRunRead
	block         museBlockRead
	// summary, where a read asks one, is the event's replacement's text.
	summary *summaryText
}

// museRunRead is what a hook run's end holds: its `event`, `hook_key`,
// `status`, `effects`, `origin.source_digest` and the asked tokens of its
// `stderr`; whole where each is of its field's kind, strings and a list of
// them.
type museRunRead struct {
	event, hookKey, status, digest string
	context, blocked, whole        bool
	stderr                         tokenLines
}

// museBlockRead is what a context block holds: its `id` and `source`; whole
// where each is a string.
type museBlockRead struct {
	id     recordWord
	source string
	whole  bool
}

// read reads one line's members.
func (l *museNoteLine) read(w *lineWalk) error {
	l.payloadType, l.whole = "", true
	l.payloadStart()
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "payload_type":
			l.payloadType, ok, err = w.wordField(k)
		case "payload":
			l.payloadStart()
			ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				switch name {
				case "event":
					l.event = true
					return l.readEvent(w, k)
				case "record":
					l.recordKind, l.call = "", recordWord{}
					took, recordErr := w.objectField(k, func(name string, k jsonscan.Kind) error {
						var field bool
						var fieldErr error
						switch name {
						case "kind":
							l.recordKind, field, fieldErr = w.wordField(k)
						case "call_id":
							l.call, field, fieldErr = w.idField(k)
						default:
							return w.scan.Skip()
						}
						l.whole = l.whole && field
						return fieldErr
					})
					l.whole = l.whole && took
					return recordErr
				}
				return w.scan.Skip()
			})
		default:
			return w.scan.Skip()
		}
		l.whole = l.whole && ok
		return err
	})
}

// payloadStart starts the line's payload: one written again replaces it.
func (l *museNoteLine) payloadStart() {
	l.recordKind, l.call = "", recordWord{}
	l.event, l.kindOK, l.eventKind = false, true, ""
	l.runStart()
	l.block = museBlockRead{whole: true}
}

// runStart starts the event's reading as a hook run's end.
func (l *museNoteLine) runStart() {
	stderr := l.run.stderr
	stderr.reset()
	l.run = museRunRead{whole: true, stderr: stderr}
}

// readEvent reads a runtime record's event, any value, as an object of a
// kind, a hook run's end and a context block at once: null decodes as each,
// holding none of their members.
func (l *museNoteLine) readEvent(w *lineWalk, k jsonscan.Kind) error {
	l.kindOK, l.eventKind = true, ""
	l.runStart()
	l.block = museBlockRead{whole: true}
	if l.summary != nil {
		l.summary.reset()
	}
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ObjectStart:
	default:
		l.kindOK, l.run.whole, l.block.whole = false, false, false
		return w.scan.Skip()
	}
	h, blk := &l.run, &l.block
	return w.members(func(name string, k jsonscan.Kind) error {
		var took bool
		var err error
		switch name {
		case "kind":
			l.eventKind, took, err = w.wordField(k)
			l.kindOK = l.kindOK && took
			return err
		case "id":
			blk.id, took, err = w.idField(k)
			blk.whole = blk.whole && took
			return err
		case "source":
			blk.source, took, err = w.wordField(k)
			blk.whole = blk.whole && took
			return err
		case "replacement":
			if l.summary == nil {
				return w.scan.Skip()
			}
			l.summary.reset()
			_, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "text" {
					return w.scan.Skip()
				}
				_, textErr := l.summary.read(w, k)
				return textErr
			})
			return err
		case "event":
			h.event, took, err = w.wordField(k)
		case "hook_key":
			h.hookKey, took, err = w.wordField(k)
		case "status":
			h.status, took, err = w.wordField(k)
		case "stderr":
			h.stderr.reset()
			took = k == jsonscan.String || k == jsonscan.Null
			switch k {
			case jsonscan.String:
				err = w.read(&h.stderr)
				h.stderr.end()
			default:
				err = w.scan.Skip()
			}
		case "effects":
			h.context, h.blocked = false, false
			took, err = w.listField(k, func(k jsonscan.Kind) error {
				effect, ok, effectErr := w.wordField(k)
				h.whole = h.whole && ok
				h.context = h.context || effect == "context"
				h.blocked = h.blocked || effect == "blocked"
				return effectErr
			})
		case "origin":
			h.digest = ""
			took, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "source_digest" {
					return w.scan.Skip()
				}
				var ok bool
				var digestErr error
				h.digest, ok, digestErr = w.wordField(k)
				h.whole = h.whole && ok
				return digestErr
			})
		default:
			return w.scan.Skip()
		}
		h.whole = h.whole && took
		return err
	})
}

// kind is the kind of the line's runtime event as [museHeld] reads it, ""
// for a line of another payload type or of no event (a record of another
// kind, `agent_tree_initialized`, carries none, measured on 1.4.25); false
// for an event that is no object of a kind.
func (l *museNoteLine) kind() (string, bool) {
	switch {
	case l.payloadType != "runtime.session" || !l.event:
		return "", true
	case !l.kindOK:
		return "", false
	}
	return l.eventKind, true
}

// summarised makes the event's replacement's text the newest summary where
// the line installs a compaction, as the transcript reads one
// ([museReading.commit]).
func (l *museNoteLine) summarised(s *summaryRead) {
	if kind, shaped := l.kind(); shaped && kind == "context_compaction_installed" {
		s.taken(l.summary)
	}
}

// terminal reports whether the line is a terminal tool batch effect naming
// its call.
func (l *museNoteLine) terminal() bool {
	return l.payloadType == "tool_batch.effect.terminal" && l.recordKind == "terminal" && !l.call.empty()
}

// tokenLines reads a text as it is written for the take tokens of asked it
// holds whole on a line of their own ([museIsToken]), each once, holding a
// token's length of a line.
type tokenLines struct {
	asked  map[string]bool
	line   []byte
	tokens []string
}

// reset starts a text.
func (t *tokenLines) reset() { t.line, t.tokens = t.line[:0], t.tokens[:0] }

func (t *tokenLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		seg := p
		if i >= 0 {
			seg = p[:i]
		}
		room := claudeTakeTokenLen + 1 - len(t.line)
		t.line = append(t.line, seg[:max(0, min(room, len(seg)))]...)
		if i < 0 {
			break
		}
		t.end()
		p = p[i+1:]
	}
	return n, nil
}

// end ends the line being read.
func (t *tokenLines) end() {
	if len(t.line) == claudeTakeTokenLen && t.asked[string(t.line)] && museIsToken(string(t.line)) &&
		!slices.Contains(t.tokens, string(t.line)) {
		t.tokens = append(t.tokens, string(t.line))
	}
	t.line = t.line[:0]
}

// taken is the tokens read, the reader's own to keep.
func (t *tokenLines) taken() []string { return slices.Clone(t.tokens) }

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
