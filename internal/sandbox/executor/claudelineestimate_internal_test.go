package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// leUser is a user record whose message's content is content, raw, after its
// own keys those beside it (each ending in a comma).
func leUser(content, beside string) string {
	return `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":` + content +
		`},"uuid":"` + ctUUID(1) + `","timestamp":"` + ctStamp + `",` + beside + ctTail("2.1.322") + `}`
}

// leAttachment is an attachment record holding attachment, raw, its record
// carrying beside (each key ending in a comma).
func leAttachment(attachment, beside string) string {
	return ctAttachment(ctUUID(1), ctUUID(2), attachment, beside)
}

// leMessageCase is a user or assistant record and what Claude Code estimates
// it at, worked by hand from the harness's rules.
type leMessageCase struct {
	name   string
	record string
	want   threadEstimate
}

// leMessageCases is every rule of a message's estimate, each worked by hand:
// a length L at three and four characters a token is Math.round(L/3) and
// Math.round(L/4), rounded per element.
var leMessageCases = []leMessageCase{
	{"a string content of 5 units", leUser(`"abcde"`, ""), threadEstimate{2, 1}},
	{"two text blocks of one unit each, each rounded to none",
		ctAssistant("", ctUUID(2), `[{"type":"text","text":"a"},{"type":"text","text":"a"}]`), threadEstimate{0, 0}},
	{"a text block of 8 units", ctAssistant("", ctUUID(2), `[{"type":"text","text":"abcdefgh"}]`),
		threadEstimate{3, 2}},
	{"thinking at its text, the signature not charged",
		ctAssistant("", ctUUID(2), `[{"type":"thinking","thinking":"abcdef","signature":"sigsigsigsigsig"}]`),
		threadEstimate{2, 2}},
	{"redacted thinking at its data",
		ctAssistant("", ctUUID(2), `[{"type":"redacted_thinking","data":"abcdefghijk"}]`), threadEstimate{4, 3}},
	{"a call at its name and its input as written (4 + 16)",
		ctAssistant("", ctUUID(2), `[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`),
		threadEstimate{7, 5}},
	{"a call whose input is null at {} (4 + 2)",
		ctAssistant("", ctUUID(2), `[{"type":"tool_use","id":"toolu_1","name":"Read","input":null}]`),
		threadEstimate{2, 2}},
	{"a result at its string", leUser(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"hello world"}]`, ""),
		threadEstimate{4, 3}},
	{"a result's strings each rounded to none",
		leUser(`[{"type":"tool_result","tool_use_id":"toolu_1","content":["a","a"]}]`, ""), threadEstimate{0, 0}},
	{"a result's text and image",
		leUser(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"abcd"},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]`, ""),
		threadEstimate{2001, 2001}},
	{"a result inside a result, by the rules again",
		leUser(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_result","content":"abcdef"}]}]`,
			""), threadEstimate{2, 2}},
	{"a text document titled .JSONL at two characters a token",
		leUser(`[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"abcdefgh"},`+
			`"title":"notes.JSONL"}]`, ""), threadEstimate{4, 4}},
	{"a text document titled json, its own last part",
		leUser(`[{"type":"document","source":{"type":"text","data":"abcdefgh"},"title":"json"}]`, ""),
		threadEstimate{4, 4}},
	{"a text document titled .txt at the model's rate",
		leUser(`[{"type":"document","source":{"type":"text","data":"abcdefgh"},"title":"notes.txt"}]`, ""),
		threadEstimate{3, 2}},
	{"a text document whose title's last part is not json",
		leUser(`[{"type":"document","source":{"type":"text","data":"abcdefgh"},"title":"a.json.txt"}]`, ""),
		threadEstimate{3, 2}},
	{"a text document with no title",
		leUser(`[{"type":"document","source":{"type":"text","data":"abcdefgh"}}]`, ""), threadEstimate{3, 2}},
	{"a PDF document flat",
		leUser(`[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}]`, ""),
		threadEstimate{2000, 2000}},
	{"a content document's string",
		leUser(`[{"type":"document","source":{"type":"content","content":"abcdef"}}]`, ""), threadEstimate{2, 2}},
	{"a content document's list, its objects alone",
		leUser(`[{"type":"document","source":{"type":"content","content":["zzzzzzzzzzzz",`+
			`{"type":"text","text":"abcdef"}]}}]`, ""), threadEstimate{2, 2}},
	{"a content document of neither, flat",
		leUser(`[{"type":"document","source":{"type":"content","content":7}}]`, ""), threadEstimate{2000, 2000}},
	{"an image flat", leUser(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]`,
		""), threadEstimate{2000, 2000}},
	{"a block of another type at its JSON as written (75 units)",
		ctAssistant("", ctUUID(2), `[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}]`),
		threadEstimate{25, 19}},
	{"a malformed call inside a block of another type, charged at the block's JSON (59 units)",
		leUser(`[{"type":"mystery","content":[{"type":"tool_use","name":7}]}]`, ""), threadEstimate{20, 15}},
	{"a task delivery's image beside the content",
		leUser(`"abc"`, `"taskDelivery":{"result":{"content":[{"type":"image"},{"type":"text","text":"x"}]}},`),
		threadEstimate{2001, 2001}},
	{"a task delivery's snapshot's images in place of its own result's",
		leUser(`"abc"`, `"taskDelivery":{"mcpTaskSnapshot":{"result":{"content":[{"type":"image"},{"type":"image"}]}},`+
			`"result":{"content":[{"type":"image"}]}},`), threadEstimate{4001, 4001}},
	{"an empty content charges no task delivery's image",
		leUser(`""`, `"taskDelivery":{"result":{"content":[{"type":"image"}]}},`), threadEstimate{0, 0}},
	{"a key written twice by its last value",
		ctAssistant("", ctUUID(2), `[{"type":"text","text":"abcdefghijklmnopqrstuvwxyz","text":"ab"}]`),
		threadEstimate{1, 1}},
	{"a record of another type charges nothing",
		`{"parentUuid":null,"type":"system","subtype":"informational","content":"abcdefgh","uuid":"` + ctUUID(3) +
			`","timestamp":"` + ctStamp + `"}`, threadEstimate{}},
}

// A THREAD LINE ESTIMATES ITS CONTENT AS CLAUDE CODE DOES (2.1.322's EX and
// n7e, its k7o of a record): per element, Math.round(UTF-16 length / n) at
// three and at four characters a token, each element rounded before the sum;
// a text, a thinking block's text, a redacted one's data, a call's name and
// its input as written, a result's content by the rules again, a JSON-titled
// text document at two, an image and a document of no text 2,000 flat, any
// other element at its JSON; on a user or assistant record, plus 2,000 an
// image its task delivery carries. And where the harness's own estimate
// throws (a text document whose title is there and no string), the estimate
// answers an error rather than a figure. Mutation: sum a list's strings and
// round once, and a result of two one-unit strings charges a token.
func TestAThreadLineEstimatesItsContentAsClaudeCodeDoes(t *testing.T) {
	t.Parallel()
	for _, tc := range leMessageCases {
		got, err := leLine(t, tc.record).Estimate()
		if err != nil || got != tc.want {
			t.Errorf("%s: estimated at %+v (%v), want %+v", tc.name, got, err, tc.want)
		}
	}
	thrown := leLine(t, leUser(`[{"type":"document","source":{"type":"text","data":"abcdefgh"},"title":5}]`, ""))
	if got, err := thrown.Estimate(); err == nil {
		t.Errorf("a text document titled with a number is estimated at %+v, want the error the harness throws", got)
	}
}

// AN ESTIMATE COUNTS UTF-16 UNITS PAST THE SKILL NAME CAP: a text's length is
// JavaScript's, a character past the basic plane two units and every other
// one, written as itself or escaped, a lone surrogate's escape one, at any
// length, never stopped at the 512 units a skill's name is held to.
//
// Mutation: cap the count at one past a skill name's bound, and a text of
// 1,800 units is estimated as one of 513.
func TestAnEstimateCountsUTF16UnitsPastTheSkillNameCap(t *testing.T) {
	t.Parallel()
	want := threadEstimate{600, 450}
	for name, content := range map[string]string{
		"written as itself": `"` + strings.Repeat("€", 600) + strings.Repeat("😀", 600) + `"`,
		"escaped":           `"` + strings.Repeat(`\u20ac`, 600) + strings.Repeat(`\ud83d\ude00`, 600) + `"`,
	} {
		if got, err := leLine(t, leUser(content, "")).Estimate(); err != nil || got != want {
			t.Errorf("a text of 600 euro signs and 600 emoji %s is estimated at %+v (%v), want %+v (1,800 units)",
				name, got, err, want)
		}
	}
	for raw, units := range map[string]int64{
		`""`: 0, `"a"`: 1, `"\n"`: 1, `"\""`: 1, `"\ud800"`: 1, `"\ud83d\ude00"`: 2, `"😀"`: 2, `"é"`: 1,
		`"` + strings.Repeat("x", skillListingMaxNameUnits+100) + `"`: skillListingMaxNameUnits + 100,
	} {
		if got := quotedUnits([]byte(raw)); got != units {
			t.Errorf("%.40s is %d UTF-16 units long, want %d", raw, got, units)
		}
	}
}

// leAttachmentCase is an attachment record and what Claude Code estimates it
// at, and whether that reads.
type leAttachmentCase struct {
	name   string
	record string
	want   threadEstimate
	read   bool
}

// leRendered is a stored rendering of the eight units `abcdefgh`.
const leRendered = `"rendered":[{"content":"abcdefgh"}],"renderedRole":"system",`

// lePDFRead is a PDF read's one entry of pages pages, raw.
func lePDFRead(pages string) string {
	return `{"path":"/box/a.pdf","pages":` + pages + `,"result":{"type":"text","text":"x"}}`
}

// leAttachmentCases is every rule of an attachment's estimate.
var leAttachmentCases = []leAttachmentCase{
	{"a stored rendering at its content", leAttachment(`{"type":"hook_success","content":"`+
		strings.Repeat("y", 300)+`"}`, leRendered), threadEstimate{3, 2}, true},
	{"a rendering of two messages, each text block on its own", leAttachment(`{"type":"hook_success"}`,
		`"rendered":[{"content":"abcdef"},{"content":[{"type":"text","text":"abc"},{"type":"text","text":"abcde"}],`+
			`"extra":1}],`), threadEstimate{5, 4}, true},
	{"no rendering stored: rendered again by type", leAttachment(`{"type":"file","filename":"a.go"}`, ""),
		threadEstimate{}, false},
	{"an empty rendered content does not read", leAttachment(`{"type":"hook_success"}`,
		`"rendered":[{"content":""}],`), threadEstimate{}, false},
	{"a rendering of no message does not read", leAttachment(`{"type":"hook_success"}`, `"rendered":[],`),
		threadEstimate{}, false},
	{"a rendering holding an image does not read", leAttachment(`{"type":"hook_success"}`,
		`"rendered":[{"content":[{"type":"image"}]}],`), threadEstimate{}, false},
	{"a rendering of an empty list does not read", leAttachment(`{"type":"hook_success"}`,
		`"rendered":[{"content":[]}],`), threadEstimate{}, false},
	{"a type rendered as no message, storing none, is nothing", leAttachment(`{"type":"prompt_snapshot",`+
		`"systemPrompt":"`+strings.Repeat("s", 4000)+`"}`, ""), threadEstimate{}, true},
	{"a PDF read at 3,000 a page", leAttachment(`{"type":"message_pdf_reads","reads":[`+lePDFRead("2")+`,`+
		lePDFRead("1")+`]}`, leRendered), threadEstimate{9000, 9000}, true},
	{"a PDF read whose pages are no count does not read", leAttachment(`{"type":"message_pdf_reads","reads":[`+
		lePDFRead(`"2"`)+`]}`, ""), threadEstimate{}, false},
	{"a PDF read of no pages does not read", leAttachment(`{"type":"message_pdf_reads","reads":[`+
		lePDFRead("0")+`]}`, ""), threadEstimate{}, false},
	{"a PDF read with no reads does not read", leAttachment(`{"type":"message_pdf_reads"}`, ""),
		threadEstimate{}, false},
	{"polled events are rendered again whatever they store", leAttachment(`{"type":"poll_events"}`, leRendered),
		threadEstimate{}, false},
	{"a project's document reads are rendered again whatever they store",
		leAttachment(`{"type":"project_doc_reads"}`, leRendered), threadEstimate{}, false},
	{"a queued command's rendering and its task delivery's image", leAttachment(`{"type":"queued_command",`+
		`"taskDelivery":{"result":{"content":[{"type":"image"}]}}}`, `"rendered":[{"content":"abcd"}],`),
		threadEstimate{2001, 2001}, true},
	{"an attachment of no type storing no rendering is nothing", leAttachment(`{"text":"abc"}`, ""),
		threadEstimate{}, true},
	{"a record of another type is nothing", leUser(`"abcdefgh"`, ""), threadEstimate{}, true},
}

// AN ATTACHMENT IS ESTIMATED BY ITS RENDERING (2.1.322's k7o, rTe, qLe and
// Cje): by what its record stores as `rendered`, each message's content at
// its length, each text block on its own, never by the attachment's own
// bytes; a PDF read at 3,000 a page; a queued command's task delivery's images
// beside its rendering; nothing for an attachment of no type, or of a type the
// harness renders as no message, that stores none; and not read (false) where
// the harness renders it again by its type: no rendering stored, one of
// another shape, a PDF read whose pages do not read, and the two types it
// renders again whatever they store. An attachment that is no object answers
// its type's error. Mutation: take the stored rendering's own JSON as its
// length, and the rendering of `abcdefgh` charges its quotes and keys.
func TestAnAttachmentIsEstimatedByItsRendering(t *testing.T) {
	t.Parallel()
	for _, tc := range leAttachmentCases {
		got, read, err := leLine(t, tc.record).AttachmentEstimate()
		if err != nil || got != tc.want || read != tc.read {
			t.Errorf("%s: estimated at %+v, read %t (%v); want %+v, read %t", tc.name, got, read, err, tc.want,
				tc.read)
		}
	}
	if got, read, err := leLine(t, leAttachment(`null`, "")).AttachmentEstimate(); err == nil {
		t.Errorf("an attachment that is null is estimated at %+v, read %t, want its type's error", got, read)
	}
}

// leProducers is every producer of a line's facts, each by how a reader reads
// a line through it.
var leProducers = []struct {
	name string
	read func(raw []byte) (threadLine, error)
}{
	{"the whole line's decode", func(raw []byte) (threadLine, error) { return decodeThreadLine(1, raw, nil) }},
	{"the line's scanner pass", leScan},
}

// EVERY PRODUCER FILLS THE ESTIMATE: what each producer of a line's facts
// answers of a line's estimate ([threadLine.Estimate],
// [threadLine.AttachmentEstimate]) is what the harness's rules read of the
// line decoded whole (leWholeEstimate: every value decoded at once, every
// text's length counted on its decoded string, rounded by math.Round at each
// rate), on every line of every Claude Code thread under testdata, the
// layer's own records and the hard cases, and every case above: so a
// producer, the whole-line decode or another, never leaves a record's
// estimate unfilled, nor fills it otherwise than the rules read it.
//
// Mutation: leave an assistant record's estimate at zero, and every reply of
// the testdata threads charges nothing.
func TestEveryProducerFillsTheEstimate(t *testing.T) {
	t.Parallel()
	lines := byteFactLines(t)
	for _, tc := range leMessageCases {
		lines = append(lines, []byte(tc.record))
	}
	for _, tc := range leAttachmentCases {
		lines = append(lines, []byte(tc.record))
	}
	for _, p := range leProducers {
		var messages, attachments int
		for _, raw := range lines {
			l, err := p.read(raw)
			if err != nil {
				continue
			}
			wantMessage, wantAttachment, wantRead, wantErr := leWholeEstimate(t, raw)
			got, err := l.Estimate()
			if _, messageErr := l.Message(); messageErr != nil {
				if err == nil {
					t.Errorf("%s: %.80s: its message does not read (%v), yet it is estimated at %+v", p.name, raw,
						messageErr, got)
				}
				continue
			}
			if (err != nil) != (wantErr != nil) || got != wantMessage {
				t.Errorf("%s: %.80s: estimated at %+v (%v), whole %+v (%v)", p.name, raw, got, err, wantMessage,
					wantErr)
			}
			if got.At3 > 0 {
				messages++
			}
			if _, typeErr := l.AttachmentType(); typeErr != nil {
				continue
			}
			gotAttachment, read, err := l.AttachmentEstimate()
			if err != nil || gotAttachment != wantAttachment || read != wantRead {
				t.Errorf("%s: %.80s: its attachment estimated at %+v, read %t (%v), whole %+v, read %t", p.name, raw,
					gotAttachment, read, err, wantAttachment, wantRead)
			}
			if read && gotAttachment.At3 > 0 {
				attachments++
			}
		}
		// The claim is read on records that charge something, not on a
		// corpus that charges nothing.
		if messages < 100 || attachments < 100 {
			t.Errorf("%s: %d messages and %d attachments charged anything; the claim reads at least 100 of each",
				p.name, messages, attachments)
		}
	}
}

// leWholeEstimate is what the harness's rules read of raw, a line decoded
// whole: the record's message's estimate and the error it throws, and its
// attachment's estimate and whether that reads.
func leWholeEstimate(t *testing.T, raw []byte) (message, attachment threadEstimate, read bool, err error) {
	t.Helper()
	// A line or a message that does not decode charges nothing here: its
	// reader skips it on the producer's own error.
	var record map[string]json.RawMessage
	if !leDecodes(raw, &record) {
		return threadEstimate{}, threadEstimate{}, true, nil
	}
	switch leString(record["type"]) {
	case recordUser, recordAssistant:
		var m map[string]json.RawMessage
		if !leDecodes(record["message"], &m) {
			return threadEstimate{}, threadEstimate{}, true, nil
		}
		content := m["content"]
		images := int64(0)
		if string(content) != `""` {
			images = leImages(record["taskDelivery"])
		}
		for _, n := range []float64{3, 4} {
			var c int64
			if c, err = leContent(content, n); err != nil {
				return threadEstimate{}, threadEstimate{}, true, err
			}
			message = leAt(message, n, c+estimateMediaTokens*images)
		}
		return message, threadEstimate{}, true, nil
	case recordAttachment:
		attachment, read = leWholeAttachment(record)
		return threadEstimate{}, attachment, read, nil
	}
	return threadEstimate{}, threadEstimate{}, true, nil
}

// leWholeAttachment is an attachment record's estimate by the harness's rules,
// decoded whole.
func leWholeAttachment(record map[string]json.RawMessage) (threadEstimate, bool) {
	var a map[string]json.RawMessage
	if json.Unmarshal(record["attachment"], &a) != nil || a == nil {
		return threadEstimate{}, true
	}
	typ := leString(a["type"])
	switch typ {
	case "message_pdf_reads":
		var reads []map[string]json.RawMessage
		if json.Unmarshal(a["reads"], &reads) != nil || len(a["reads"]) == 0 || a["reads"][0] != '[' {
			return threadEstimate{}, false
		}
		var pages int64
		for _, r := range reads {
			var n float64
			p := r["pages"]
			if r == nil || len(p) == 0 || strings.ContainsAny(string(p), "-.eE\"") || json.Unmarshal(p, &n) != nil ||
				n < 1 {
				return threadEstimate{}, false
			}
			pages += int64(n)
		}
		return threadEstimate{pages * 3000, pages * 3000}, true
	case "poll_events", "project_doc_reads":
		return threadEstimate{}, false
	}
	var rendered []map[string]json.RawMessage
	shaped := json.Unmarshal(record["rendered"], &rendered) == nil && len(rendered) > 0
	var e threadEstimate
	for _, m := range rendered {
		content := m["content"]
		switch {
		case m == nil || len(content) == 0:
			shaped = false
		case content[0] == '"':
			shaped = shaped && leString(content) != ""
			for _, n := range []float64{3, 4} {
				e = leAt(e, n, leMC(content, n))
			}
		default:
			var blocks []map[string]json.RawMessage
			shaped = shaped && json.Unmarshal(content, &blocks) == nil && len(blocks) > 0
			for _, b := range blocks {
				text := b["text"]
				shaped = shaped && b != nil && leString(b["type"]) == "text" && len(text) > 0 && text[0] == '"' &&
					leString(text) != ""
				for _, n := range []float64{3, 4} {
					e = leAt(e, n, leMC(text, n))
				}
			}
		}
	}
	if shaped {
		if typ == "queued_command" {
			images := leImages(a["taskDelivery"])
			e = threadEstimate{e.At3 + 2000*images, e.At4 + 2000*images}
		}
		return e, true
	}
	if a["type"] == nil || slices.Contains([]string{"prompt_snapshot", "prompt_render_point", "deferred_tools_record",
		"thinking_drop", "credential_org"}, typ) {
		return threadEstimate{}, true
	}
	return threadEstimate{}, false
}

// leDecodes reports whether raw decodes whole into v.
func leDecodes(raw []byte, v any) bool { return json.Unmarshal(raw, v) == nil }

// leAt is e with c added at the rate n.
func leAt(e threadEstimate, n float64, c int64) threadEstimate {
	if n == 3 {
		e.At3 += c
	} else {
		e.At4 += c
	}
	return e
}

// leString is raw decoded as a string, "" where it is none.
func leString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// leRound is Math.round(units / n).
func leRound(units int, n float64) int64 { return int64(math.Round(float64(units) / n)) }

// leMC is a value at the rate n where it is a string, nothing else (mc).
func leMC(raw json.RawMessage, n float64) int64 {
	if len(raw) == 0 || raw[0] != '"' {
		return 0
	}
	return leRound(utf16Units([]byte(leString(raw))), n)
}

// leContent is a content value at the rate n (EX), decoded whole.
func leContent(raw json.RawMessage, n float64) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	switch raw[0] {
	case '"':
		return leMC(raw, n), nil
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return 0, err
		}
		var sum int64
		for _, e := range list {
			c, err := leElement(e, n)
			if err != nil {
				return 0, err
			}
			sum += c
		}
		return sum, nil
	}
	return 0, nil
}

// leElement is one element at the rate n (n7e), decoded whole.
func leElement(raw json.RawMessage, n float64) (int64, error) {
	switch raw[0] {
	case '"':
		return leMC(raw, n), nil
	case '{':
	default:
		return 0, nil
	}
	var e map[string]json.RawMessage
	if err := json.Unmarshal(raw, &e); err != nil {
		return 0, err
	}
	typ := ""
	if t := e["type"]; len(t) > 0 && t[0] == '"' {
		typ = leString(t)
	}
	switch typ {
	case "text":
		return leMC(e["text"], n), nil
	case "document":
		if src := e["source"]; len(src) > 0 && src[0] == '{' {
			var s map[string]json.RawMessage
			if err := json.Unmarshal(src, &s); err != nil {
				return 0, err
			}
			switch leString(s["type"]) {
			case "text":
				title, ext := e["title"], ""
				switch {
				case len(title) == 0 || string(title) == "null":
				case title[0] == '"':
					parts := strings.Split(leString(title), ".")
					ext = strings.ToLower(parts[len(parts)-1])
				default:
					return 0, errors.New("the title has no split")
				}
				rate := 4.0
				if ext == "json" || ext == "jsonl" || ext == "jsonc" {
					rate = 2
				}
				return leMC(s["data"], math.Min(n, rate)), nil
			case "content":
				c := s["content"]
				if len(c) > 0 && c[0] == '"' {
					return leMC(c, n), nil
				}
				if len(c) > 0 && c[0] == '[' {
					var list []json.RawMessage
					if err := json.Unmarshal(c, &list); err != nil {
						return 0, err
					}
					var sum int64
					for _, el := range list {
						if el[0] != '{' && el[0] != '[' {
							continue
						}
						v, err := leElement(el, n)
						if err != nil {
							return 0, err
						}
						sum += v
					}
					return sum, nil
				}
			}
		}
		return 2000, nil
	case "image":
		return 2000, nil
	case "tool_result":
		return leContent(e["content"], n)
	case "tool_use":
		name := e["name"]
		if len(name) == 0 || name[0] != '"' {
			return 0, errors.New("the name is no string")
		}
		input := 2
		if in := e["input"]; len(in) > 0 && string(in) != "null" {
			input = utf16Units(in)
		}
		return leRound(utf16Units([]byte(leString(name)))+input, n), nil
	case "thinking":
		return leMC(e["thinking"], n), nil
	case "redacted_thinking":
		return leMC(e["data"], n), nil
	}
	return leRound(utf16Units(raw), n), nil
}

// leImages is how many images a task delivery carries (S), decoded whole.
func leImages(delivery json.RawMessage) int64 {
	var d map[string]json.RawMessage
	if len(delivery) == 0 || delivery[0] != '{' || json.Unmarshal(delivery, &d) != nil {
		return 0
	}
	if s := d["mcpTaskSnapshot"]; len(s) > 0 && s[0] == '{' {
		d = nil
		if json.Unmarshal(s, &d) != nil {
			return 0
		}
	}
	var result map[string]json.RawMessage
	if r := d["result"]; len(r) == 0 || r[0] != '{' || json.Unmarshal(r, &result) != nil {
		return 0
	}
	var content []json.RawMessage
	if c := result["content"]; len(c) == 0 || c[0] != '[' || json.Unmarshal(c, &content) != nil {
		return 0
	}
	var images int64
	for _, e := range content {
		var el map[string]json.RawMessage
		if e[0] == '{' && json.Unmarshal(e, &el) == nil && leString(el["type"]) == "image" {
			images++
		}
	}
	return images
}

// leScan is raw, one line, read in the line's one scanner pass by a reader
// that asks for its estimate ([threadReader.Estimating]).
func leScan(raw []byte) (threadLine, error) {
	r := newThreadReader(bytes.NewReader(append(slices.Clone(raw), '\n')), threadPlainMax)
	r.Estimating()
	return r.Next(context.Background())
}

// leLine is record read as the one line of a thread by a reader that asks
// for its estimate.
func leLine(t testing.TB, record string) threadLine {
	t.Helper()
	l, err := leScan([]byte(record))
	if err != nil {
		t.Fatalf("%.120s reads as %v; want one record", record, err)
	}
	return l
}

// leHardCase is a record at an edge of the estimate's reading and what both
// producers estimate it at: a message's figure, or where fault its error; an
// attachment's figure and whether it reads.
type leHardCase struct {
	name                    string
	record                  string
	want                    threadEstimate
	attachment, read, fault bool
}

// leBlocks is a user record whose message's content is the list of blocks.
func leBlocks(blocks ...string) string { return leUser(`[`+strings.Join(blocks, ",")+`]`, "") }

// leResult is a tool_result block of content, raw.
func leResult(content string) string {
	return `{"type":"tool_result","tool_use_id":"toolu_1","content":` + content + `}`
}

// leCall is a tool_use block of name and its input's member as written, raw:
// `"input":...`, or none.
func leCall(name, input string) string {
	if input != "" {
		input = "," + input
	}
	return `{"type":"tool_use","id":"toolu_1","name":"` + name + `"` + input + `}`
}

// leDocument is a text document of data, raw, beside its other members.
func leDocument(data, beside string) string {
	return `{"type":"document","source":{"type":"text","data":` + data + `}` + beside + `}`
}

// leDelivered is a user record of ten units of content beside delivery, raw,
// its task delivery.
func leDelivered(delivery string) string {
	return leUser(`"abcdefghij"`, `"taskDelivery":`+delivery+`,`)
}

// leReads is a PDF read of reads, raw.
func leReads(reads string) string {
	return leAttachment(`{"type":"message_pdf_reads","reads":`+reads+`}`, "")
}

// leRenderedFirst is an attachment record that writes beside before its
// attachment.
func leRenderedFirst(beside, attachment string) string {
	return `{"parentUuid":null,"isSidechain":false,` + beside + `"attachment":` + attachment +
		`,"type":"attachment","uuid":"` + ctUUID(2) + `","timestamp":"` + ctStamp + `",` + ctTail("2.1.310") + `}`
}

// leTen and leTwentyTwo are the figures of a text of 10 and of 22 units: a
// length 10 (mod 12), at which one unit more moves the rate of three and one
// fewer the rate of four, so a count off by one either way reads otherwise.
var (
	leTen        = threadEstimate{3, 3}
	leTwentyTwo  = threadEstimate{7, 6}
	leTenHalved  = threadEstimate{5, 5}
	leImageAnd10 = func(images int64) threadEstimate { return leTen.plus(estimateFlat(2000 * images)) }
)

// leInputs is a call's name and its input's member as written
// ([leCall]), of each kind an input is written as, and what the call charges.
var leInputs = []struct {
	kind, name, input string
	want              threadEstimate
}{
	{"an object with blanks before and inside it (12 + 10)", "abcdefghijkl", `"input":   { "k": 1 }`, leTwentyTwo},
	{"a string (2 + 8)", "ab", `"input":"abcdef"`, leTen},
	{"a list (1 + 9)", "a", `"input":[1,2,3,4]`, leTen},
	{"a number (2 + 8)", "ab", `"input":12345678`, leTen},
	{"true (6 + 4)", "abcdef", `"input":true`, leTen},
	{"false (5 + 5)", "abcde", `"input":false`, leTen},
	{"null, as {} (8 + 2)", "abcdefgh", `"input":null`, leTen},
	{"none, as {} (8 + 2)", "abcdefgh", "", leTen},
}

// leHardCases is every hard case, each charging a text of 10 units (22 where
// a name and an input must both be there) unless it says otherwise.
func leHardCases() []leHardCase {
	pdf := func(pages int64) threadEstimate { return estimateFlat(pages * 3000) }
	var cases []leHardCase
	// A call as a block, nested as a result's first element (the digest's)
	// and as an element of a content document's source, its input each kind.
	for _, in := range leInputs {
		call := leCall(in.name, in.input)
		for _, at := range []struct{ where, record string }{
			{"a block", leBlocks(call)},
			{"a result's element", leBlocks(leResult(`[` + call + `]`))},
			{"a source's element", leBlocks(`{"type":"document","source":{"type":"content","content":[` + call + `]}}`)},
		} {
			cases = append(cases, leHardCase{name: "a call as " + at.where + ", its input " + in.kind,
				record: at.record, want: in.want})
		}
	}
	return append(cases, []leHardCase{
		{name: "a block's type last", record: leBlocks(`{"text":"abcdefghij","type":"text"}`), want: leTen},
		{name: "a nested type escaped, past the digest", record: leBlocks(leResult(
			`[{"type":"tool_result","content":[{"type":"\u0074ext","text":"abcdefghij"}]}]`)), want: leTen},
		{name: "a second element's type escaped", record: leBlocks(leResult(
			`["",{"type":"\u0074ext","text":"abcdefghij"}]`)), want: leTen},
		{name: "a result's string element", record: leBlocks(leResult(`["abcdefghij"]`)), want: leTen},
		{name: "a result's string element after a one-unit text, rounded to none", record: leBlocks(leResult(
			`[{"type":"text","text":"x"},"abcdefghij"]`)), want: leTen},
		{name: "an input escaping a character beside one written (1 + 9)", record: leBlocks(leCall("a",
			`"input":"\u00e9é"`)), want: leTen},
		{name: "an input holding an invalid byte (7 + 3)", record: leBlocks(leCall("abcdefg", "\"input\":\"\xff\"")),
			want: leTen},
		{name: "an input holding a lone surrogate (2 + 8)", record: leBlocks(leCall("ab", `"input":"\ud800"`)),
			want: leTen},
		{name: "an input written twice, its last (2 + 8)", record: leBlocks(leCall("ab",
			`"input":[1],"input":12345678`)), want: leTen},
		{name: "a text that is a number", record: leBlocks(`{"type":"text","text":12345}`)},
		{name: "a text that is a literal", record: leBlocks(`{"type":"text","text":true}`)},
		{name: "a text that is an object", record: leBlocks(`{"type":"text","text":{"abcdefghij":"x"}}`)},
		{name: "a thinking that is a number", record: leBlocks(`{"type":"thinking","thinking":12345}`)},
		{name: "a data that is an object", record: leBlocks(`{"type":"redacted_thinking","data":{"abc":1}}`)},
		{name: "a source's data that is a literal", record: leBlocks(leDocument("false", ""))},
		{name: "a nested call whose name is a number", record: leBlocks(leResult(
			`[{"type":"tool_use","name":12345}]`)), fault: true},
		{name: "a nested type turning after its content", record: leBlocks(leResult(
			`[{"type":"text","content":"abcdefghij","type":"tool_result"}]`)), want: leTen},
		{name: "a one-unit content, rounded to none, beside an image",
			record: leUser(`"a"`, `"taskDelivery":{"result":{"content":[{"type":"image"}]}},`), want: estimateFlat(2000)},
		{name: "a content's first line naming the handback, its rest past a piece unread by any automaton",
			record: leUser(`"`+claudeToolHandback+`\n`+strings.Repeat("x", 12010-len(claudeToolHandback)-1)+`"`,
				""), want: estimateUnits(12010)},
		{name: "a text block past a piece, saying something in its first", record: leBlocks(
			`{"type":"text","text":"a` + strings.Repeat("x", 12009) + `"}`), want: threadEstimate{4003, 3003}},
		// 1 + 3,003 × 3 = 9,010 units; its 4,096th byte decoded is an emoji's first.
		{name: "a text block past a piece, past ASCII", record: leBlocks(
			`{"type":"text","text":"a` + strings.Repeat("é😀", 3003) + `"}`), want: threadEstimate{3003, 2253}},
		{name: "a text block past a piece, past ASCII, escaped", record: leBlocks(
			`{"type":"text","text":"a` + strings.Repeat(`\u00e9\ud83d\ude00`, 3003) + `"}`),
			want: threadEstimate{3003, 2253}},
		{name: "a source's data before its type", record: leBlocks(
			`{"type":"document","source":{"data":"abcdefghij","type":"text"}}`), want: leTen},
		{name: "a type turning after its source", record: leBlocks(
			`{"type":"image","source":{"type":"text","data":"abcdefghij"},"type":"document"}`), want: leTen},
		{name: "a type turning from a document to an image", record: leBlocks(
			`{"type":"document","source":{"type":"text","data":"abcdefghij"},"type":"image"}`),
			want: estimateFlat(2000)},
		{name: "a title of an escaped JSON", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":"notes.\u004aSON"`)),
			want: leTenHalved},
		{name: "a title of a.JSON", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":"a.JSON"`)),
			want: leTenHalved},
		{name: "a title of a.JSON, its dot and J escaped", record: leBlocks(leDocument(`"abcdefghij"`,
			`,"title":"a\u002e\u004aSON"`)), want: leTenHalved},
		{name: "a title past ASCII", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":"x.jsön"`)), want: leTen},
		{name: "a title past ASCII, escaped", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":"x.js\u00f6n"`)),
			want: leTen},
		{name: "a title of a lone surrogate's escape", record: leBlocks(leDocument(`"abcdefghij"`,
			`,"title":"x.\ud800json"`)), want: leTen},
		{name: "a title of an invalid byte", record: leBlocks(leDocument(`"abcdefghij"`, ",\"title\":\"x.\xffjson\"")),
			want: leTen},
		{name: "a title ending in a dot", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":"a.json."`)),
			want: leTen},
		{name: "a title of six letters", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":"jsonlx"`)),
			want: leTen},
		{name: "a title past 64 KiB ending .json", record: leBlocks(leDocument(`"abcdefghij"`,
			`,"title":"`+strings.Repeat("x", 70000)+`.json"`)), want: leTenHalved},
		{name: "a title that is a number", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":5`)), fault: true},
		{name: "a title of null", record: leBlocks(leDocument(`"abcdefghij"`, `,"title":null`)), want: leTen},
		{name: "a title written twice, its last not JSON", record: leBlocks(leDocument(`"abcdefghij"`,
			`,"title":"a.json","title":"a.txt"`)), want: leTen},
		{name: "a title written twice, its last JSON", record: leBlocks(leDocument(`"abcdefghij"`,
			`,"title":"a.txt","title":"a.jsonc"`)), want: leTenHalved},
		{name: "a thinking written twice", record: leBlocks(
			`{"type":"thinking","thinking":"abc","thinking":"abcdefghij"}`), want: leTen},
		{name: "a data written twice", record: leBlocks(
			`{"type":"redacted_thinking","data":"x","data":"abcdefghij"}`), want: leTen},
		{name: "a source written twice", record: leBlocks(
			`{"type":"document","source":{"type":"text","data":"x"},"source":{"type":"text","data":"abcdefghij"}}`),
			want: leTen},
		{name: "a result's content written twice", record: leBlocks(leResult(`"x","content":"abcdefghij"`)),
			want: leTen},
		{name: "the content an empty string, then a list beside an image",
			record: leUser(`"","content":[{"type":"text","text":"abcdefghij"}]`,
				`"taskDelivery":{"result":{"content":[{"type":"image"}]}},`), want: leImageAnd10(1)},
		{name: "the content a list beside an image, then an empty string",
			record: leUser(`[{"type":"text","text":"abcdefghij"}],"content":""`,
				`"taskDelivery":{"result":{"content":[{"type":"image"}]}},`)},
		{name: "the content a faulting list, then a string", record: leUser(`[`+leResult(
			`[{"type":"tool_use","name":7}]`)+`],"content":"abcdefghij"`, ""), want: leTen},
		{name: "a message that is a string beside an image", record: `{"parentUuid":null,"isSidechain":false,"type":"user","message":"abcdefghij",` +
			`"taskDelivery":{"result":{"content":[{"type":"image"}]}},"uuid":"` + ctUUID(1) + `","timestamp":"` +
			ctStamp + `",` + ctTail("2.1.322") + `}`, fault: true},
		{name: "two faults, the first's words", record: leBlocks(leResult(`[{"type":"tool_use","name":7}]`),
			leDocument(`"x"`, `,"title":5`)), fault: true},
		{name: "a delivery's result twice, the last no list", record: leDelivered(
			`{"result":{"content":[{"type":"image"}]},"result":{"content":5}}`), want: leTen},
		{name: "a delivery's result's content twice, the last no list", record: leDelivered(
			`{"result":{"content":[{"type":"image"}],"content":5}}`), want: leTen},
		{name: "a snapshot object, then null: its own result", record: leDelivered(
			`{"mcpTaskSnapshot":{"result":{"content":[{"type":"image"}]}},"mcpTaskSnapshot":null,` +
				`"result":{"content":[{"type":"image"},{"type":"image"}]}}`), want: leImageAnd10(2)},
		{name: "a snapshot object with no result", record: leDelivered(
			`{"mcpTaskSnapshot":{},"result":{"content":[{"type":"image"}]}}`), want: leTen},
		{name: "a result before its snapshot", record: leDelivered(
			`{"result":{"content":[{"type":"image"}]},"mcpTaskSnapshot":{"result":{"content":[{"type":"image"},` +
				`{"type":"image"}]}}}`), want: leImageAnd10(2)},
		{name: "a snapshot twice, its last", record: leDelivered(
			`{"mcpTaskSnapshot":{"result":{"content":[{"type":"image"}]}},"mcpTaskSnapshot":{"result":{"content":` +
				`[{"type":"image"},{"type":"image"},{"type":"image"}]}}}`), want: leImageAnd10(3)},
		{name: "a snapshot twice, its last of no result", record: leDelivered(
			`{"mcpTaskSnapshot":{"result":{"content":[{"type":"image"}]}},"mcpTaskSnapshot":{}}`), want: leTen},
		{name: "a snapshot that is no object", record: leDelivered(
			`{"mcpTaskSnapshot":"x","result":{"content":[{"type":"image"}]}}`), want: leImageAnd10(1)},
		{name: "a snapshot's result that is no object", record: leDelivered(
			`{"mcpTaskSnapshot":{"result":[{"type":"image"}]},"result":{"content":[{"type":"image"}]}}`), want: leTen},
		{name: "a delivery that is no object", record: leDelivered(`[{"result":{"content":[{"type":"image"}]}}]`),
			want: leTen},
		{name: "an element's type image, then text", record: leDelivered(
			`{"result":{"content":[{"type":"image","type":"text"}]}}`), want: leTen},
		{name: "an element's type image, escaped", record: leDelivered(
			`{"result":{"content":[{"type":"\u0069mage"}]}}`), want: leImageAnd10(1)},
		{name: "a rendering twice, its last", attachment: true, read: true, want: leTen,
			record: leAttachment(`{"type":"hook_success"}`, `"rendered":[{"content":"x"}],"rendered":[{"content":`+
				`"abcdefghij"}],`)},
		{name: "a rendering before its attachment", attachment: true, read: true, want: leTen,
			record: leRenderedFirst(`"rendered":[{"content":"abcdefghij"}],`, `{"type":"hook_success"}`)},
		{name: "a rendered message's content twice", attachment: true, read: true, want: leTen,
			record: leAttachment(`{"type":"hook_success"}`, `"rendered":[{"content":[],"content":"abcdefghij"}],`)},
		{name: "a rendered content that is a number", attachment: true,
			record: leAttachment(`{"type":"hook_success"}`, `"rendered":[{"content":12345}],`)},
		{name: "a rendered text that is an object", attachment: true,
			record: leAttachment(`{"type":"hook_success"}`, `"rendered":[{"content":[{"type":"text","text":{}}]}],`)},
		{name: "a rendered block's type escaped", attachment: true, read: true, want: leTen,
			record: leAttachment(`{"type":"hook_success"}`,
				`"rendered":[{"content":[{"type":"\u0074ext","text":"abcdefghij"}]}],`)},
		{name: "pages at 2^53 - 1", attachment: true, read: true, want: pdf(threadEstimateMax / 3000),
			record: leReads(`[{"pages":9007199254740991}]`)},
		{name: "pages at 2^53", attachment: true, record: leReads(`[{"pages":9007199254740992}]`)},
		{name: "pages of 24 digits", attachment: true, record: leReads(`[{"pages":123456789012345678901234}]`)},
		{name: "pages twice, the last no count", attachment: true,
			record: leReads(`[{"pages":3,"pages":9007199254740992}]`)},
		{name: "pages twice, the last a count", attachment: true, read: true, want: pdf(3),
			record: leReads(`[{"pages":9007199254740992,"pages":3}]`)},
		{name: "two reads past the cap", attachment: true, read: true, want: pdf(threadEstimateMax / 3000),
			record: leReads(`[{"pages":9007199254740991},{"pages":9007199254740991}]`)},
		{name: "no reads", attachment: true, read: true, record: leReads(`[]`)},
		{name: "a read that is no object", attachment: true, record: leReads(`[5]`)},
		{name: "a read's pages that are no count", attachment: true, record: leReads(`[{"pages":1.0}]`)},
		{name: "a queued command's delivery and rendering", attachment: true, read: true, want: leImageAnd10(2),
			record: leAttachment(`{"type":"queued_command","taskDelivery":{"result":{"content":[{"type":"image"},`+
				`{"type":"image"}]}}}`, `"rendered":[{"content":"abcdefghij"}],`)},
	}...)
}

// THE ESTIMATE READS EVERY KEY BY ITS DECODED NAME AND LAST OCCURRENCE, IN
// ANY ORDER, as the harness's JSON.parse reads it, and every length as the
// harness counts it, in both producers alike: a type written after the keys
// it reads, escaped or turning; a call's input as written, its blanks and
// escapes counted, each of its kinds, as a block and nested; a text block
// past a piece; a result's string element; a text a rule reads that is no
// string none; a title decoded before it is split; each key written twice, a
// delivery's snapshot and result in either order; a rendering and a PDF
// read's pages at their edges. Each figure is 10 (mod 12) units ([leTen]).
//
// Mutation: start a nested input without what Next took of it, set it where
// null, or stop a text block at its first piece, and this fails naming it.
func TestTheEstimateReadsTheHardCasesAsTheHarness(t *testing.T) {
	t.Parallel()
	for _, tc := range leHardCases() {
		for _, p := range leProducers {
			l, err := p.read([]byte(tc.record))
			if err != nil {
				t.Errorf("%s: %s reads it as %v", tc.name, p.name, err)
				continue
			}
			if tc.attachment {
				got, read, err := l.AttachmentEstimate()
				if err != nil || got != tc.want || read != tc.read {
					t.Errorf("%s: %s estimates it at %+v, read %t (%v); want %+v, read %t", tc.name, p.name, got,
						read, err, tc.want, tc.read)
				}
				continue
			}
			if got, err := l.Estimate(); (err != nil) != tc.fault || (!tc.fault && got != tc.want) {
				t.Errorf("%s: %s estimates it at %+v (%v); want %+v, a fault %t", tc.name, p.name, got, err,
					tc.want, tc.fault)
			}
		}
	}
}

// leChain is a message's content of one block whose element lists nest an
// object at level levels down, the text of 10 units, each level a result's
// content or a content document's source's, as link says: "content",
// "source", or "both", alternating from a result.
func leChain(levels int, link string) string {
	element := `{"type":"text","text":"abcdefghij"}`
	for level := levels - 1; level >= 1; level-- {
		source := link == "source" || link == "both" && level%2 == 0
		switch {
		case source:
			element = `{"type":"document","source":{"type":"content","content":[` + element + `]}}`
		case level == 1:
			element = leResult(`[` + element + `]`)
		default:
			element = `{"type":"tool_result","content":[` + element + `]}`
		}
	}
	return `[` + element + `]`
}

// AN ESTIMATE NESTED PAST ITS BOUND DOES NOT READ: an object a rule charges
// at [estimateNestMax] element lists, through results' contents, through
// documents' sources or alternating, is charged as the unbounded walk charges
// it; one list deeper, the estimate is the nesting fault, no figure, in both
// producers, where the unbounded walk charges it; inside a block that
// charges its JSON or its text the nesting is no fault and both read alike;
// and a chain nested as deep as a line is read is read to its end, its
// estimate the fault.
//
// Mutation: bound the walk one level later or earlier, raise no level
// through a source's content, or charge a past-bound object nothing, and
// this fails naming the chain.
func TestAnEstimateNestedPastItsBoundDoesNotRead(t *testing.T) {
	t.Parallel()
	for _, link := range []string{"content", "source", "both"} {
		for _, levels := range []int{estimateNestMax, estimateNestMax + 1} {
			content := leChain(levels, link)
			unbounded, err := estimateContent(json.RawMessage(content), 0)
			if err != nil || unbounded.est != leTen || unbounded.fault.what != "" {
				t.Fatalf("%s to %d levels: the unbounded walk charges %+v (%v), want %+v", link, levels, unbounded,
					err, leTen)
			}
			for _, p := range leProducers {
				l, err := p.read([]byte(leUser(content, "")))
				if err != nil {
					t.Fatalf("%s to %d levels: %s reads it as %v", link, levels, p.name, err)
				}
				got, err := l.Estimate()
				switch {
				case levels <= estimateNestMax && (err != nil || got != unbounded.est):
					t.Errorf("%s to %d levels: %s estimates %+v (%v), want %+v as unbounded", link, levels, p.name,
						got, err, unbounded.est)
				case levels > estimateNestMax && (err == nil || !strings.Contains(err.Error(), "nested past")):
					t.Errorf("%s to %d levels: %s estimates %+v (%v), want the nesting fault", link, levels, p.name,
						got, err)
				}
			}
		}
		inner := leChain(estimateNestMax+3, link)
		for _, block := range []string{`{"type":"mystery","content":` + inner + `}`,
			`{"type":"text","text":"abcdefghij","content":` + inner + `}`} {
			whole, err := decodeThreadLine(1, []byte(leBlocks(block)), nil)
			if err != nil {
				t.Fatal(err)
			}
			want, wantErr := whole.Estimate()
			got, err := leLine(t, leBlocks(block)).Estimate()
			if wantErr != nil || err != nil || got != want {
				t.Errorf("%s past the bound inside %.30s: estimated %+v (%v), whole %+v (%v)", link, block, got, err,
					want, wantErr)
			}
		}
	}
	// As deep as a line is read: the record, its message and content, then
	// two containers a level and the text's object.
	deep := leChain((jsonscan.MaxDepth-2)/2, "content")
	for _, p := range leProducers {
		l, err := p.read([]byte(leUser(deep, "")))
		if err != nil {
			t.Fatalf("a chain to the depth bound: %s reads it as %v", p.name, err)
		}
		if got, err := l.Estimate(); err == nil || !strings.Contains(err.Error(), "nested past") {
			t.Errorf("a chain to the depth bound: %s estimates %+v (%v), want the nesting fault", p.name, got, err)
		}
	}
}

// A LINE READ UNASKED ANSWERS NO ESTIMATE: a reader that did not ask
// ([threadReader.Estimating]), or asked and was reset, answers
// [errNotEstimated] of both accessors, which the next request reads as not
// read; and its pass counts nothing and reads nothing for the estimate, so
// an unasked reading never pays for one.
//
// Mutation: estimate whatever was asked, or answer an unasked line's
// estimate as nothing, and this fails.
func TestALineReadUnaskedAnswersNoEstimate(t *testing.T) {
	t.Parallel()
	for _, record := range []string{
		leDelivered(`{"result":{"content":[{"type":"image"}]}}`),
		leAttachment(`{"type":"queued_command","taskDelivery":{"result":{"content":[{"type":"image"}]}}}`, leRendered),
		leReads(`[` + lePDFRead("2") + `]`),
	} {
		for _, reset := range []bool{false, true} {
			r := newThreadReader(strings.NewReader(record+"\n"), threadPlainMax)
			if reset {
				r.Estimating()
				r.Reset(strings.NewReader(record+"\n"), threadPlainMax, threadLinesMax)
			}
			l, err := r.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got, err := l.Estimate(); !errors.Is(err, errNotEstimated) {
				t.Errorf("%.60s, reset %t: estimated at %+v (%v), want errNotEstimated", record, reset, got, err)
			}
			if got, read, err := l.AttachmentEstimate(); !errors.Is(err, errNotEstimated) {
				t.Errorf("%.60s, reset %t: its attachment estimated at %+v, read %t (%v), want errNotEstimated",
					record, reset, got, read, err)
			}
			if got, ok := nextCharge(l); ok {
				t.Errorf("%.60s, reset %t: the next request charges it %+v, want it not read", record, reset, got)
			}
			f := l.facts()
			if r.scan.Units() != 0 || r.dec.images != 0 || r.dec.rendered != (renderedCharge{}) || f.estimating ||
				f.message.charge != (estimateCharge{}) || f.message.emptyText || f.attachment.pdf != (pdfPages{}) ||
				f.attachment.images != 0 {
				t.Errorf("%.60s, reset %t: an unasked pass counted %d units and read images %d, %d, rendered %+v, "+
					"charge %+v, pages %+v", record, reset, r.scan.Units(), r.dec.images, f.attachment.images,
					r.dec.rendered, f.message.charge, f.attachment.pdf)
			}
		}
	}
}

// AN ESTIMATING READER ALLOCATES NOTHING AN ELEMENT: a result of nested
// elements, a document of a content source of a text block each, read
// estimating, allocates at four thousand elements within 16 KiB of what it
// does at one thousand (the fewest of five readings each, its own allocations
// alone, [ownAllocations]), so no charge, fault or walk of an element is held
// past it.
//
// Mutation: hold an element's charge in a growing list, or make a fault an
// error as it is met, and this fails.
func TestAnEstimatingReaderAllocatesNothingAnElement(t *testing.T) {
	element := `{"type":"document","source":{"type":"content","content":[{"type":"text","text":"ab"}]},` +
		`"title":"a.json"}`
	head := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_1","content":`
	tail := `}]},"uuid":"` + ctUUID(1) + `","timestamp":"` + ctStamp + `"}`
	line := func(n int) []scanPart { return scanLine(scanLit(head), scanList(element, n), scanLit(tail)) }
	allocated := func(parts []scanPart) uint64 {
		fewest := uint64(math.MaxUint64)
		for range 5 {
			r := newThreadReader(&scanGenerated{parts: line(1)}, threadPlainMax)
			r.Estimating()
			if _, err := r.Next(t.Context()); err != nil {
				t.Fatal(err)
			}
			r.Reset(&scanGenerated{parts: parts}, threadPlainMax, threadLinesMax)
			r.Estimating()
			var (
				l   threadLine
				err error
			)
			_, bytes := ownAllocations(func() { l, err = r.Next(t.Context()) })
			if got, estErr := l.Estimate(); err != nil || estErr != nil || got.At3 == 0 {
				t.Fatalf("the line reads as %v, estimated at %+v (%v); want a record and its figure", err, got, estErr)
			}
			fewest = min(fewest, bytes)
		}
		return fewest
	}
	if few, many := allocated(line(1000)), allocated(line(4000)); many > few+16<<10 {
		t.Errorf("an estimating reader allocates %d bytes at a thousand elements and %d at four thousand; want "+
			"no more than 16 KiB more", few, many)
	}
}

// scanEstimateLines is records at the estimate's edges, for the differential
// (claudelinescan_internal_test.go): every case above; chains of element
// lists to [estimateNestMax] and one past it, alone and inside a block that
// charges its JSON; and a content refused, alone and after a block charged,
// beside a delivery's image, whose message estimates nothing.
func scanEstimateLines() [][]byte {
	delivery := `"taskDelivery":{"result":{"content":[{"type":"image"}]}},`
	lines := [][]byte{[]byte(leUser(`5`, delivery)),
		[]byte(leUser(`[{"type":"text","text":"abcdefghij"},5]`, delivery))}
	for _, tc := range leMessageCases {
		lines = append(lines, []byte(tc.record))
	}
	for _, tc := range leAttachmentCases {
		lines = append(lines, []byte(tc.record))
	}
	for _, tc := range leHardCases() {
		lines = append(lines, []byte(tc.record))
	}
	for _, link := range []string{"content", "source", "both"} {
		for _, levels := range []int{estimateNestMax, estimateNestMax + 1} {
			lines = append(lines, []byte(leUser(leChain(levels, link), "")),
				[]byte(leBlocks(`{"type":"mystery","content":`+leChain(levels, link)+`}`)))
		}
	}
	return lines
}
