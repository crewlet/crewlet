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
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAPersonsReadAtItsBounds measures a person's read of a session at
// its bounds, both harnesses, which set [TranscriptReadHeldBytes] and
// [PersonTranscriptReadWorst]: one value of 32 MiB of each text (plain, every
// credential shape, the two whose redaction lengthens most, dense tokens, a
// kept session's lines) and of 64 MiB; one held just within the value bound;
// what one read holds at once; a seat's read for words; and one value of each
// slow text to the read budget, the wait set by those a stored session holds
// that long, never by `pwd=x` lines (pwd, pwd_escaped). One case a process
// under GODEBUG=gctrace=1: held is the largest live heap at GOGC=10, peak the
// largest heap and the time at the default GOGC.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAPersonsReadAtItsBounds$/^<case>$' -benchtime 1x
func BenchmarkAPersonsReadAtItsBounds(b *testing.B) {
	type read struct {
		by     Reader
		filter TranscriptFilter
	}
	person := read{ReadByPerson, TranscriptFilter{}}
	seat := read{ReadBySeat, TranscriptFilter{Terms: []string{"word"}}}
	const line, budget, held, half = 32<<20 - 64<<10, 240 << 20, TranscriptValueMaxBytes - 64<<10,
		TranscriptValueMaxBytes / 2
	// A case's records: Claude Code's lines, or OpenCode's items of one step;
	// none where the harness writes no such record.
	type records func(cc bool) []string
	var cases []struct {
		name   string
		recs   records
		read   read
		ccOnly bool
	}
	add := func(name string, recs records, r read) {
		cases = append(cases, struct {
			name   string
			recs   records
			read   read
			ccOnly bool
		}{name, recs, r, false})
	}
	one := func(text string, size int) records {
		return func(cc bool) []string { return []string{personBenchAnswer(cc, "1", personBenchQuoted(text, size))} }
	}
	for _, text := range []string{"plain", "dense", "grow", "pwd", "tokens", "kept"} {
		add(text+"_32", one(text, line), person)
	}
	// Held just within the value bound, as read: its decoded text.
	within := personBenchHeld
	for _, text := range []string{"dense", "grow", "pwd"} {
		add(text+"_8", func(cc bool) []string { return []string{personBenchAnswer(cc, "1", within(text, held))} }, person)
	}
	add("seat_pwd_8", func(cc bool) []string { return []string{personBenchAnswer(cc, "1", within("pwd_word", held))} }, seat)
	add("joined", func(cc bool) []string {
		if cc {
			return []string{personBenchResult(personBenchParts(within("pwd", held), within("pwd", held)), "")}
		}
		return []string{personBenchCall("1", `{"status":"error","input":{"t":1},"error":{"type":`+within("pwd", held)+
			`,"message":`+within("pwd", half)+`},"content":`+personBenchParts(within("pwd", half-64<<10),
			within("pwd", held))+`}`)}
	}, person)
	add("retained", func(cc bool) []string {
		if cc {
			return []string{
				personBenchResult(personBenchParts(within("pwd", half), within("pwd", half-64<<10)),
					`,"toolUseResult":{"stdout":`+within("pwd", held)+`}`),
				personBenchResult(within("pwd", held), ""),
			}
		}
		return []string{
			personBenchCall("1", `{"status":"error","input":{"t":1},"error":{"type":`+within("pwd", held)+`}}`),
			personBenchCall("2", `{"status":"error","input":{"t":1},"error":{"message":`+within("pwd", held)+`}}`),
			personBenchAnswer(false, "3", within("pwd", held)),
		}
	}, person)
	add("ids", func(bool) []string {
		var blocks strings.Builder
		for i := range 16 {
			if i > 0 {
				blocks.WriteByte(',')
			}
			blocks.WriteString(`{"type":"tool_use","id":` + within("plain", held-i) + `,"name":"Bash","input":{}}`)
		}
		return []string{`{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant",` +
			`"content":[` + blocks.String() + `]}}`}
	}, person)
	cases[len(cases)-1].ccOnly = true // OpenCode reads a call's id as a word of its walk's own few bytes
	add("seat_dense_32", one("dense", line), seat)
	add("grow_64", one("grow", 64<<20), person)
	for _, text := range []string{"grow", "pwd", "pwd_escaped", "dense", "tokens", "kept"} {
		add("budget_"+text, one(text, budget), person)
	}
	for _, text := range []string{"grow", "pwd", "pwd_escaped", "dense", "tokens", "kept"} {
		add("seat_budget_"+text, one(text, budget), seat)
	}
	for _, harness := range []string{"cc", "oc"} {
		for _, bc := range cases {
			if bc.ccOnly && harness != "cc" {
				continue
			}
			b.Run(harness+"_"+bc.name, func(b *testing.B) {
				recs := bc.recs(harness == "cc")
				var file, name string
				var format int
				if harness == "cc" {
					file, name, format = filepath.Join(b.TempDir(), "archive.tar.gz"), ClaudeCodeName, claudeArchiveFormat
					benchArchive(b, file, func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
						benchMember(b, tw, "projects/"+claudeProjectDir+"/"+ctSession+".jsonl",
							strings.Join(recs, "\n")+"\n")
						return sandbox.HoldingsAsk{}
					})
				} else {
					file, name, format = filepath.Join(b.TempDir(), "export.json.gz"), OpenCodeName, openCodeArchiveFormat
					benchExportFile(b, file, func(b *testing.B, w io.Writer) []sandbox.LoggedCall {
						_, _ = io.WriteString(w, benchHead+","+ohStepOpen+strings.Join(recs, ",")+ohStepClose+benchTail)
						return nil
					})
				}
				recs = nil
				open := func(context.Context) (io.ReadCloser, error) { return benchOpen(file) }
				runtime.GC()
				debug.FreeOSMemory()
				fmt.Fprintf(os.Stderr, "person-read-bench %s: the read starts\n", b.Name())
				b.ResetTimer()
				for range b.N {
					start := time.Now()
					got, err := ReadTranscriptAs(b.Context(), bc.read.by, bc.read.filter, name, format, open, 0)
					elapsed := time.Since(start) //fixedwait:measure the read's time is what the case reports
					if err != nil {
						b.Fatalf("the read: %v", err)
					}
					fmt.Fprintf(os.Stderr, "person-read-bench %s: the read ends in %s, %d entries, cut %q\n",
						b.Name(), elapsed, len(got.Entries), got.Cut)
				}
				b.StopTimer()
			})
		}
	}
}

// personBenchAnswer is a call's result of one text, quoted: Claude Code's record
// of it, or an OpenCode item of a completed call answering it.
func personBenchAnswer(cc bool, id, quoted string) string {
	if cc {
		return personBenchResult(quoted, "")
	}
	return personBenchCall(id, `{"status":"completed","input":{"t":1},"content":[{"type":"text","text":`+quoted+`}]}`)
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
