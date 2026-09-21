package engine_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
)

// danglingCompanyDoc applies cleanly and carries two dangling references: a
// division lead nobody holds, which its child unit inherits, and a manages
// entry naming nobody. The embeddings block is what lets the same document
// be REFUSED at build (see danglingRefusedDoc) while still passing
// validation, so the refused case exercises the apply rather than the parse.
const danglingCompanyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
  embeddings:
    type: openai
    model: text-embedding-3-large
    api_key: ${K}
    dimensions: 3072
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    manages: [engineering, nobody]
units:
  - name: Engineering
    id: engineering
    lead: ghost
    roles:
      - name: Dev
        handle: dev
        llm: zulu
    children:
      - name: Backend
        roles:
          - name: Dev B
            handle: dev-b
            llm: zulu
`

// danglingRefusedDoc is the same company asking for a vector width the
// node's store did not open at. It validates, so it reaches Engine.Apply, and
// the apply refuses it on every attempt: a revision this node retries and
// never serves.
var danglingRefusedDoc = strings.Replace(danglingCompanyDoc, "dimensions: 3072", "dimensions: 1536", 1)

// danglingLine is the part of one org_dangling_reference line a test asserts.
type danglingLine struct {
	Epoch int64  `json:"epoch"`
	Ref   string `json:"ref"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// danglingLines decodes every org_dangling_reference line the reconciler
// wrote, ignoring anything else, and flags a line missing its detail: a
// warning with no message says what is wrong but not what to do.
func danglingLines(t *testing.T, logs *bytes.Buffer) []danglingLine {
	t.Helper()
	var out []danglingLine
	scanner := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for scanner.Scan() {
		var line struct {
			Msg    string `json:"msg"`
			Level  string `json:"level"`
			Detail string `json:"detail"`
			danglingLine
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("decode log line %q: %v", scanner.Text(), err)
		}
		if line.Msg != "org_dangling_reference" {
			continue
		}
		if line.Level != slog.LevelWarn.String() || line.Detail == "" {
			t.Errorf("line %q: want a warning carrying a detail", scanner.Text())
		}
		out = append(out, line.danglingLine)
	}
	return out
}

// TestDanglingReferencesAreLoggedOncePerAppliedEpoch pins where the warning
// is emitted, which is the whole contract: once per node per epoch it
// applies. Not on every tick (the loop runs for the life of the node), not on
// a refused attempt that is retried for a revision this node is not serving,
// and again for a re-activation, because that is a new epoch the node
// rebuilds from. A lead inherited by a child unit is one line, on the unit
// that wrote it.
func TestDanglingReferencesAreLoggedOncePerAppliedEpoch(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	// THE REFERENCES ARE ON THE CHART, which is where they live: a lead and
	// a `manages:` entry name a seat, and seats are the chart's. So the
	// engine boots on this document and its seed puts the dangling ones on
	// the log — the revisions activated below carry the SETTINGS, and a
	// settings document has nothing to resolve.
	p := planeFor(t, newEngine(t, engine.Options{
		Company: parsedCompany(t, danglingCompanyDoc),
	}), func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	})

	first := p.activate(t.Context(), t, danglingCompanyDoc)
	for range 3 {
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	want := []danglingLine{
		{Epoch: first, Ref: "lead", From: "Engineering", To: "ghost"},
		{Epoch: first, Ref: "manages", From: "CEO", To: "nobody"},
	}
	if got := danglingLines(t, &logs); !slices.Equal(got, want) {
		t.Fatalf("after three ticks on one epoch, logged %+v, want %+v", got, want)
	}

	second := p.activate(t.Context(), t, danglingCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	want = append(want,
		danglingLine{Epoch: second, Ref: "lead", From: "Engineering", To: "ghost"},
		danglingLine{Epoch: second, Ref: "manages", From: "CEO", To: "nobody"},
	)
	if got := danglingLines(t, &logs); !slices.Equal(got, want) {
		t.Fatalf("after a re-activation, logged %+v, want %+v", got, want)
	}

	p.activate(t.Context(), t, danglingRefusedDoc)
	for range configplane.MaxApplyAttempts {
		if err := p.recon.Tick(t.Context()); err == nil {
			t.Fatal("a revision resizing the store's vectors applied cleanly")
		}
	}
	if p.recon.Applied() != second {
		t.Fatalf("applied = %d, want the last good epoch %d", p.recon.Applied(), second)
	}
	if got := danglingLines(t, &logs); !slices.Equal(got, want) {
		t.Errorf("a refused revision logged its references: %+v, want only %+v", got, want)
	}
}
