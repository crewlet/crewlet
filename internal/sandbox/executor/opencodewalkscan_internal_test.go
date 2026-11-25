package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// exportDoc is one export document of the walk's corpus, as written.
type exportDoc struct {
	name string
	doc  string
}

// exportPieces hands r's bytes at most n a read, so a scanner's buffer meets
// every edge a short piece makes.
type exportPieces struct {
	r io.Reader
	n int
}

func (p *exportPieces) Read(b []byte) (int, error) { return p.r.Read(b[:min(len(b), p.n)]) }

// exportCorpus is every export the walk is held to its readers' own walks
// over: each release's real exports (the person's, the holdings', and the
// pages of a session's message list, wrapped as an export's messages), and
// documents made of messages written every way a box can write one, whole,
// cut short and followed by more.
func exportCorpus(t *testing.T) []exportDoc {
	t.Helper()
	var corpus []exportDoc
	root := filepath.Join("testdata", "opencode")
	for _, path := range []string{"2.3.2/export.json", "2.3.2/text.json", "2.3.2/tool.json"} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, exportDoc{path, string(raw)})
	}
	for _, release := range []string{"2.3.2"} {
		archives, err := filepath.Glob(filepath.Join(root, release, "holdings", "*.json.gz"))
		if err != nil {
			t.Fatal(err)
		}
		for _, archive := range archives {
			if strings.HasSuffix(archive, "manifest.json.gz") {
				continue
			}
			corpus = append(corpus, exportDoc{archive, gunzipped(t, archive)})
		}
		pages, err := filepath.Glob(filepath.Join(root, release, "context", "page-*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range pages {
			raw, err := os.ReadFile(page)
			if err != nil {
				t.Fatal(err)
			}
			var list struct {
				Data json.RawMessage `json:"data"`
			}
			if err = json.Unmarshal(raw, &list); err != nil {
				t.Fatal(err)
			}
			corpus = append(corpus, exportDoc{page, `{"info":{"id":"ses_page"},"messages":` + string(list.Data) + `}`})
		}
	}
	return append(corpus, exportSynthetic()...)
}

// gunzipped is the file at path, gunzipped.
func gunzipped(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

// exportVariants are messages written every way the walk's readers read apart:
// every member order, keys written twice at every level, values of every kind
// where a reader expects one, values past the capture's bound and words past
// the walk's buffer, names escaped, cased otherwise or longer than any, text
// that is no UTF-8, and nesting to the scanner's depth.
func exportVariants() []string {
	long := strings.Repeat("e", exportValueMaxBytes)
	tool := func(state string) string {
		return `{"type":"tool","id":"call_1","name":"crewlet_echo","executed":false,"state":` + state +
			`,"time":{"created":1,"ran":2,"completed":3}}`
	}
	done := `{"status":"completed","input":{"t":1},"content":[{"type":"text","text":"done"}],` +
		`"metadata":{"truncated":false}}`
	step := func(content string) string {
		return `{"id":"msg_s","type":"assistant","time":{"created":1,"completed":2},"model":{"id":"m",` +
			`"providerID":"stub"},"content":` + content + `,"tokens":{"input":100,"output":5,"reasoning":1,` +
			`"cache":{"read":20,"write":3}}}`
	}
	return []string{
		ocUser("msg_u", "the brief", `[]`),
		ocUser("msg_u", "a file", `[{"type":"file","mime":"application/pdf"},{"mime":"image/png"},7]`),
		ocUser("msg_u", "files", `{"x":1}`),
		ocUser("msg_u", "files", `null`),
		ocIdle,
		`{}`,
		`{"type":"idle"}`,
		ocCompaction("msg_c", "completed"),
		ocCompaction("msg_c", "running"),
		ocReply("msg_r", `"stub"`, `[`+ocTool(`[{"type":"text","text":"ok"}]`)+`]`, 1000, 10, 2, 30, 4),
		ocReply("msg_r", `"stub"`, `[`+ocTool(`[{"type":"file","mime":"application/pdf"},{"type":"text"},"x"]`)+`]`,
			10, 0, 0, 3, 0),
		step(`[` + tool(done) + `]`),
		step(`[` + tool(`{"status":"error","input":{},"error":{"type":"tool.execution","message":"boom"}}`) + `]`),
		step(`[` + tool(`{"status":"error","error":[1,2],"content":{"a":1}}`) + `]`),
		step(`[` + tool(`{"status":"error","error":"words","content":7}`) + `]`),
		step(`[` + tool(`null`) + `,` + tool(`"words"`) + `,` + tool(`[1]`) + `]`),
		step(`[7,"x",null,[],{"type":"text","text":"hm"},{"type":"reasoning","text":"r"}]`),
		step(`"words"`), step(`7`), step(`null`), step(`{"a":1}`),
		step(`[{"state":` + done + `,"type":"tool"},{"type":"tool","type":"text","state":` + done + `}]`),
		step(`[{"type":"tool","state":{"status":"x"},"state":` + done + `}]`),
		step(`[{"type":"tool","state":{"content":[{"type":"file"}],"content":[{"mime":"application/pdf",` +
			`"mime":"image/png"}],"error":{},"error":null}}]`),
		step(`[{"type":"toolXXXXXXXXXXXXXXXXXXXXXXX","state":` + done + `}]`),
		step(`[{"type":"tool","state":{"content":[{"type":"file","mime":"application/pdfXXXXXXXXXXXXXXX"}]}}]`),
		step(`[{"t\u0079pe":"tool","state":{"c\u006fntent":[{"m\u0069me":"application\/pdf"}]}}]`),
		`{"content":[],"tokens":{"input":9,"cache":{"read":1,"write":1}},"time":{"completed":2},"type":"assistant"}`,
		`{"type":"assistant","type":"user","time":{"completed":2},"tokens":{"input":7}}`,
		`{"type":"assistant","time":{"completed":2},"time":{"completed":0},"tokens":{"input":7}}`,
		`{"type":"assistant","Type":"user","time":{"completed":2},"tokens":{"input":7}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},"tokens":{"input":8},"error":null}`,
		`{"typ\u0065":"assistant","time":{"completed":2},"tokens":{"input":8}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},"error":{"name":"x"}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":"lots"}`,
		`{"type":["assistant"],"time":{"completed":2},"tokens":{"input":7}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7},"error":{"m":"` + long + `"}}`,
		`{"type":"assistant","time":{"completed":2,"n":"` + long + `"},"tokens":{"input":7}}`,
		`{"type":"assistant","time":{"completed":2},"tokens":{"input":7,"n":"` + long + `"}}`,
		`{"type":"` + long + `","time":{"completed":2},"tokens":{"input":7}}`,
		`{"type":"compaction","id":"` + long + `","status":"completed"}`,
		`{"type":"compaction","id":"msg_c","status":"` + long + `"}`,
		ocReply("msg_r", `"`+long+`"`, `[]`, 10, 0, 0, 3, 0),
		ocReply("msg_r", `"stub"`, `[]`, 10, 0, 0, 3, 0)[:len(ocReply("msg_r", `"stub"`, `[]`, 10, 0, 0, 3, 0))-1] +
			`,"providerResultStateAndMore":1,"messagesmessages":[]}`,
		"{\"type\":\"user\",\"text\":\"\xff\xfe invalid \\ud800 lone\",\"files\":[{\"mime\":\"\xffapplication/pdf\"}]}",
		`{"type":"user","deep":` + strings.Repeat("[", jsonscan.MaxDepth-3) + strings.Repeat("]", jsonscan.MaxDepth-3) +
			`}`,
		`"no object"`,
		`7`,
		`null`,
		`{"type":"user","text":"` + strings.Repeat("x", 3<<10) + `"}`,
	}
}

// exportSynthetic is documents of [exportVariants], three hundred of one to
// six messages each, some cut short, some followed by more, some with two
// lists or keys around the list, and the documents that are no export.
func exportSynthetic() []exportDoc {
	r := rand.New(rand.NewPCG(1170, 5))
	variants := exportVariants()
	var docs []exportDoc
	for _, v := range variants {
		docs = append(docs, exportDoc{"one variant", `{"info":{"id":"ses_x"},"messages":[` + v + `]}`})
	}
	for range 300 {
		var messages []string
		for range 1 + r.IntN(6) {
			messages = append(messages, variants[r.IntN(len(variants))])
		}
		list := `[` + strings.Join(messages, ",") + `]`
		doc := `{"info":{"id":"ses_x"},"messages":` + list + `}`
		switch r.IntN(8) {
		case 0:
			doc = doc[:r.IntN(len(doc))]
		case 1:
			doc += ` {"more":1}`
		case 2:
			doc = `{"messages":[` + ocIdle + `],"info":{"x":[1,2]},"messages":` + list + `}`
		case 3:
			doc = "\n {\"extra\":[1,{\"a\":2}],\n\"messages\" : " + list + ",\"after\":true}\n"
		}
		docs = append(docs, exportDoc{"synthetic", doc})
	}
	for _, doc := range []string{`[1,2]`, `{"messages":5}`, `{"messages":{}}`, `{}`, ``, ` `, `{"messages":[]} x`,
		`{"m\u0065ssages":[` + ocIdle + `]}`} {
		docs = append(docs, exportDoc{"no export", doc})
	}
	return docs
}

// meterReadOf is what the meter's own walk read of doc ([walkExport]), whole,
// and what the export walk read of it, whole and in pieces of seventeen
// bytes: the figure, the count's reading and the error's words.
func meterReadOf(t *testing.T, doc string) (oracle, whole, pieces string) {
	t.Helper()
	read := func(figure int64, count openCodeCountReading, err error) string {
		said := "<nil>"
		if err != nil {
			said = err.Error()
		}
		return fmt.Sprintf("%d %+v %s", figure, count, said)
	}
	var c openCodeCountReading
	figure, err := walkExport(t.Context(), bytes.NewReader(gzipOf(t, doc)), &c)
	oracle = read(figure, c, err)
	c = openCodeCountReading{}
	figure, err = countExport(t.Context(), bytes.NewReader(gzipOf(t, doc)), &c, nil)
	whole = read(figure, c, err)
	c = openCodeCountReading{}
	m := &exportMeter{count: &c}
	err = scanExport(t.Context(), &exportPieces{r: strings.NewReader(doc), n: 17}, m)
	if err != nil {
		m.last = 0
	}
	pieces = read(m.last, c, err)
	return oracle, whole, pieces
}

// THE METER READS AN EXPORT AS ITS OWN WALK DID: over every export of the
// corpus, the meter on the export walk, read whole and in pieces of seventeen
// bytes, answers the figure, the count and the error the walk it kept before
// it answered ([walkExport]), to the word.
//
// Mutation: fold a key written twice by its first value, read a capture past
// its bound as absent, charge a state's failure only where it is an object,
// or count a list's file marks from its last list alone, and a case goes red.
func TestTheMeterReadsAnExportAsItsWalkDid(t *testing.T) {
	t.Parallel()
	for _, d := range exportCorpus(t) {
		oracle, whole, pieces := meterReadOf(t, d.doc)
		if whole != oracle || pieces != oracle {
			t.Errorf("%s %.300q:\nwalk   %.400s\npieces %.400s\nwant   %.400s", d.name, d.doc, whole, pieces, oracle)
		}
	}
}

// benchExportMeter measures the meter on the export walk against the walk it
// kept before ([walkExport]), each a sub-benchmark of its own, on exports at
// the plaintext's bound: 255 MiB of ordinary steps, each one call answered
// with a kibibyte, of `{}` messages and of `{"type":"idle"}` messages, and of
// steps whose tokens are captured at the bound. The take's pipe waits on the
// meter, so the walk must keep up with the walk it replaced. Run one case a
// process, reading GODEBUG=gctrace=1 between its stderr lines.
func benchExportMeter(b *testing.B) {
	answer := strings.Repeat("an ordinary answer line. ", 40)
	atBound := `{"type":"assistant","time":{"completed":2},"tokens":{"input":7,"n":"` +
		strings.Repeat("t", exportValueMaxBytes-64) + `"}}`
	for _, bc := range []struct {
		name    string
		message func(i int) string
	}{
		{"ordinary_steps", func(i int) string {
			return ohStep(ohDone("echo", `{"n":`+fmt.Sprint(i)+`}`, answer))
		}},
		{"empty_messages", func(int) string { return `{}` }},
		{"idle_messages", func(int) string { return `{"type":"idle"}` }},
		{"values_at_the_bound", func(int) string { return atBound }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "export.json.gz")
			benchExportFile(b, file, func(_ *testing.B, w io.Writer) []sandbox.LoggedCall {
				_, _ = io.WriteString(w, `{"info":{"id":"ses_bench"},"messages":[`+ocUser("msg_u", "the brief", `[]`))
				for i, n := 1, 0; n < 255<<20; i++ {
					m := "," + bc.message(i)
					_, _ = io.WriteString(w, m)
					n += len(m)
				}
				_, _ = io.WriteString(w, `]}`)
				return nil
			})
			for _, walk := range []struct {
				name string
				read func(context.Context, io.Reader, *openCodeCountReading) (int64, error)
			}{{"walk", func(ctx context.Context, r io.Reader, c *openCodeCountReading) (int64, error) {
				return countExport(ctx, r, c, nil)
			}}, {"oracle", walkExport}} {
				b.Run(walk.name, func(b *testing.B) {
					fmt.Fprintf(os.Stderr, "export-meter-bench %s/%s: the read starts\n", bc.name, walk.name)
					for b.Loop() {
						f, err := benchOpen(file)
						if err != nil {
							b.Fatal(err)
						}
						var c openCodeCountReading
						_, err = walk.read(b.Context(), f, &c)
						_ = f.Close()
						if err != nil {
							b.Fatal(err)
						}
					}
					fmt.Fprintf(os.Stderr, "export-meter-bench %s/%s: the read ends\n", bc.name, walk.name)
				})
			}
		})
	}
}

// holdingsReadOf is what the reading's own walk read of doc, for ask from
// start ([readOpenCodeHoldingsWhole], no value bound), and what the export
// walk read of it, whole and in pieces of seventeen bytes: the holdings and
// the reading's words.
func holdingsReadOf(t *testing.T, doc, session string, start sandbox.RunStart, ask sandbox.HoldingsAsk) (oracle,
	whole, pieces string) {
	t.Helper()
	read := func(h sandbox.SessionHoldings, err error) string {
		said := "<nil>"
		if own, ok := errors.AsType[*holdingsError](err); ok {
			said = own.why
		} else if err != nil {
			said = err.Error()
		}
		return fmt.Sprintf("%+v %s", h, said)
	}
	oracle = read(readOpenCodeHoldingsWhole(t.Context(), strings.NewReader(doc), math.MaxInt, session, start, ask))
	whole = read(readOpenCodeHoldings(t.Context(), strings.NewReader(doc), session, start, ask))
	pieces = read(readOpenCodeHoldings(t.Context(), &exportPieces{r: strings.NewReader(doc), n: 17}, session, start,
		ask))
	return oracle, whole, pieces
}

// holdingsVariants are steps and messages written every way a reading reads
// apart: a counted part of each outcome and every way OpenCode could hand it
// otherwise, its members in every order and written twice, values past the
// capture's bound, texts empty, blank, escaped, marked and no UTF-8, and
// every message kind, time and content a step may hold.
func holdingsVariants() []string {
	long := strings.Repeat("p", exportValueMaxBytes+8)
	done := ohDone("echo", `{"t":1}`, "done")
	failed := ohFailed("echo", `{"t":1}`, "boom")
	parts := []string{done, failed, ohFailed("echo", `{"t":3}`, ""), ohDone("echo", `{"t":4}`, " \n"),
		ohDone("echo", `{"t":2}`, strings.Repeat("w", 9000)),
		ohDone("echo", `{"t":2}`, strings.Repeat("w", 8000)+"[REDACTED:x]"+strings.Repeat("w", 976)),
		strings.Replace(ohDone("echo", `{"t":1}`, "PLACE"), "PLACE", `\u0064one`, 1),
		strings.Replace(ohDone("echo", `{"t":1}`, "PLACE"), "PLACE", "d\xffne", 1),
		strings.Replace(ohDone("echo", `{"t":1}`, "PLACE"), "PLACE", `\ud800one`, 1),
	}
	for _, edit := range [][2]string{
		{`"truncated":false`, `"truncated":true`}, {`"truncated":false`, `"truncated":false,"x":1`},
		{`"truncated":false`, `"truncated":false,"truncated":false`}, {`,"metadata":{"truncated":false}`, ``},
		{`"metadata":{"truncated":false}`, `"metadata":[]`}, {`{"type":"text","text":"done"}`, `{"type":"file","text":"done"}`},
		{`{"type":"text","text":"done"}`, `{"type":"text","text":"done","mime":"text/plain"}`},
		{`{"type":"text","text":"done"}`, `{"text":"done","type":"text"}`}, {`{"type":"text","text":"done"}`, `7`},
		{`{"type":"text","text":"done"}`, `{"type":"text","text":7}`}, {`[{"type":"text","text":"done"}]`, `[]`},
		{`[{"type":"text","text":"done"}]`, `{"type":"text","text":"done"}`},
		{`{"type":"text","text":"done"}`, `{"type":"text","text":"done"},{"type":"text","text":""}`},
		{`"executed":false`, `"executed":true`}, {`"executed":false`, `"executed":"` + long + `"`},
		{`"executed":false,`, ``}, {`"executed":false`, `"executed":false,"executed":false`},
		{`"crewlet_echo"`, `"execute"`}, {`"crewlet_echo"`, `"crewlet_` + long + `"`}, {`"crewlet_echo"`, `7`},
		{`"crewlet_echo"`, `"crewlet_\u0065cho"`}, {`"status":"completed"`, `"status":"running"`},
		{`"status":"completed"`, `"status":"` + long + `"`}, {`"input":{"t":1}`, `"input":"{\"t\":1}"`},
		{`"input":{"t":1}`, `"input":{"t":1,"p":"` + long + `"}`}, {`"input":{"t":1}`, `"input":{"t":1.0}`},
		{`"time":{"created":1,"ran":2,"completed":3}`, `"time":{"created":1,"queued":2}`},
		{`"time":{"created":1,"ran":2,"completed":3}`, `"time":7`}, {`,"time":{"created":1,"ran":2,"completed":3}`, ``},
		{`"time":{"created":1,"ran":2,"completed":3}`, `"time":{"created":1,"created":1}`},
		{`"state":{`, `"state":{"status":"error"},"state":{`}, {`"type":"tool"`, `"type":"tool","native":{}`},
		{`"type":"tool"`, `"type":"tool","text":"x"`}, {`"type":"tool"`, `"type":"tool","later":1`},
		{`"type":"tool",`, ``}, {`"type":"tool"`, `"type":"` + long + `"`}, {`"type":"tool"`, `"type":7`},
		{`"type":"tool",`, ``},
	} {
		parts = append(parts, strings.Replace(done, edit[0], edit[1], 1))
	}
	for _, edit := range [][2]string{
		{`"tool.execution"`, `"aborted"`}, {`"error":{`, `"content":[],"error":{`},
		{`"error":{`, `"metadata":{"truncated":false},"error":{`}, {`"message":"boom"`, `"message":"boom","x":1`},
		{`"message":"boom"`, `"message":7`}, {`"type":"tool.execution",`, ``}, {`"error":{`, `"error":[{`},
	} {
		parts = append(parts, strings.Replace(failed, edit[0], edit[1], 1))
	}
	// THE STATE'S AND THE ITEM'S MEMBERS IN ANOTHER ORDER: content first, status
	// last, time before state, type last.
	content := `"content":[{"type":"text","text":"done"}],`
	parts = append(parts,
		strings.Replace(strings.Replace(done, content, ``, 1), `"state":{`, `"state":{`+content, 1),
		strings.Replace(strings.Replace(done, `"status":"completed",`, ``, 1), `"truncated":false}`,
			`"truncated":false},"status":"completed"`, 1),
		strings.Replace(strings.Replace(done, `{"type":"tool",`, `{`, 1), `"completed":3}}`,
			`"completed":3},"type":"tool"}`, 1))
	var steps []string
	for _, p := range parts {
		steps = append(steps, ohStep(p))
	}
	step := ohStep(done)
	for _, edit := range [][2]string{
		{`"type":"assistant",`, ``}, {`"type":"assistant"`, `"type":"` + long + `"`}, {`"type":"assistant"`, `"type":7`},
		{`"type":"assistant"`, `"type":"assistant","type":"assistant"`}, {`"cost":0`, `"cost":0,"files":[]`},
		{`"cost":0`, `"cost":0,"files":[],"files":[]`}, {`"cost":0`, `"cost":0,"metadata":{}`},
		{`"cost":0`, `"cost":0,"later":1`}, {`"completed":4`, `"completed":null`}, {`"completed":4`, `"completed":0`},
		{`"completed":4`, `"completed":"4"`}, {`,"completed":4`, ``}, {`"completed":4`, `"completed":1e-400`},
		{`"completed":4`, `"completed":4,"firstToken":5`}, {`"streamed":3`, `"lastToken":3`},
		{`"streamed":3`, `"streamed":3,"streamed":3`}, {`"time":{`, `"time":7,"x":{`},
		{`"content":[`, `"content":[],"content":[`}, {`"content":[`, `"content":[7,`},
		{`"content":[`, `"content":[{"type":"image"},`}, {`"content":[`, `"content":[{"type":7},`},
		{`"content":[`, `"content":[{"text":"x"},`}, {`"content":[`, `"content":[{"type":"reasoning","text":"r"},`},
	} {
		steps = append(steps, strings.Replace(step, edit[0], edit[1], 1))
	}
	// A STEP'S TYPE AFTER ITS CONTENT, as a box may write it.
	steps = append(steps, strings.Replace(strings.Replace(step, `"type":"assistant",`, ``, 1), `"cost":0}`,
		`"cost":0,"type":"assistant"}`, 1),
		strings.Replace(ohStep(), `"content":[]`, `"content":{}`, 1), strings.Replace(ohStep(), `"content":[],`, ``, 1),
		strings.Replace(ohStep(), `"content":[]`, `"content":null`, 1),
		`{"id":"msg_c","type":"compaction","status":"completed","time":{"created":5}}`,
		`{"id":"msg_c","type":"compaction","status":"running","time":{"created":5}}`, ocIdle, `{"type":"later-kind"}`,
		`{"type":"user","id":"msg_user","text":"again"}`, `"words"`, `{}`)
	return steps
}

// holdingsCalls are the asks a variant is read for: each outcome the variants
// write, cut and whole, alone and together.
func holdingsCalls() [][]sandbox.LoggedCall {
	failed := func(c sandbox.LoggedCall) sandbox.LoggedCall {
		c.Failed = true
		return c
	}
	done, boom := ohCall(1, "echo", `{"t":1}`, "done"), failed(ohCall(2, "echo", `{"t":1}`, "boom"))
	cut := ohCut(ohCall(3, "echo", `{"t":2}`, strings.Repeat("w", 9000)))
	empty, blank := failed(ohCall(4, "echo", `{"t":3}`, "")), ohCall(5, "echo", `{"t":4}`, " \n")
	return [][]sandbox.LoggedCall{{done}, {done, boom, cut, empty, blank}, {cut}, {empty}}
}

// A HOLDINGS READING READS AN EXPORT AS ITS OWN WALK DID: over every export of
// the corpus and documents of every holdings variant, read for each ask from
// the session's start and from a message's, the reading on the export walk,
// whole and in pieces of seventeen bytes, answers the holdings and the words
// the decode walk's reading answered ([readOpenCodeHoldingsWhole]), but for
// the answers the export walk moves, each its own case below.
//
// Mutation: read a capture past its bound as absent, decide a part before its
// state is read, count a part of a message whose kind comes last, or match a
// class of size zero, and a case goes red.
func TestAHoldingsReadingReadsAnExportAsItsWalkDid(t *testing.T) {
	t.Parallel()
	asks := holdingsCalls()
	starts := []sandbox.RunStart{ohBegan, {After: "msg_user"}}
	check := func(name, doc, session string, start sandbox.RunStart, calls []sandbox.LoggedCall) {
		oracle, whole, pieces := holdingsReadOf(t, doc, session, start,
			sandbox.HoldingsAsk{Calls: calls, Resumer: "2.3.2"})
		if whole != oracle || pieces != oracle {
			t.Errorf("%s %.200q:\nwalk   %.400s\npieces %.400s\nwant   %.400s", name, doc, whole, pieces, oracle)
		}
	}
	for _, d := range exportCorpus(t) {
		check(d.name, d.doc, ohSession, ohBegan, asks[1])
	}
	for _, release := range openCodeHoldingsReleases {
		for name, f := range ohFixtures(t, release) {
			zr, err := gzip.NewReader(bytes.NewReader(f.archive))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			check(release+" "+name, string(doc), f.session, sandbox.RunStart{Began: f.began}, f.calls)
		}
	}
	r := rand.New(rand.NewPCG(1170, 7))
	variants := holdingsVariants()
	docs := []string{}
	for _, v := range variants {
		docs = append(docs, ohDoc(ohUser, v))
	}
	for range 400 {
		messages := []string{ohUser}
		for range 1 + r.IntN(4) {
			messages = append(messages, variants[r.IntN(len(variants))])
		}
		doc := ohDoc(messages...)
		switch r.IntN(10) {
		case 0:
			doc = doc[:r.IntN(len(doc))]
		case 1:
			doc += ` {}`
		case 2:
			doc = strings.Replace(doc, `"info":{`, `"info":{"id":"ses_other","id":`+ohQuote(ohSession)+`,"x":`, 1)
		case 3:
			doc = strings.Replace(doc, `"messages":[`, `"after":1,"messages":[`, 1) + "\n\t "
		}
		docs = append(docs, doc)
	}
	for _, doc := range docs {
		for _, start := range starts {
			for _, calls := range asks {
				check("variant", doc, ohSession, start, calls)
			}
		}
	}
}

// A TEXT'S DIGEST IS ITS WHOLE TEXT'S, WHEREVER ITS PIECES END: written a
// piece at a time, whole characters each, as the walk hands it, in pieces of
// every length, a text's digest answers its size, its sum, the sum of every
// opening it reaches, whether it holds the scrub's marker and whether it is
// blank exactly as the text read whole does. A class's opening is at most
// [sandbox.MaxBridgeOutputBytes], inside the walk's first piece, so only here
// does an opening's sum cross an edge.
//
// Mutation: read the marker within each piece alone or off the last piece's
// tail alone, judge blanks by the last piece, or reach an opening only past
// it, and a case goes red.
func TestATextsDigestIsItsWholeTextsWhereverItsPiecesEnd(t *testing.T) {
	t.Parallel()
	texts := []string{"", " ", "words", " \t\n\v\f\r\u0085 　 ", " 　　x", "x　 \n ", "é€😀 ok"}
	for at := range len(redact.Marker) + 2 {
		texts = append(texts, strings.Repeat("w", at)+redact.Marker+"x]www",
			strings.Repeat("w", at)+redact.Marker[:len(redact.Marker)-1]+"www")
	}
	openings := []int{0, 1, 7, 13, 26}
	for _, text := range texts {
		for size := range max(1, len(text)) {
			d := newTextDigest(openings)
			d.reset()
			for rest := text; rest != ""; {
				n := 0
				for n < len(rest) {
					_, width := utf8.DecodeRuneInString(rest[n:])
					if n > 0 && n+width > size+1 {
						break
					}
					n += width
				}
				d.write([]byte(rest[:n]))
				rest = rest[n:]
			}
			d.end()
			if d.n != len(text) || d.whole != sha256.Sum256([]byte(text)) ||
				d.marker != strings.Contains(text, redact.Marker) || d.blank != (strings.TrimSpace(text) == "") {
				t.Errorf("%q in pieces of %d bytes: %d bytes, marked %t, blank %t, its sum %t", text, size+1, d.n,
					d.marker, d.blank, d.whole == sha256.Sum256([]byte(text)))
			}
			for i, opening := range openings {
				if reached := i < d.next; reached != (opening <= len(text)) ||
					reached && d.sums[i] != sha256.Sum256([]byte(text[:opening])) {
					t.Errorf("%q in pieces of %d bytes: the opening of %d reached %t, its sum %x", text, size+1,
						opening, reached, d.sums[i][:4])
				}
			}
		}
	}
}

// THE EXPORT WALK MOVES TWO OF A READING'S ANSWERS, toward telling a call and
// toward reading one: a step's `completed` written as a number literal past
// the capture's bound is no reading, where the decode walk parsed it; and a
// value past the decode walk's value bound (here lowered to 4 KiB), a message
// or any member of the document (`info`'s title, a key the import passes), is
// read, where that walk read none.
//
// Mutation: parse a `completed` past its capture, or bound a message or a
// member the reading passes, and a case goes red.
func TestTheExportWalkMovesAReadingsAnswersOnlyAsListed(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "2.3.2"}
	literal := `"completed":4.` + strings.Repeat("0", exportValueMaxBytes)
	past := ohDoc(ohUser, strings.Replace(ohStep(ohDone("echo", `{"t":1}`, "done")), `"completed":4`, literal, 1))
	if h, err := readOpenCodeHoldingsWhole(t.Context(), strings.NewReader(past), math.MaxInt, ohSession, ohBegan,
		ask); !slices.Equal(ohHeld(t, h, err, c), []int{1}) {
		t.Fatal("the decode walk holds no call of a step whose completed is past the bound; the case moves nothing")
	}
	if h, err := readOpenCodeHoldings(t.Context(), strings.NewReader(past), ohSession, ohBegan, ask); err == nil ||
		h.Read {
		t.Errorf("a completed past the capture's bound: a reading = %+v; want none", h)
	}
	doc, value := ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "done"))), strings.Repeat("v", exportValueMaxBytes+1)
	for name, member := range map[string][2]string{
		"info's title":                   {`"title":"a run"`, `"title":"` + value + `"`},
		"another member of the document": {`"messages":[`, `"later":"` + value + `","messages":[`},
	} {
		edited := strings.Replace(doc, member[0], member[1], 1)
		if _, err := readOpenCodeHoldingsWhole(t.Context(), strings.NewReader(edited), 4<<10, ohSession, ohBegan,
			ask); err == nil {
			t.Fatalf("the decode walk read %s past its bound; the case moves nothing", name)
		}
		h, err := readOpenCodeHoldings(t.Context(), strings.NewReader(edited), ohSession, ohBegan, ask)
		if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
			t.Errorf("%s past the decode walk's bound: held %v; want its call", name, got)
		}
	}
	long := ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, strings.Repeat("l", 8<<10))))
	long8 := ohCall(1, "echo", `{"t":1}`, strings.Repeat("l", 8<<10))
	ask = sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{long8}, Resumer: "2.3.2"}
	if _, err := readOpenCodeHoldingsWhole(t.Context(), strings.NewReader(long), 4<<10, ohSession, ohBegan,
		ask); err == nil {
		t.Fatal("the decode walk read a message past its bound; the case moves nothing")
	}
	h, err := readOpenCodeHoldings(t.Context(), strings.NewReader(long), ohSession, ohBegan, ask)
	if got := ohHeld(t, h, err, long8); !slices.Equal(got, []int{1}) {
		t.Errorf("a message past the decode walk's bound: held %v; want its call", got)
	}
}

// wholeOpenCode reads an OpenCode archive by the decode walk, the person's
// oracle ([transcriptBuilder.openCodeExportWhole]).
type wholeOpenCode struct{ OpenCode }

func (wholeOpenCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.openCodeExportWhole(ctx, archive)
}

// piecesOpenCode reads an OpenCode archive on the export walk, its inflated
// bytes handed n a read.
type piecesOpenCode struct {
	OpenCode
	n int
}

func (p piecesOpenCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.openCodeExport(ctx, &exportPieces{r: archive, n: p.n})
}

// personRead is one way a person's read of an export is asked for.
type personRead struct {
	from     int
	bounds   TranscriptBounds
	filter   TranscriptFilter
	valueMax int
	refuse   bool
}

// personReadOf is what c reads of doc as r asks, as JSON: the page and the
// error's words.
func personReadOf(t *testing.T, c carrier, doc []byte, r personRead) string {
	t.Helper()
	b := newTranscriptBuilder(r.from, r.bounds, r.filter)
	b.valueMax = r.valueMax
	if r.refuse {
		b.redactor = func(io.Writer) entryRedaction { return &refusingRedaction{} }
	}
	page, err := b.decode(t.Context(), c, bytes.NewReader(doc))
	said, encodeErr := json.Marshal(page)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	return fmt.Sprintf("%s %v", said, err)
}

// personVariants are messages written every way a person's read reads apart
// past a value bound of a kibibyte: a brief opening with a heading past it and
// within it, blank texts past it, a kind written last, a call's input as an
// object and as a string past it, answers whose parts, joined or each, run past
// it, a failure's words and an answer joined past it, blank parts past it, a failure's words past it, states written in order
// and out of it, a key written twice, each member a result is read by written
// twice around a value past it and another member written twice beside one, a
// tool's name past it, an image's mime past it, and credentials across the
// bound.
func personVariants() []string {
	q := func(s string) string {
		raw, err := json.Marshal(s)
		if err != nil {
			panic(err)
		}
		return string(raw)
	}
	w := strings.Repeat("words of the model, ", 100)
	note := noteHeadOpen + "ada" + noteHeadClose + "\nTarget main.\n"
	blank := strings.Repeat(" \n", 1000)
	secret := redacttest.Shapes()[0].Sample
	part := func(text string) string { return `{"type":"text","text":` + q(text) + `}` }
	tool := func(state string) string {
		return `{"type":"tool","name":"bash","time":{"created":1,"ran":2,"completed":3},"state":` + state + `}`
	}
	step := func(items ...string) string {
		return `{"type":"assistant","time":{"created":5},"content":[` + strings.Join(items, ",") + `]}`
	}
	return []string{
		`{"type":"user","text":` + q(note+w) + `,"time":{"created":1}}`,
		`{"text":` + q(note+w) + `,"time":{"created":1},"type":"user"}`,
		`{"type":"user","text":` + q(note+lateCallHead+"\nit failed") + `}`,
		`{"type":"user","text":` + q(blank) + `}`,
		`{"type":"user","text":` + q(strings.Repeat("x", 1010)+secret+w) + `}`,
		`{"type":"compaction","summary":` + q(w) + `,"status":"completed"}`,
		`{"summary":` + q(w) + `,"status":"completed","type":"compaction","time":{"created":9}}`,
		step(part(w), part(blank), `{"text":`+q(w)+`,"type":"text"}`, `{"type":"reasoning","text":`+q(w)+`}`),
		step(tool(`{"status":"completed","input":{"command":` + q(w) + `},"content":[` + part(w) + `,` + part(w) + `]}`)),
		step(tool(`{"status":"completed","input":` + q(`{"a": 1, "b": "`+w+`"}`) + `,"content":[]}`)),
		step(tool(`{"status":"completed","input":` + q(`{"a": 1}`) + `}`)),
		step(tool(`{"status":"completed","input":null,"content":[` + part(blank) + `,` + part(" ") +
			`,{"type":"file","mime":"image/png"},` + part("ok") + `]}`)),
		step(tool(`{"status":"error","input":{},"error":{"type":"tool.execution","message":` + q(w) + `}}`)),
		step(tool(`{"status":"error","error":{"type":"tool.execution","message":` + q(w) + `},"content":[` +
			part(w) + `]}`)),
		step(tool(`{"status":"error","content":[` + part(w) + `],"error":{"type":"tool.execution","message":"boom"}}`)),
		step(tool(`{"content":[` + part(w) + `],"status":"completed"}`)),
		step(tool(`{"status":"completed","content":[{"text":` + q(w) + `,"type":"text"}]}`)),
		step(tool(`{"status":"error","error":{"type":` + q(w) + `,"message":""}}`)),
		step(tool(`{"status":"error","error":{"type":` + q(w) + `,"message":"boom"}}`)),
		step(tool(`{"status":"completed","content":[` + part(w) + `],"content":[` + part("again") + `]}`)),
		step(tool(`{"status":"completed","content":[` + part(strings.Repeat("x", 600)) + `,` +
			part(strings.Repeat("y", 600)) + `]}`)),
		step(tool(`{"status":"error","error":{"message":` + q(strings.Repeat("m", 600)) + `},"content":[` +
			part(strings.Repeat("y", 600)) + `]}`)),
		step(tool(`{"status":"error","error":{"type":"x","message":` + q(w) + `},"error":{"message":"two"}}`)),
		step(tool(`{"status":"error","error":{"message":` + q(strings.Repeat("a", 300)) + `},"content":[` +
			part(strings.Repeat("b", 300)) + `,` + part(strings.Repeat("c", 300)) + `,` +
			part(strings.Repeat("d", 300)) + `]}`)),
		step(tool(`{"status":"error","error":{"type":` + q(strings.Repeat("t", 600)) + `},"content":[` +
			part(strings.Repeat("y", 600)) + `]}`)),
		step(tool(`{"status":"error","content":[` + part(strings.Repeat("y", 600)) + `],"error":{"message":` +
			q(strings.Repeat("m", 600)) + `}}`)),
		step(tool(`{"error":{"message":` + q(strings.Repeat("m", 600)) + `},"content":[` +
			part(strings.Repeat("y", 600)) + `],"status":"error"}`)),
		step(tool(`{"status":"error","error":{"message":` + q(strings.Repeat("m", 600)) + `},"content":[` +
			part(strings.Repeat("y", 600)) + `,{"text":"z","type":"text"}]}`)),
		step(tool(`{"status":"error","content":[` + part("ok") + `],"error":{"message":"boom"}}`)),
		step(tool(`{"status":"completed","status":"completed","content":[` + part(w) + `]}`)),
		step(tool(`{"status":"completed","content":[` + part(w) + `],"status":"completed"}`)),
		step(tool(`{"status":"error","error":{"message":` + q(w) + `,"message":"x"}}`)),
		step(tool(`{"status":"error","error":{"message":"x","message":` + q(w) + `}}`)),
		step(tool(`{"status":"error","error":{"message":` + q(w) + `,"message":"x"},"content":[` + part("on") + `]}`)),
		step(tool(`{"status":"error","error":{"type":` + q(w) + `,"type":"x"}}`)),
		step(tool(`{"status":"error","error":{"type":"a","message":` + q(w) + `,"type":"b"}}`)),
		step(tool(`{"status":"completed","content":[` + part(w) + `,{"type":"file","mime":"image/png","mime":"x"}]}`)),
		step(tool(`{"status":"completed","content":[{"type":"text","text":` + q(w) + `,"text":"x"},` + part("on") + `]}`)),
		step(tool(`{"status":"completed","content":[{"type":"text","text":"x","text":` + q(w) + `}]}`)),
		step(tool(`{"status":"completed","content":[{"type":"text","text":` + q(w) + `,"type":"text"}]}`)),
		step(tool(`{"status":"completed","content":[{"type":"text","id":"a","id":"b","text":` + q(w) + `}]}`)),
		step(tool(`{"status":"error","error":{"id":"a","id":"b","message":` + q(w) + `}}`)),
		step(tool(`{"status":"completed","title":"a","title":"b","content":[` + part(w) + `]}`)),
		step(tool(`{"status":"running","content":[` + part(w) + `]}`)),
		step(tool(`{"status":"completed","content":[{"type":"file","mime":` + q("image/"+w) + `}]}`)),
		step(tool(`{"status":"completed","content":[` + part(strings.Repeat("y", 1020)+secret+" and more") + `]}`)),
		`{"type":"assistant","content":[{"type":"tool","name":` + q(w) + `,"state":{"status":"completed",` +
			`"content":[` + part("ok") + `]}}]}`,
		`{"type":"assistant","content":[{"state":{"status":"completed","input":{"x":` + q(w) + `}},"type":"tool"}]}`,
		step(tool(`{"status":"completed","input":"not JSON at all ` + w + `"}`)),
	}
}

// A PERSON'S READ READS AN EXPORT AS ITS DECODE WALK DID: over the walk's
// corpus and every person variant, the read on the export walk, whole and in
// pieces of seventeen bytes, answers the page and the error the decode walk
// answers ([transcriptBuilder.openCodeExportWhole]), which holds each message
// whole and applies R1 to R6 and the lines bound where it decides them: from
// the start and later, at a person's bounds and a seat's, under filters, at the
// value bound (8 MiB) and below, with every redaction refused, and past the
// lines bound in a step's items, an answer's parts and a non-message.
//
// Mutation: split a brief past the bound, drop a blank part past it, re-read a
// string input past it as JSON, stream a result whose state is out of order,
// show a refused redaction, or drop a lines bound, and a case goes red.
func TestAPersonsReadReadsAnExportAsItsDecodeWalkDid(t *testing.T) {
	t.Parallel()
	person, seat := TranscriptReadBounds(), SeatTranscriptBounds()
	calls := TranscriptFilter{Kinds: []EntryKind{EntryToolCall, EntryToolResult}}
	terms := TranscriptFilter{Terms: []string{"words"}}
	wide := []personRead{{bounds: person, valueMax: TranscriptValueMaxBytes}, {bounds: person, valueMax: 1 << 10},
		{bounds: seat, valueMax: 1 << 10, from: 2, filter: calls}}
	every := append(wide, personRead{bounds: person, valueMax: 1 << 10, filter: terms},
		personRead{bounds: seat, valueMax: 512, filter: terms, from: 1},
		personRead{bounds: person, valueMax: 1 << 10, refuse: true},
		personRead{bounds: person, valueMax: 1 << 10, from: 1000},
		personRead{bounds: person, valueMax: 1 << 10, filter: TranscriptFilter{Kinds: []EntryKind{EntryModel}}})
	check := func(name string, doc string, reads []personRead) {
		archive := gzipOf(t, doc)
		for _, r := range reads {
			want := personReadOf(t, wholeOpenCode{}, archive, r)
			if got := personReadOf(t, OpenCode{}, archive, r); got != want {
				t.Errorf("%s %.200q read %+v:\nwalk   %.600s\nwant   %.600s", name, doc, r, got, want)
			}
			if got := personReadOf(t, piecesOpenCode{n: 17}, archive, r); got != want {
				t.Errorf("%s %.200q read %+v in pieces:\nwalk   %.600s\nwant   %.600s", name, doc, r, got, want)
			}
		}
	}
	for _, d := range exportCorpus(t) {
		check(d.name, d.doc, wide)
	}
	variants := personVariants()
	for _, v := range variants {
		check("a variant", `{"info":{"id":"ses_x"},"messages":[`+ocUser("msg_u", "the brief", `[]`)+`,`+v+`]}`,
			every)
	}
	check("every variant", `{"messages":[`+strings.Join(variants, ",")+`]}`, every)
	// Past the lines bound inside a step's items and an answer's parts, and,
	// after a brief and a step framing the bound exactly or one short of it, at
	// an element that is no message.
	empties := func(n int) string { return strings.Repeat(`{},`, n-1) + `{}` }
	brief := `{"type":"user","text":"the brief"}`
	edge := func(items int) string {
		return `{"messages":[` + brief + `,{"type":"assistant","content":[` + empties(items-1) +
			`,{"type":"text","text":"x"}]},0,` + brief + `]}`
	}
	for _, doc := range []string{
		`{"messages":[` + brief + `,{"type":"assistant","content":[{"type":"text","text":"before"},` +
			empties(TranscriptReadLines) + `,{"type":"text","text":"after"}]},` + brief + `]}`,
		`{"messages":[` + brief + `,{"type":"assistant","content":[{"type":"tool","name":"bash","state":` +
			`{"status":"completed","content":[` + empties(TranscriptReadLines) + `]}}]},` + brief + `]}`,
		edge(TranscriptReadLines - 2), edge(TranscriptReadLines - 3),
	} {
		check("past the lines bound", doc, []personRead{{bounds: person, valueMax: 1 << 10}})
	}
}

// A PERSON'S READ OF ANY EXPORT IS ITS DECODE WALK'S: whatever document the
// export walk is handed, at value bounds of 64 bytes and a kibibyte, unfiltered
// and filtered by a term and by kind, it reads it without failing on its own,
// and where both it and the decode walk read it to a page, the page is the
// decode walk's. Seeded with every person variant, alone and all together.
//
// Mutation: let a result's stream go without the member it was taking, and a
// seed with a failure's message written twice fails.
func FuzzAPersonsReadOfAnyExportIsItsDecodeWalks(f *testing.F) {
	variants := personVariants()
	for _, v := range variants {
		f.Add(`{"messages":[` + v + `]}`)
	}
	f.Add(`{"messages":[` + strings.Join(variants, ",") + `]}`)
	// read is what one walk reads of doc, and whether it read it to a page.
	read := func(ctx context.Context, doc string, valueMax int, filter TranscriptFilter, whole bool) (string, bool) {
		b := newTranscriptBuilder(0, TranscriptReadBounds(), filter)
		b.valueMax = valueMax
		walk := b.openCodeExport
		if whole {
			walk = b.openCodeExportWhole
		}
		err := walk(ctx, strings.NewReader(doc))
		return fmt.Sprint(b.entries, b.places, b.next, b.cut, err), err == nil || errors.Is(err, errTranscriptPage)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		for _, valueMax := range []int{64, 1 << 10} {
			for _, filter := range []TranscriptFilter{{}, {Terms: []string{"x"}}, {Kinds: []EntryKind{EntryToolResult}}} {
				got, walked := read(t.Context(), doc, valueMax, filter, false)
				want, decoded := read(t.Context(), doc, valueMax, filter, true)
				if walked && decoded && got != want {
					t.Fatalf("at %d under %+v:\nwalk   %.800s\nwant   %.800s", valueMax, filter, got, want)
				}
			}
		}
	})
}
