package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// THE WHOLE-ROW READS OF A HERMES RECORD, a person's and a collection's, as
// they were before they moved onto the line walk: the oracles the walk is held
// to, with what the walk decides applied where it decides it
// (hermessession.go): every line within the bound counts toward the lines
// bound, one past it is said in the walk's words and counts toward none; a key
// matches its name exactly; a word past [transcriptNameMaxBytes] reads as
// none; past [hermesUIDsMax] uids a person's read ends said and a collection's
// is refused; past the value bound R1, R2 and R6 (linewhole_internal_test.go),
// and a text that opens with a steered note's framing is read without it; and
// a collection's parse matches no part past [hermesNoteMaxBytes], nor any of a
// row whose list part's text is not written once after its one type.

// wholeHermes reads a Hermes archive with the whole-row oracle.
type wholeHermes struct{ Hermes }

func (wholeHermes) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if h.Typeflag != tar.TypeReg || h.Name != hermesArchiveMember {
			continue
		}
		if err := b.hermesRecordWhole(ctx, &b.handed); err != nil {
			return err
		}
	}
}

// piecesHermes reads a Hermes archive on the walk, its inflated bytes handed
// n a read.
type piecesHermes struct {
	Hermes
	n int
}

func (p piecesHermes) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Hermes{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// hermesRowWhole is one row of a dumped record, the parts the readers take.
type hermesRowWhole struct {
	Table   string
	Message hermesMessageWhole
}

type hermesMessageWhole struct {
	ID                int64           `json:"id"`
	Active            *int            `json:"active"`
	Compacted         *int            `json:"compacted"`
	CompressedSummary *int            `json:"_compressed_summary"`
	FinishReason      *string         `json:"finish_reason"`
	DisplayKind       *string         `json:"display_kind"`
	UID               string          `json:"message_uid"`
	Role              string          `json:"role"`
	Content           json.RawMessage `json:"content"`
	ToolCalls         json.RawMessage `json:"tool_calls"`
	ToolCallID        string          `json:"tool_call_id"`
	ToolName          string          `json:"tool_name"`
	Timestamp         json.RawMessage `json:"timestamp"`
}

func (m hermesMessageWhole) text() string {
	var s string
	if wireDecode(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if wireDecode(m.Content, &parts) != nil {
		return ""
	}
	var out string
	for _, p := range parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// textPast is the message's words as the walk reads them at valueMax: lost
// where a list's part past it is out of order (R2), and past it unframed.
func (m hermesMessageWhole) textPast(valueMax int) (string, bool) {
	text := m.text()
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if trimmed := bytes.TrimSpace(m.Content); len(trimmed) > 0 && trimmed[0] == '[' &&
		wireDecode(m.Content, &parts) == nil {
		var lost bool
		if text, lost = joinedWhole(m.Content, hermesParts, valueMax); lost {
			return "", true
		}
	}
	if len(text) > valueMax {
		text = strings.TrimSuffix(strings.TrimPrefix(text, hermesNoteOpen), hermesNoteClose)
	}
	return text, false
}

func (m hermesMessageWhole) current() bool {
	return (m.Active == nil || *m.Active != 0) && (m.Compacted == nil || *m.Compacted == 0) && !m.summary()
}

func (m hermesMessageWhole) summary() bool {
	return m.CompressedSummary != nil && *m.CompressedSummary != 0
}

func (m hermesMessageWhole) replied() bool {
	return m.Role == "assistant" && m.FinishReason != nil && *m.FinishReason != ""
}

func (m hermesMessageWhole) at() time.Time {
	if len(bytes.TrimSpace(m.Timestamp)) > transcriptNameMaxBytes+2 {
		return time.Time{}
	}
	var f float64
	if wireDecode(m.Timestamp, &f) != nil {
		var s string
		if wireDecode(m.Timestamp, &s) != nil {
			return time.Time{}
		}
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}
		}
		f = parsed
	}
	if f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return time.Time{}
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

type hermesCallWhole struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Args     json.RawMessage `json:"arguments"`
	Function *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func (m hermesMessageWhole) calls() []hermesCallWhole {
	raw := []byte(m.ToolCalls)
	var s string
	if wireDecode(raw, &s) == nil {
		raw = []byte(s)
	}
	var out []hermesCallWhole
	if wireDecode(raw, &out) != nil {
		return nil
	}
	for i, c := range out {
		if c.Function != nil {
			out[i].Name, out[i].Args = firstNonBlank(c.Name, c.Function.Name), c.Function.Args
			if len(c.Args) > 0 {
				out[i].Args = c.Args
			}
		}
	}
	return out
}

func hermesArgsWhole(raw json.RawMessage) string {
	var s string
	if wireDecode(raw, &s) == nil {
		return s
	}
	return compactJSON(raw)
}

// hermesWholeLines hands each line of r, its break among it where it has one,
// to each.
func hermesWholeLines(r io.Reader, each func(line []byte) error) error {
	br := bufio.NewReaderSize(r, lineChunkBytes)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if eachErr := each(line); eachErr != nil {
				return eachErr
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

func hermesRowOfWhole(line []byte) (hermesRowWhole, bool) {
	var wire struct {
		Table string          `json:"t"`
		Row   json.RawMessage `json:"r"`
	}
	if wireDecode(line, &wire) != nil || wordWhole(wire.Table) == "" {
		return hermesRowWhole{}, false
	}
	row := hermesRowWhole{Table: wire.Table}
	if wire.Table == "messages" && wireDecode(wire.Row, &row.Message) != nil {
		return hermesRowWhole{}, false
	}
	row.Message.Role, row.Message.ToolCallID = wordWhole(row.Message.Role), wordWhole(row.Message.ToolCallID)
	return row, true
}

func (b *transcriptBuilder) hermesRecordWhole(ctx context.Context, r io.Reader) error {
	framed := 0
	read := map[[16]byte]struct{}{}
	return hermesWholeLines(r, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(line) > hermesRowMaxBytes {
			return b.pastWhole(len(line), hermesRowMaxBytes)
		}
		if framed++; framed > b.bounds.ReadLines {
			return b.linesPast("Hermes session", "rows")
		}
		row, ok := hermesRowOfWhole(bytes.TrimSpace(line))
		if !ok || row.Table != "messages" || hermesCopy(read, recordWordOf(row.Message.UID)) {
			return nil
		}
		if len(read) > hermesUIDsMax {
			return b.uidsPast()
		}
		return b.hermesMessageWhole(row.Message)
	})
}

func (b *transcriptBuilder) hermesMessageWhole(m hermesMessageWhole) error {
	at := m.at()
	text, lost := m.textPast(b.valueMax)
	prose := func(kind EntryKind) (bool, error) {
		if lost {
			return true, b.lostJoin()
		}
		if kind == EntryBrief && len(m.text()) <= b.valueMax {
			if note, ok := hermesSteered(text); ok {
				text = note
			}
		}
		if strings.TrimSpace(text) == "" {
			return false, nil
		}
		if kind == EntryBrief && len(m.text()) <= b.valueMax && opensWithHeading(text) {
			for heading, words := range headedIn(text) {
				if err := b.add(TranscriptEntry{Kind: heading, At: at}, words); err != nil {
					return true, err
				}
			}
			return true, nil
		}
		return true, b.add(TranscriptEntry{Kind: kind, At: at}, text)
	}
	if m.summary() {
		_, err := prose(EntrySummary)
		return err
	}
	switch m.Role {
	case "user":
		_, err := prose(EntryBrief)
		return err
	case "assistant":
		if _, err := prose(EntryModel); err != nil {
			return err
		}
		for _, c := range m.calls() {
			if b.refusedName(c.Name) {
				if err := b.add(TranscriptEntry{Kind: EntryUnread}, unredactedText); err != nil {
					return err
				}
				continue
			}
			b.called(c.ID, c.Name)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, Tool: c.Name, At: at}, hermesArgsWhole(c.Args)); err != nil {
				return err
			}
		}
		return nil
	case "tool":
		name := m.ToolName
		if b.refusedName(name) {
			name = ""
		}
		tool := firstNonBlank(b.answered(m.ToolCallID), name)
		if lost {
			return b.lostJoin()
		}
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: tool, At: at}, text)
	}
	return nil
}

// hermesNotesHeldWhole is the whole-row read of a collection's receipts.
func hermesNotesHeldWhole(r io.Reader, takes []ServedTake, from int64) ([]string, error) {
	type match struct {
		inbox string
		row   int64
	}
	var settled []string
	var pending []match
	next := 0
	earlier := map[string]bool{}
	err := readLines(bufio.NewReaderSize(r, 64<<10), hermesRowMaxBytes, func(line []byte) error {
		row, ok := hermesRowOfWhole(line)
		if !ok {
			return nil
		}
		m := row.Message
		if row.Table != "messages" {
			return nil
		}
		if m.ID <= from {
			if m.UID != "" {
				earlier[m.UID] = true
			}
			if len(earlier) > hermesUIDsMax {
				return errHermesRecordUIDs
			}
			return nil
		}
		if !m.current() || earlier[m.UID] {
			return nil
		}
		if m.UID != "" {
			earlier[m.UID] = true
		}
		if len(earlier) > hermesUIDsMax {
			return errHermesRecordUIDs
		}
		switch {
		case m.replied():
			kept := pending[:0]
			for _, p := range pending {
				if p.row < m.ID {
					settled = append(settled, p.inbox)
				} else {
					kept = append(kept, p)
				}
			}
			pending = kept
		case m.Role == "user" && m.DisplayKind != nil && *m.DisplayKind == "steer" && !hermesUnsureWhole(m.Content):
			if inner, ok := hermesSteered(m.text()); ok && inner != "" {
				base := next
				for _, at := range hermesRowNotesWhole(inner, takes[base:]) {
					pending = append(pending, match{takes[base+at].InboxID, m.ID})
					next = base + at + 1
				}
			}
		}
		return nil
	}, func(int64) error {
		pending = pending[:0]
		return nil
	})
	if errors.Is(err, errHermesRecordUIDs) {
		return nil, fmt.Errorf("%w: past %d", errHermesRecordUIDs, hermesUIDsMax)
	}
	return settled, err
}

// hermesUnsureWhole reports whether a list content holds a part whose string
// text is not written once after its one type.
func hermesUnsureWhole(content json.RawMessage) bool {
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return false
	}
	for _, part := range parts {
		kinds, texts, seen, unsure := 0, 0, false, false
		_ = eachKey(part, func(name []byte, value json.RawMessage, _ int) error {
			switch string(name) {
			case "type":
				kinds++
			case "text":
				texts++
				if v := bytes.TrimSpace(value); len(v) > 0 && v[0] == '"' {
					seen, unsure = true, unsure || kinds != 1
				}
			}
			return nil
		})
		if unsure || seen && (kinds != 1 || texts != 1) {
			return true
		}
	}
	return false
}

func hermesRowNotesWhole(joined string, takes []ServedTake) []int {
	type parse struct {
		end int
		at  []int
	}
	ends := map[int]map[string]int{}
	partsAt := func(pos int) map[string]int {
		if got, ok := ends[pos]; ok {
			return got
		}
		got := map[string]int{}
		h := sha256.New()
		for from, e := pos, pos; e <= len(joined); e++ {
			if e < len(joined) && joined[e] != '\n' {
				continue
			}
			h.Write([]byte(joined[from:e]))
			if e-pos <= hermesNoteMaxBytes {
				got[hex.EncodeToString(h.Sum(nil))] = e
			}
			from = e
		}
		ends[pos] = got
		return got
	}
	type state struct{ pos, from int }
	memo := map[state]parse{}
	var best func(pos, from int) parse
	best = func(pos, from int) parse {
		if p, ok := memo[state{pos, from}]; ok {
			return p
		}
		out := parse{end: pos}
		for at := from; at < len(takes) && out.end < len(joined); at++ {
			e, ok := partsAt(pos)[takes[at].Digest]
			if !ok {
				continue
			}
			rest := parse{end: e}
			if e < len(joined) {
				rest = best(e+1, at+1)
			}
			if rest.end > out.end {
				out = parse{end: rest.end, at: append([]int{at}, rest.at...)}
			}
		}
		memo[state{pos, from}] = out
		return out
	}
	return best(0, 0).at
}
