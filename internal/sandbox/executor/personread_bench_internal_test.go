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
// [TranscriptReadWorst]: one value of a tool's answer of 32 MiB (the old line
// bound) of plain words, of every credential shape and of the shape whose
// redaction lengthens it most; one held, just within the value bound; one of
// 64 MiB; a seat's read for words; and one value to the read budget of each
// slow text. One case a process, GODEBUG=gctrace=1: held is the largest live
// heap at GOGC=10, peak the largest heap and the time at the default GOGC.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAPersonsReadAtItsBounds$/^<case>$' -benchtime 1x
func BenchmarkAPersonsReadAtItsBounds(b *testing.B) {
	type read struct {
		by     Reader
		filter TranscriptFilter
	}
	person := read{ReadByPerson, TranscriptFilter{}}
	seat := read{ReadBySeat, TranscriptFilter{Terms: []string{"word"}}}
	const line, budget = 32<<20 - 64<<10, 240 << 20
	var cases []struct {
		name string
		text string
		size int
		read read
	}
	add := func(name, text string, size int, r read) {
		cases = append(cases, struct {
			name string
			text string
			size int
			read read
		}{name, text, size, r})
	}
	for _, text := range []string{"plain", "dense", "grow"} {
		add(text+"_32", text, line, person)
	}
	for _, text := range []string{"dense", "grow"} {
		add(text+"_8", text, TranscriptValueMaxBytes-64<<10, person)
	}
	add("seat_dense_32", "dense", line, seat)
	add("grow_64", "grow", 64<<20, person)
	for _, text := range []string{"grow", "dense", "tokens", "kept"} {
		add("budget_"+text, text, budget, person)
	}
	add("seat_budget_grow", "grow", budget, seat)
	for _, harness := range []string{"cc", "oc"} {
		for _, bc := range cases {
			b.Run(harness+"_"+bc.name, func(b *testing.B) {
				quoted := personBenchQuoted(bc.text, bc.size)
				var file, name string
				var format int
				if harness == "cc" {
					file, name, format = filepath.Join(b.TempDir(), "archive.tar.gz"), ClaudeCodeName, claudeArchiveFormat
					benchArchive(b, file, func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
						benchMember(b, tw, "projects/"+claudeProjectDir+"/"+ctSession+".jsonl",
							`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":`+
								`[{"type":"tool_result","tool_use_id":"toolu_1","content":`+quoted+`}]}}`+"\n")
						return sandbox.HoldingsAsk{}
					})
				} else {
					file, name, format = filepath.Join(b.TempDir(), "export.json.gz"), OpenCodeName, openCodeArchiveFormat
					benchExportFile(b, file, func(b *testing.B, w io.Writer) []sandbox.LoggedCall {
						_, _ = io.WriteString(w, benchHead+","+ohStepOpen+`{"type":"tool","id":"call_x","name":`+
							`"crewlet_echo","state":{"status":"completed","input":{"t":1},"content":[{"type":"text",`+
							`"text":`+quoted+`}]},"time":{"created":1,"ran":2,"completed":3}}`+ohStepClose+benchTail)
						return nil
					})
				}
				quoted = ""
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

// personBenchQuoted is a JSON string of about size bytes as written, of text:
// plain words; every credential shape a sentence apart (dense); the shape
// whose redaction lengthens it most, a space apart (grow); a key's armour and
// then 200-byte tokens; or JSON lines of a kept session.
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
