package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHICH OF A ROUND'S CALLS A GEMINI CLI SESSION HOLDS, read off its kept chat
// file by what each call was ([Runner.ReadHoldings]): Gemini CLI hands the
// bridge no call id, so, as OpenCode's reading does, the reading counts the
// run's parts of each class of calls ([sandbox.CallClass]) the resume hands
// its model verbatim, and the verdict by content ([contentVerdict]) holds a
// class only where every call of it has a part. The walk (geminiwalk.go)
// replays the file a line at a time as 0.70.0's loader does. The package
// doc's "A session's calls are read by what they were" has the rules;
// ADR-0073's amendment "a cut-off Gemini CLI session is read by content, from
// the lines its take verified" what they leave.

// geminiHoldingsReleases are the Gemini CLI releases whose chat files a
// reading reads, each compared whole: those its rules were measured on,
// writer and resume alike (testdata/gemini/0.70.0/holdings). A set, never a
// range: a later release joins once measured the same way.
var geminiHoldingsReleases = []string{"0.70.0"}

// geminiHoldingsRead reports whether release is one of
// [geminiHoldingsReleases], compared whole.
func geminiHoldingsRead(release string) bool { return slices.Contains(geminiHoldingsReleases, release) }

// holdingsFloor is the oldest release of [geminiHoldingsReleases]
// ([releaseSetFloor]).
func (GeminiCLI) holdingsFloor() string { return releaseSetFloor(geminiHoldingsReleases) }

// checkHoldings holds ask, and the archive its row describes, to what a Gemini
// CLI reading answers, before anything is opened: a writer the set reads, the
// archive's and the ask's alike; a session id that is a canonical lowercase
// uuid, as the runner mints one; a run that began its session or after a
// count of its lines the take verified; a run whose box found no other server
// that could spell the bridge's tools ([SessionArchive.Unshadowed]); and an
// ask a round's log can make ([logPlaceAskFault]).
func (GeminiCLI) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	switch {
	case !geminiHoldingsRead(ask.Resumer) || archive.Version != ask.Resumer:
		return &holdingsError{why: fmt.Sprintf("Gemini CLI %.64q wrote the session, and a reading reads only "+
			"the releases its rules were measured on (geminiHoldingsRead, internal/sandbox/executor)",
			archive.Version)}
	case !canonicalUUID(archive.SessionID) || !sandbox.ValidSessionID(archive.SessionID):
		return &holdingsError{why: "the session's id names no Gemini CLI session"}
	case !archive.RunStart.Known() || archive.RunStart.After != "":
		return &holdingsError{why: "the run that packed the session recorded no line it began after"}
	case !archive.Unshadowed:
		return &holdingsError{why: "the run's box was not found free of another MCP server that could name a " +
			"tool as Gemini CLI names the engine's"}
	}
	if why := logPlaceAskFault(ask); why != "" {
		return &holdingsError{why: "the relaunch asked about calls its log cannot have: " + why}
	}
	return nil
}

// canonicalUUID reports whether id is a uuid as its canonical lowercase form
// spells it.
func canonicalUUID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// readHoldings reads body, archive's gzipped tar, within bounds' plain meter
// ([readGeminiHoldings]), the plaintext and the body each drained to io.EOF
// before it answers.
func (GeminiCLI) readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
	ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error) {

	zr, err := gzip.NewReader(body)
	if err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session's archive does not read as one archive",
			cause: err}
	}
	plain := boundedPlain(zr, bounds.plain)
	holdings, err := readGeminiHoldings(ctx, plain, archive.SessionID, archive.RunStart, ask)
	if err != nil {
		return sandbox.SessionHoldings{}, err
	}
	// DRAINED TO io.EOF: the gzip's checksum and the store's stream are each
	// provisional until then.
	if _, err = io.Copy(io.Discard, plain); err != nil {
		return sandbox.SessionHoldings{}, chatReadFailed(ctx, err)
	}
	if _, err = io.Copy(io.Discard, body); err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{
			why: "the session's stream did not end where its archive did", cause: err}
	}
	return holdings, nil
}

// readGeminiHoldings reads plain, a session's tar as kept (the take's scrub's
// output), for the calls ask marks asked: its one chat file
// ([chatHoldingsMember]), each line ([chatLines]) through the separator guard
// ([geminiSeparators]) to the walk ([geminiHoldingsWalk]), nothing after it in
// the tar; then the verdict, a pure function of the walk's facts and the ask.
func readGeminiHoldings(ctx context.Context, plain io.Reader, session string, start sandbox.RunStart,
	ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	tr, err := chatHoldingsMember(plain, geminiChatMember)
	if err != nil {
		return sandbox.SessionHoldings{}, chatReadFailed(ctx, err)
	}
	w := newGeminiHoldingsWalk(ctx, session, start, ask)
	lines, err := chatLines(ctx, &geminiSeparators{r: tr}, w.line)
	if err == nil {
		err = chatHoldingsEnd(tr)
	}
	if err != nil {
		return sandbox.SessionHoldings{}, chatReadFailed(ctx, err)
	}
	return w.ended(ask, lines)
}

// errGeminiSeparator is a chat file holding a line separator the resume's
// loader splits a line at, or the scrub's escape of one, or a carriage
// return.
var errGeminiSeparator = &holdingsError{why: "the session's chat file holds a line separator (U+2028, U+2029, " +
	"raw or escaped, or a carriage return) its harness's loader may split a line at"}

// geminiSeparators reads r, a chat file, failing at the first U+2028 or
// U+2029 raw, written as a `\u` escape (hex in any case, the scrub writes
// them so), or a carriage return, anywhere in the file: 0.70.0's loader
// drops a line holding one raw, and the box's own copy may hold one raw where
// the kept copy holds its escape. carry is the last bytes read, so an escape
// across two reads is found.
type geminiSeparators struct {
	r       io.Reader
	carry   [geminiSeparatorSpan - 1]byte
	carried int
	err     error
}

// geminiSeparatorSpan is the longest separator as written, an escape's six
// bytes.
const geminiSeparatorSpan = len(`\u2028`)

// Read reads r, answering errGeminiSeparator from the read that holds one.
func (s *geminiSeparators) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		var buf [2 * geminiSeparatorSpan]byte
		edge := append(append(buf[:0], s.carry[:s.carried]...), p[:min(n, len(s.carry))]...)
		if geminiSeparatorIn(edge) || geminiSeparatorIn(p[:n]) {
			s.err = errGeminiSeparator
			return 0, s.err
		}
		if n >= len(s.carry) {
			s.carried = copy(s.carry[:], p[n-len(s.carry):n])
		} else {
			s.carried = copy(s.carry[:], edge[max(0, len(edge)-len(s.carry)):])
		}
	}
	return n, err
}

// geminiSeparatorIn reports whether b holds a separator [geminiSeparators]
// refuses.
func geminiSeparatorIn(b []byte) bool {
	if bytes.IndexByte(b, '\r') >= 0 || bytes.Contains(b, []byte("\u2028")) || bytes.Contains(b, []byte("\u2029")) {
		return true
	}
	for i := 0; i+geminiSeparatorSpan <= len(b); i++ {
		j := bytes.IndexByte(b[i:], '\\')
		if j < 0 || i+j+geminiSeparatorSpan > len(b) {
			return false
		}
		i += j
		if e := b[i : i+geminiSeparatorSpan]; (e[1] == 'u' || e[1] == 'U') && string(e[2:5]) == "202" &&
			(e[5] == '8' || e[5] == '9') {
			return true
		}
	}
	return false
}
