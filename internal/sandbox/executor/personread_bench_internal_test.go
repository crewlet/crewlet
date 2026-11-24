package executor

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAPersonsReadAtItsBounds measures a person's read of a session at
// its bounds, every carrier ([personBenchCarriers]), which set
// [TranscriptReadHeldBytes] and [PersonTranscriptReadWorst]: one value of each
// text at 32 and 64 MiB and to the read budget, held within the value bound,
// and a seat's read for words; then a call's result, its input and the model's
// words at the carrier's line bound, a value held beside a line that fills it,
// and a line past it. One case a process under GODEBUG=gctrace=1: held is the
// largest live heap at GOGC=10, peak the largest heap and the time at the
// default GOGC. A case whose read decodes its every line fails where it makes
// no entry of its value's kind.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAPersonsReadAtItsBounds$/^<case>$' -benchtime 1x
func BenchmarkAPersonsReadAtItsBounds(b *testing.B) {
	carriers := personBenchCarriers()
	for _, h := range Registered() {
		_, ok := h.(carrier)
		if ok && !slices.ContainsFunc(carriers, func(c personBenchCarrier) bool { return c.name == h.Name() }) {
			b.Fatalf("%s carries a session the bench does not write", h.Name())
		}
	}
	for _, c := range carriers {
		for _, bc := range personBenchCases(c) {
			b.Run(c.name+"_"+bc.name, func(b *testing.B) { personBenchRead(b, c, bc) })
		}
	}
}

// personBenchCarrier is how the bench writes a carrier's session: its archive
// of records, a record of a call's result, of its input and of the model's
// words, each holding one quoted value; the line its line cases are measured
// at; and the longest line its read decodes, none for 0.
type personBenchCarrier struct {
	name                string
	archive             func(b *testing.B, file string, recs []string)
	result, call, words func(quoted string) string
	bound, lineMax      int
}

// personBenchCase is one read the bench measures: its records, the reader and
// filter, and the kind of entry the value makes.
type personBenchCase struct {
	name   string
	recs   func() []string
	by     Reader
	filter TranscriptFilter
	kind   EntryKind
}

// personBenchCases is every case the bench reads of c's session: those every
// carrier writes, then those c alone writes.
func personBenchCases(c personBenchCarrier) []personBenchCase {
	const value, budget, held, half = 32<<20 - 64<<10, 240 << 20, TranscriptValueMaxBytes - 64<<10,
		TranscriptValueMaxBytes / 2
	seat := TranscriptFilter{Terms: []string{"word"}}
	var cases []personBenchCase
	add := func(name string, by Reader, filter TranscriptFilter, kind EntryKind, recs func() []string) {
		cases = append(cases, personBenchCase{name, recs, by, filter, kind})
	}
	result := func(quoted func() string) func() []string {
		return func() []string { return []string{c.result(quoted())} }
	}
	quoted := func(text string, size int) func() string {
		return func() string { return personBenchQuoted(text, size) }
	}
	within := func(text string, size int) func() string { return func() string { return personBenchHeld(text, size) } }
	for _, text := range []string{"plain", "dense", "grow", "pwd", "tokens", "kept"} {
		add(text+"_32", ReadByPerson, TranscriptFilter{}, EntryToolResult, result(quoted(text, value)))
	}
	for _, text := range []string{"dense", "grow", "pwd"} {
		add(text+"_8", ReadByPerson, TranscriptFilter{}, EntryToolResult, result(within(text, held)))
	}
	add("seat_pwd_8", ReadBySeat, seat, EntryToolResult, result(within("pwd_word", held)))
	add("seat_dense_32", ReadBySeat, seat, EntryToolResult, result(quoted("dense", value)))
	add("grow_64", ReadByPerson, TranscriptFilter{}, EntryToolResult, result(quoted("grow", 64<<20)))
	for _, text := range []string{"grow", "pwd", "pwd_escaped", "dense", "tokens", "kept"} {
		add("budget_"+text, ReadByPerson, TranscriptFilter{}, EntryToolResult, result(quoted(text, budget)))
		add("seat_budget_"+text, ReadBySeat, seat, EntryToolResult, result(quoted(text, budget)))
	}
	line := c.bound - 128<<10
	plain := quoted("plain", line)
	add("line_result", ReadByPerson, TranscriptFilter{}, EntryToolResult, result(plain))
	add("line_call", ReadByPerson, TranscriptFilter{}, EntryToolCall, func() []string { return []string{c.call(plain())} })
	add("line_words", ReadByPerson, TranscriptFilter{}, EntryModel, func() []string { return []string{c.words(plain())} })
	add("line_past", ReadByPerson, TranscriptFilter{}, EntryToolResult, result(quoted("plain", 2*c.bound)))
	add("line_pwdheld", ReadByPerson, TranscriptFilter{}, EntryToolResult, func() []string {
		v := personBenchHeld("pwd", held)
		return []string{c.result(v + `,"pad":` + personBenchQuoted("plain", line-len(v)))}
	})
	pwd := func(n int) string { return personBenchHeld("pwd", n) }
	switch c.name {
	case ClaudeCodeName:
		add("joined", ReadByPerson, TranscriptFilter{}, EntryToolResult, func() []string {
			return []string{personBenchResult(personBenchParts(pwd(held), pwd(held)), "")}
		})
		add("retained", ReadByPerson, TranscriptFilter{}, EntryToolResult, func() []string {
			return []string{
				personBenchResult(personBenchParts(pwd(half), pwd(half-64<<10)),
					`,"toolUseResult":{"stdout":`+pwd(held)+`}`),
				personBenchResult(pwd(held), ""),
			}
		})
		// OpenCode reads a call's id as a word of its walk's own few bytes.
		add("ids", ReadByPerson, TranscriptFilter{}, EntryToolCall, func() []string {
			var blocks strings.Builder
			for i := range 16 {
				if i > 0 {
					blocks.WriteByte(',')
				}
				blocks.WriteString(`{"type":"tool_use","id":` + personBenchHeld("plain", held-i) +
					`,"name":"Bash","input":{}}`)
			}
			return []string{`{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant",` +
				`"content":[` + blocks.String() + `]}}`}
		})
	case OpenCodeName:
		add("joined", ReadByPerson, TranscriptFilter{}, EntryToolResult, func() []string {
			return []string{personBenchCall("1", `{"status":"error","input":{"t":1},"error":{"type":`+
				pwd(held)+`,"message":`+pwd(half)+`},"content":`+
				personBenchParts(pwd(half-64<<10), pwd(held))+`}`)}
		})
		add("retained", ReadByPerson, TranscriptFilter{}, EntryToolResult, func() []string {
			return []string{
				personBenchCall("1", `{"status":"error","input":{"t":1},"error":{"type":`+pwd(held)+`}}`),
				personBenchCall("2", `{"status":"error","input":{"t":1},"error":{"message":`+pwd(held)+`}}`),
				c.result(pwd(held)),
			}
		})
	case GeminiCLIName:
		// A response is read once the next entry begins, so its words are held
		// beside the next line.
		add("line_held", ReadByPerson, TranscriptFilter{}, EntryModel, func() []string {
			return []string{c.words(plain()), c.result(plain())}
		})
	}
	return cases
}

// personBenchRead measures one read of bc's session as c writes it.
func personBenchRead(b *testing.B, c personBenchCarrier, bc personBenchCase) {
	h, ok := carrierOf(c.name)
	if !ok {
		b.Fatalf("%s carries no session", c.name)
	}
	file := filepath.Join(b.TempDir(), "session")
	reads := personBenchWrite(b, c, bc, file)
	open := func(context.Context) (io.ReadCloser, error) { return benchOpen(file) }
	runtime.GC()
	debug.FreeOSMemory()
	fmt.Fprintf(os.Stderr, "person-read-bench %s: the read starts\n", b.Name())
	b.ResetTimer()
	for range b.N {
		start := time.Now()
		got, err := ReadTranscriptAs(b.Context(), bc.by, bc.filter, c.name, h.ArchiveFormat(), open, 0)
		elapsed := time.Since(start) //fixedwait:measure the read's time is what the case reports
		if err != nil {
			b.Fatalf("the read: %v", err)
		}
		largest, made := 0, false
		kinds := make([]EntryKind, 0, len(got.Entries))
		for _, e := range got.Entries {
			largest, made = max(largest, e.Bytes), made || e.Kind == bc.kind
			kinds = append(kinds, e.Kind)
		}
		fmt.Fprintf(os.Stderr, "person-read-bench %s: the read ends in %s, kinds %v, largest %d, cut %q\n",
			b.Name(), elapsed, kinds, largest, got.Cut)
		if reads && !made {
			b.Fatalf("the read made no %s entry: %v", bc.kind, kinds)
		}
	}
	b.StopTimer()
}

// personBenchWrite writes bc's records to file as c's session, and reports
// whether its read makes the value's entry: unfiltered, decoding every line.
// The records are let go of on return, before the read.
func personBenchWrite(b *testing.B, c personBenchCarrier, bc personBenchCase, file string) bool {
	recs := bc.recs()
	c.archive(b, file, recs)
	return len(bc.filter.Terms) == 0 && !slices.ContainsFunc(recs, func(r string) bool {
		return c.lineMax > 0 && len(r) > c.lineMax
	})
}

// personBenchCarriers is every carrier's session as the bench writes it.
func personBenchCarriers() []personBenchCarrier {
	const stamp = `"timestamp":"2026-01-01T00:00:00Z"`
	member := func(name string) func(*testing.B, string, []string) {
		return func(b *testing.B, file string, recs []string) {
			benchArchive(b, file, func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
				benchMember(b, tw, name, strings.Join(recs, "\n")+"\n")
				return sandbox.HoldingsAsk{}
			})
		}
	}
	return []personBenchCarrier{
		{name: ClaudeCodeName, bound: maxLineBytes,
			archive: member("projects/" + claudeProjectDir + "/" + ctSession + ".jsonl"),
			result:  func(q string) string { return personBenchResult(q, "") },
			call: func(q string) string {
				return `{"type":"assistant",` + stamp + `,"message":{"role":"assistant","content":[{"type":` +
					`"tool_use","id":"toolu_1","name":"Bash","input":{"command":` + q + `}}]}}`
			},
			words: func(q string) string {
				return `{"type":"assistant",` + stamp + `,"message":{"role":"assistant","content":[{"type":` +
					`"text","text":` + q + `}]}}`
			}},
		{name: OpenCodeName, bound: maxLineBytes,
			archive: func(b *testing.B, file string, recs []string) {
				benchExportFile(b, file, func(b *testing.B, w io.Writer) []sandbox.LoggedCall {
					_, _ = io.WriteString(w, benchHead+","+ohStepOpen+strings.Join(recs, ",")+ohStepClose+benchTail)
					return nil
				})
			},
			result: func(q string) string {
				return personBenchCall("3", `{"status":"completed","input":{"t":1},"content":[{"type":"text",`+
					`"text":`+q+`}]}`)
			},
			call: func(q string) string {
				return personBenchCall("1", `{"status":"completed","input":{"command":`+q+`},"content":`+
					`[{"type":"text","text":"ok"}]}`)
			},
			words: func(q string) string { return `{"type":"text","text":` + q + `}` }},
		{name: QwenCodeName, bound: maxLineBytes, lineMax: maxLineBytes,
			archive: member("projects/k/chats/" + ctSession + ".jsonl"),
			result: func(q string) string {
				return `{"type":"tool_result",` + stamp + `,"message":{"role":"user","parts":[{"functionResponse":` +
					`{"id":"c1","name":"run_shell_command","response":{"output":` + q + `}}}]}}`
			},
			call: func(q string) string {
				return `{"type":"assistant",` + stamp + `,"message":{"role":"model","parts":[{"functionCall":` +
					`{"id":"c1","name":"run_shell_command","args":{"command":` + q + `}}}]}}`
			},
			words: func(q string) string {
				return `{"type":"assistant",` + stamp + `,"message":{"role":"model","parts":[{"text":` + q + `}]}}`
			}},
		{name: GeminiCLIName, bound: maxLineBytes, lineMax: maxLineBytes, archive: member("chats/session-x.jsonl"),
			result: func(q string) string {
				return `{"id":"u2",` + stamp + `,"type":"user","content":[{"functionResponse":` +
					`{"id":"c1","name":"run_shell_command","response":{"output":` + q + `}}}]}`
			},
			call: func(q string) string {
				return `{"id":"m1",` + stamp + `,"type":"gemini","content":"","toolCalls":[{"id":"c1",` +
					`"name":"run_shell_command","args":{"command":` + q + `}}]}`
			},
			words: func(q string) string { return `{"id":"m1",` + stamp + `,"type":"gemini","content":` + q + `}` }},
		{name: KimiName, bound: maxLineBytes, lineMax: maxLineBytes,
			archive: member("wd_a_0/session_x/agents/main/wire.jsonl"),
			result: func(q string) string {
				return `{"type":"context.append_loop_event","time":1,"event":{"type":"tool.result",` +
					`"toolCallId":"c1","result":{"output":` + q + `,"isError":false}}}`
			},
			call: func(q string) string {
				return `{"type":"context.append_loop_event","time":1,"event":{"type":"tool.call",` +
					`"toolCallId":"c1","name":"Shell","args":{"command":` + q + `}}}`
			},
			words: func(q string) string {
				return `{"type":"context.append_loop_event","time":1,"event":{"type":"content.part",` +
					`"part":{"type":"text","text":` + q + `}}}`
			}},
		{name: CopilotName, bound: maxLineBytes, lineMax: maxLineBytes,
			archive: member(copilotSessionState + "/s/events.jsonl"),
			result: func(q string) string {
				return `{"type":"tool.execution_complete",` + stamp + `,"data":{"toolCallId":"c1","success":true,` +
					`"result":{"content":` + q + `}}}`
			},
			call: func(q string) string {
				return `{"type":"assistant.message",` + stamp + `,"data":{"content":"","toolRequests":` +
					`[{"toolCallId":"c1","name":"bash","arguments":{"command":` + q + `}}]}}`
			},
			words: func(q string) string {
				return `{"type":"assistant.message",` + stamp + `,"data":{"content":` + q + `}}`
			}},
		{name: PiName, bound: maxLineBytes, lineMax: maxLineBytes, archive: member(".pi/sessions/s.jsonl"),
			result: func(q string) string {
				return `{"type":"message","id":"e2",` + stamp + `,"message":{"role":"toolResult","toolCallId":"c1",` +
					`"toolName":"bash","content":[{"type":"text","text":` + q + `}],"isError":false}}`
			},
			call: func(q string) string {
				return `{"type":"message","id":"e1",` + stamp + `,"message":{"role":"assistant","content":` +
					`[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":` + q + `}}]}}`
			},
			words: func(q string) string {
				return `{"type":"message","id":"e1",` + stamp + `,"message":{"role":"assistant","content":` +
					`[{"type":"text","text":` + q + `}]}}`
			}},
		{name: GrokName, bound: maxLineBytes, lineMax: maxLineBytes, archive: member("sessions/g1/chat_history.jsonl"),
			result: func(q string) string { return `{"type":"tool_result","tool_call_id":"c1","content":` + q + `}` },
			call: func(q string) string {
				return `{"type":"assistant","content":"","tool_calls":[{"id":"c1","name":"bash","arguments":` +
					benchStringOf(`{"command":`+q+`}`) + `}]}`
			},
			words: func(q string) string { return `{"type":"assistant","content":` + q + `}` }},
		{name: MuseName, bound: maxLineBytes, lineMax: maxLineBytes, archive: member("2026/01/01/s/session.jsonl"),
			result: func(q string) string {
				return `{"payload_type":"runtime.session","recorded_at":1,"payload":{"event":` +
					`{"kind":"tool_result_batch_committed","results":[{"tool_call_id":"c1","text":` + q + `}]}}}`
			},
			call: func(q string) string {
				return `{"payload_type":"runtime.session","recorded_at":1,"payload":{"event":` +
					`{"kind":"assistant_tool_calls_committed","tool_calls":[{"call_id":"c1","name":"bash","args":` +
					benchStringOf(`{"command":`+q+`}`) + `}]}}}`
			},
			words: func(q string) string {
				return `{"payload_type":"runtime.session","recorded_at":1,"payload":{"event":` +
					`{"kind":"assistant_message_committed","text":` + q + `}}}`
			}},
		{name: HermesName, bound: hermesRowMaxBytes, lineMax: hermesRowMaxBytes, archive: member(hermesArchiveMember),
			result: func(q string) string {
				return `{"t":"messages","r":{"id":2,"role":"tool","tool_call_id":"c1","tool_name":"terminal",` +
					`"content":` + q + `,"timestamp":1.5}}`
			},
			call: func(q string) string {
				return `{"t":"messages","r":{"id":1,"role":"assistant","content":"","tool_calls":[{"id":"c1",` +
					`"type":"function","function":{"name":"terminal","arguments":` + benchStringOf(`{"command":`+q+`}`) +
					`}}],"timestamp":1.5}}`
			},
			words: func(q string) string {
				return `{"t":"messages","r":{"id":1,"role":"assistant","content":` + q + `,"timestamp":1.5}}`
			}},
		{name: CursorAgentName, bound: maxLineBytes, lineMax: maxLineBytes,
			archive: member(cursorArchiveDir + "/" + ctSession + ".jsonl"),
			result: func(q string) string {
				return `{"tool":{"toolCallId":"c1","toolName":"Shell","content":[{"text":{"text":` + q +
					`}}],"isError":false}}`
			},
			call: func(q string) string {
				return `{"assistant":{"content":[{"toolCall":{"toolCallId":"c1","toolName":"Shell","argsJson":` +
					benchStringOf(`{"command":`+q+`}`) + `}}]}}`
			},
			words: func(q string) string { return `{"assistant":{"content":[{"text":{"text":` + q + `}}]}}` }},
		{name: CodexName, bound: codexRolloutLineMaxBytes, lineMax: codexRolloutLineMaxBytes,
			archive: member("rollout-" + codexCarriedStamp + "-" + ctSession + ".jsonl"),
			result: func(q string) string {
				return `{` + stamp + `,"type":"response_item","payload":{"type":"function_call_output",` +
					`"call_id":"c1","output":` + q + `}}`
			},
			call: func(q string) string {
				return `{` + stamp + `,"type":"response_item","payload":{"type":"function_call","name":"shell",` +
					`"arguments":` + benchStringOf(`{"command":`+q+`}`) + `,"call_id":"c1"}}`
			},
			words: func(q string) string {
				return `{` + stamp + `,"type":"response_item","payload":{"type":"message","role":"assistant",` +
					`"content":[{"type":"output_text","text":` + q + `}]}}`
			}},
	}
}

// benchStringOf is text as one JSON string.
func benchStringOf(text string) string {
	raw, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// personBenchResult is a Claude Code record of a tool's result whose content is
// content, more members after its message.
func personBenchResult(content, more string) string {
	return `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":` +
		`[{"type":"tool_result","tool_use_id":"toolu_1","content":` + content + `}]}` + more + `}`
}

// personBenchParts is a result's list of text parts, each quoted.
func personBenchParts(quoted ...string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, q := range quoted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"text","text":` + q + `}`)
	}
	b.WriteByte(']')
	return b.String()
}

// personBenchCall is an OpenCode item of a call whose state is state.
func personBenchCall(id, state string) string {
	return `{"type":"tool","id":"call_` + id + `","name":"crewlet_echo","state":` + state +
		`,"time":{"created":1,"ran":2,"completed":3}}`
}

// personBenchQuoted is a JSON string of about size bytes as written, of text:
// plain words; every credential shape a sentence apart (dense); a password a
// space apart (grow); a password's value every six bytes (pwd: `pwd=x` lines),
// and every seventh line a word a seat's term finds (pwd_word), or every seven
// as a JSON string holds them, joined by escaped breaks (pwd_escaped), whose
// redaction lengthens them most; a key's armour and then 200-byte tokens; or
// JSON lines of a kept session.
func personBenchQuoted(text string, size int) string {
	var units []string
	switch text {
	case "plain":
		units = []string{"line of the log the tool printed, plain words. "}
	case "dense":
		for _, s := range redacttest.Shapes() {
			units = append(units, "the tool printed "+s.Sample+" then a word.\n")
		}
	case "grow":
		units = []string{"password: dK8yFgN1b3jQ4e6m "}
	case "pwd":
		units = []string{"pwd=x\n"}
	case "pwd_word":
		units = []string{strings.Repeat("pwd=x\n", 6) + "word\n"}
	case "pwd_escaped":
		units = []string{`pwd=x\n`}
	case "tokens":
		units = []string{personBenchTokens(1 << 20)}
	case "kept":
		units = []string{personBenchKept(1 << 20)}
	}
	escaped := make([]string, len(units))
	for i, u := range units {
		raw, err := json.Marshal(u)
		if err != nil {
			panic(err)
		}
		escaped[i] = string(raw[1 : len(raw)-1])
	}
	var sb strings.Builder
	sb.Grow(size + 2)
	sb.WriteByte('"')
	for i := 0; ; i++ {
		u := escaped[i%len(escaped)]
		if sb.Len()+len(u) > size {
			break
		}
		sb.WriteString(u)
	}
	sb.WriteByte('"')
	return sb.String()
}

// personBenchHeld is a JSON string of text whose decoded words are just within
// n bytes, as a value is held to the value bound.
func personBenchHeld(text string, n int) string {
	unit := personBenchQuoted(text, 1<<20)
	var words string
	if err := json.Unmarshal([]byte(unit), &words); err != nil {
		panic(err)
	}
	var sb strings.Builder
	for sb.Len() < n {
		sb.WriteString(words[:min(len(words), n-sb.Len())])
	}
	raw, err := json.Marshal(strings.ToValidUTF8(sb.String(), ""))
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// personBenchTokens is n bytes of a key's armour and then 200-byte tokens, as
// internal/redact's own benchmark streams.
func personBenchTokens(n int) string {
	r := rand.New(rand.NewPCG(1, 2))
	const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var b strings.Builder
	b.WriteString("a key: -----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n\n")
	for b.Len() < n {
		b.WriteString("sk-")
		for range 197 {
			b.WriteByte(b64url[r.IntN(64)])
		}
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

// personBenchKept is n bytes of what a kept session holds, as
// internal/redact's own benchmark streams: JSON lines of prose, credentials
// and base64 images.
func personBenchKept(n int) string {
	r := rand.New(rand.NewPCG(1, 1))
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	words := strings.Fields("the a file read write ok done test build go run error line function value key " +
		"token session user model tool result")
	var b strings.Builder
	for b.Len() < n {
		switch r.IntN(20) {
		case 0:
			b.WriteString(`{"type":"image","source":{"data":"`)
			for range 1 << (14 + r.IntN(8)) {
				b.WriteByte(b64[r.IntN(64)])
			}
			b.WriteString("\"}}\n")
		case 1:
			b.WriteString(`{"text":"export API_KEY=sk-` + strings.Repeat("Ab3x", 10) + ` and password: hunter2\n"}` + "\n")
		default:
			b.WriteString(`{"type":"assistant","text":"`)
			for k := r.IntN(200); k > 0; k-- {
				b.WriteString(words[r.IntN(len(words))])
				b.WriteByte(' ')
			}
			b.WriteString("\"}\n")
		}
	}
	return b.String()[:n]
}
