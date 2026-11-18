package executor

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
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
		got, err := ctLine(t, tc.record).Estimate()
		if err != nil || got != tc.want {
			t.Errorf("%s: estimated at %+v (%v), want %+v", tc.name, got, err, tc.want)
		}
	}
	thrown := ctLine(t, leUser(`[{"type":"document","source":{"type":"text","data":"abcdefgh"},"title":5}]`, ""))
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
		if got, err := ctLine(t, leUser(content, "")).Estimate(); err != nil || got != want {
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
		got, read, err := ctLine(t, tc.record).AttachmentEstimate()
		if err != nil || got != tc.want || read != tc.read {
			t.Errorf("%s: estimated at %+v, read %t (%v); want %+v, read %t", tc.name, got, read, err, tc.want,
				tc.read)
		}
	}
	if got, read, err := ctLine(t, leAttachment(`null`, "")).AttachmentEstimate(); err == nil {
		t.Errorf("an attachment that is null is estimated at %+v, read %t, want its type's error", got, read)
	}
}

// leProducers is every producer of a line's facts, each by how a reader reads
// a line through it.
var leProducers = []struct {
	name string
	read func(raw []byte) (threadLine, error)
}{
	{"the whole line's decode", func(raw []byte) (threadLine, error) { return decodeThreadLine(1, raw) }},
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
