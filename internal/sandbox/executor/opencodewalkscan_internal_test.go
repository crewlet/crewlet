package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonscan"
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
	for _, path := range []string{"2.0.32/export.json", "2.2.5/export.json", "2.3.0/text.json", "2.3.0/tool.json"} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, exportDoc{path, string(raw)})
	}
	for _, release := range []string{"2.0.32", "2.1.4", "2.2.7", "2.3.0"} {
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
	figure, err = countExport(t.Context(), bytes.NewReader(gzipOf(t, doc)), &c)
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
			}{{"walk", countExport}, {"oracle", walkExport}} {
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
