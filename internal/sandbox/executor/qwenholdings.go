package executor

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHICH OF A ROUND'S CALLS A QWEN CODE SESSION HOLDS, read off its kept chat
// file by what each call was ([Runner.ReadHoldings]): Qwen Code hands the
// bridge no call id, so the reading counts the run's parts of each class of
// calls the continuation hands its model verbatim, and holds a class where
// every call of it has one ([contentVerdict]). The file is read a line at a
// time ([chatLines]), only where it is the one chain its load walks, each
// uuid once ([qwenHoldingsWalk]); a part names a bridge tool only by a name no
// other server can spell ([qwenBridgedTool]). ADR-0073's amendment "a cut-off
// Qwen Code session is read by content, along the chain its load walks" has
// the rules, their measurements and what they leave.

// qwenHoldingsReleases are the releases a reading reads, each one its rules
// were measured on: a set compared whole, never a range, each the release the
// build drives ([qwenRelease]).
var qwenHoldingsReleases = []string{"0.28.4"}

// qwenHoldingsRead reports whether release is one of [qwenHoldingsReleases],
// compared whole, never trimmed.
func qwenHoldingsRead(release string) bool { return slices.Contains(qwenHoldingsReleases, release) }

// qwenBridgedPrefix opens the name Qwen Code gives each tool of the engine's
// server, as its built-in profile states it
// ([harnessprofile.Profile.ModelToolPrefix]): `mcp__crewlet__`. A profile
// stating none leaves it "", which names no tool, so every call is told.
var qwenBridgedPrefix = sync.OnceValue(func() string {
	p, ok := harnessprofile.Builtin(QwenCodeName)
	if !ok {
		return ""
	}
	return p.ModelToolPrefix()
})

// qwenProviderNameMax is 0.28.4's MAX_TOOL_NAME_LENGTH: a name past it is cut
// there, after a hash of the whole, so a 63-byte name may be another server's.
const qwenProviderNameMax = 63

// qwenToolNameMost is the longest bridge tool a part names unambiguously:
// what the 14 bytes of `mcp__crewlet__` leave below a cut name's length.
const qwenToolNameMost = qwenProviderNameMax - 14 - 1

// qwenStubPrefixes open each text 0.28.4 hands its model in place of a tool's
// output (its STUB_PRODUCER_PREFIXES, and the batch budget's rewrite): a part
// opening with one is never the output verbatim.
var qwenStubPrefixes = []string{"<persisted-output>", "Output too large (", "Tool output was too large",
	"Tool output truncated."}

// qwenFailureWords is what 0.28.4 hands its model for a failure that sent no
// text; qwenNoOutput opens and closes its words for an empty output.
const (
	qwenFailureWords = "Tool reported an error."
	qwenNoOutputOpen = "("
	qwenNoOutputEnd  = " completed with no output)"
)

// qwenBridgedTool is the engine's tool a call's or an answer's `name` names:
// [qwenBridgedPrefix] and a tool no other server's name can reach
// ([qwenToolUnambiguous]), its raw span held to that bound before it is
// decoded.
func qwenBridgedTool(raw json.RawMessage) (string, bool) {
	prefix := qwenBridgedPrefix()
	most := len(prefix) + qwenToolNameMost
	if prefix == "" || len(raw) < 2 || raw[0] != '"' || len(raw) > 6*most+2 {
		return "", false
	}
	var name string
	if wireDecode(raw, &name) != nil {
		return "", false
	}
	tool, ok := strings.CutPrefix(name, prefix)
	if !ok || !qwenToolUnambiguous(tool) {
		return "", false
	}
	return tool, true
}

// qwenToolUnambiguous reports whether tool, after `mcp__crewlet__`, is one no
// other server's tool is named as: 1 to [qwenToolNameMost] bytes of
// `[A-Za-z0-9_-]`, not opening with `_` and holding no `__`. Another server
// reaches the name only through one of the three (the amendment's proof).
func qwenToolUnambiguous(tool string) bool {
	if tool == "" || len(tool) > qwenToolNameMost || tool[0] == '_' || strings.Contains(tool, "__") {
		return false
	}
	for i := range len(tool) {
		switch c := tool[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// qwenUUIDKey is a checked uuid's key in [onceEach]: its last 64 bits, the
// variant's 62 random ones among them (Qwen Code draws a version-4 uuid).
func qwenUUIDKey(id [36]byte) uint64 {
	var key uint64
	for i := 19; i < len(id); i++ {
		c := id[i]
		switch {
		case c == '-':
			continue
		case c <= '9':
			key = key<<4 | uint64(c-'0')
		default:
			key = key<<4 | uint64(c-'a'+10)
		}
	}
	return key
}

// qwenUUID reports whether id is a uuid as Qwen Code writes one: canonical,
// lowercase.
func qwenUUID[T string | []byte](id T) bool {
	if len(id) != 36 {
		return false
	}
	for i := range len(id) {
		switch c := id[i]; i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// holdingsFloor is the oldest release of [qwenHoldingsReleases]
// ([releaseSetFloor]).
func (QwenCode) holdingsFloor() string { return releaseSetFloor(qwenHoldingsReleases) }

// checkHoldings holds ask, and the archive its row describes, to what a Qwen
// Code reading answers, before anything is opened: a writer the set reads, the
// archive's and the ask's alike, compared whole; a session id Qwen Code
// mints; a run whose start is the session's beginning or a count of its lines;
// and an ask a log keyed by place can make ([logPlaceAskFault]).
func (QwenCode) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	switch {
	case !qwenHoldingsRead(ask.Resumer) || archive.Version != ask.Resumer:
		return &holdingsError{why: fmt.Sprintf("Qwen Code %.64q wrote the session, and a reading reads only the "+
			"releases its rules were measured on (qwenHoldingsRead, internal/sandbox/executor)", archive.Version)}
	case !qwenUUID(archive.SessionID) || !sandbox.ValidSessionID(archive.SessionID):
		return &holdingsError{why: "the session's id names no Qwen Code session"}
	case !archive.RunStart.Known() || archive.RunStart.After != "":
		return &holdingsError{why: "the run that packed the session recorded no line it began after"}
	}
	if why := logPlaceAskFault(ask); why != "" {
		return &holdingsError{why: "the relaunch asked about calls its log cannot have: " + why}
	}
	return nil
}

// readHoldings reads body, archive's gzipped tar, within bounds' plain meter
// ([readQwenHoldings]), and drains it to io.EOF before answering.
func (QwenCode) readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
	ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error) {

	zr, err := gzip.NewReader(body)
	if err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session's archive does not read as one chat file",
			cause: err}
	}
	holdings, err := readQwenHoldings(ctx, boundedPlain(zr, bounds.plain), archive.SessionID, archive.RunStart, ask)
	if err != nil {
		return sandbox.SessionHoldings{}, err
	}
	// DRAINED TO io.EOF: what the store hands out is provisional until then.
	if _, err = io.Copy(io.Discard, body); err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{
			why: "the session's stream did not end where its archive did", cause: err}
	}
	return holdings, nil
}

// readQwenHoldings reads plain, a session's tar as kept, for the calls ask
// marks asked: its one chat file, the session's own ([qwenChatMember]), a line
// at a time ([chatLines]) by the walk ([qwenHoldingsWalk]), nothing after it;
// then the verdict ([contentVerdict]). No line is held.
func readQwenHoldings(ctx context.Context, plain io.Reader, session string, start sandbox.RunStart,
	ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	tr, err := chatHoldingsMember(plain, func(name string) bool {
		return qwenChatMember(name) && path.Base(name) == session+".jsonl"
	})
	if err != nil {
		return sandbox.SessionHoldings{}, chatReadFailed(ctx, err)
	}
	w := newQwenHoldingsWalk(session, start, ask)
	lines, err := chatLines(ctx, tr, func(n int, body io.Reader) error { return w.line(ctx, n, body) })
	if err == nil {
		err = chatHoldingsEnd(tr)
	}
	if err != nil {
		return sandbox.SessionHoldings{}, chatReadFailed(ctx, err)
	}
	return w.ended(ask, lines)
}
