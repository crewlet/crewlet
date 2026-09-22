package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// splitFile is a company document carrying both halves, plus a comment and a
// ${VAR} the settings half has to keep.
const splitFile = `# Nimbus, the company.
name: Nimbus
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_KEY}"]   # a pointer, stored verbatim
units:
  - name: Engineering
    id: engineering
    lead: cto
    roles:
      - name: CTO
        handle: cto
        llm: zulu
roles:
  - name: Ops
    handle: ops
    llm: zulu
`

// THE SETTINGS HALF IS THE FILE MINUS TWO KEYS, and nothing else.
//
// `PUT /config` refuses a body carrying `roles:` or `units:` by name, so the
// command divides the file — and it divides the NODE TREE rather than
// re-encoding the parsed value, because Tier B's secrets are `${VAR}`
// pointers stored verbatim and a round trip through the Go types would drop
// every comment and re-quote every scalar.
func TestTheSettingsHalfIsTheFileMinusTheChartKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(splitFile), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := settingsHalfOf(path)
	if err != nil {
		t.Fatalf("settingsHalfOf: %v", err)
	}
	for _, gone := range config.ChartKeys() {
		if strings.Contains(string(got), "\n"+gone+":") {
			t.Errorf("the settings half still carries %q:\n%s", gone, got)
		}
	}
	// AND EVERYTHING ELSE SURVIVED, which is the control: a half that
	// dropped the providers would pass every assertion above.
	for _, kept := range []string{
		"name: Nimbus", "zulu", "${ANTHROPIC_KEY}", "a pointer, stored verbatim",
	} {
		if !strings.Contains(string(got), kept) {
			t.Errorf("the settings half lost %q:\n%s", kept, got)
		}
	}
	// AND IT IS STILL A COMPANY. A half that no longer parses would be
	// refused at the node, after the operator had been told nothing.
	if _, err := config.ParseCompany(got); err != nil {
		t.Errorf("the settings half does not parse: %v", err)
	}
}

// A FILE WITH NO CHART IS UNTOUCHED, byte for byte.
//
// Re-encoding one that needed no change would rewrite a document the operator
// authored for no reason at all, and the diff they read afterwards would be
// of a file nobody wrote.
func TestAFileWithNoChartIsNotRewritten(t *testing.T) {
	t.Parallel()
	const settingsOnly = "name: Nimbus\n# nothing else\n"
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(settingsOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := settingsHalfOf(path)
	if err != nil {
		t.Fatalf("settingsHalfOf: %v", err)
	}
	if string(got) != settingsOnly {
		t.Errorf("a file with no chart was rewritten:\n got %q\nwant %q",
			got, settingsOnly)
	}
}

// recordingNode is a node that accepts both surfaces and remembers what
// reached each.
type recordingNode struct {
	mu       sync.Mutex
	settings []byte
	paths    []string
	imported map[string]any
	server   *httptest.Server
}

func newRecordingNode(t *testing.T) *recordingNode {
	t.Helper()
	n := &recordingNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /config", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		n.mu.Lock()
		n.settings, n.paths = body, append(n.paths, "PUT /config")
		n.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"revision_id":"rev-1","epoch":7}`))
	})
	mux.HandleFunc("POST /chart/import", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		n.mu.Lock()
		n.paths = append(n.paths, "POST /chart/import")
		_ = json.Unmarshal(body, &n.imported)
		n.mu.Unlock()
		_, _ = w.Write([]byte(`{"outcome":"applied","position":"CHART@1:4"}`))
	})
	mux.HandleFunc("PATCH /chart/units/{key}", n.record("PATCH /chart/units/"))
	mux.HandleFunc("PATCH /chart/seats/{handle}", n.record("PATCH /chart/seats/"))
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

func (n *recordingNode) record(prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.paths = append(n.paths, prefix+r.PathValue("key")+r.PathValue("handle"))
		n.mu.Unlock()
		_, _ = w.Write([]byte(`{"outcome":"applied","position":"CHART@1:5"}`))
	}
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// ONE FILE REACHES TWO SURFACES, IN THE ORDER THAT WORKS.
//
// The settings land first, because a seat whose model chain names a provider
// is only valid once that provider exists. Then the STRUCTURE, as one record
// — because a content write states the unit it believes a seat sits in, and
// the domain refuses a value that disagrees with the row, so content written
// before its placement names a unit no row has yet. Then each object's own
// content.
func TestOneFileReachesBothSurfacesInOrder(t *testing.T) {
	// NOT PARALLEL: it sets the API token in the environment, which is
	// process-wide.
	node := newRecordingNode(t)
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(splitFile), 0o600); err != nil {
		t.Fatal(err)
	}
	company, err := config.LoadCompany(path)
	if err != nil {
		t.Fatalf("LoadCompany: %v", err)
	}
	// A TOKEN, because both clients refuse a node they cannot
	// authenticate to — which is the right refusal and not what this case
	// is about.
	t.Setenv("CREWLET_API_TOKEN", "test-token")
	var out bytes.Buffer
	boot := config.DefaultBootstrap()
	if err := importThroughNode(context.Background(), &boot,
		importTarget{path: path, apiURL: node.server.URL},
		company, "import Nimbus", &out); err != nil {
		t.Fatalf("importThroughNode: %v", err)
	}

	node.mu.Lock()
	defer node.mu.Unlock()
	// THE TWO ORDERED STEPS FIRST, and they are the only two the order of
	// matters for. Which object's content goes next does not: a content
	// record is arbitrated on that object's own subject, so the writes
	// after the structure are independent of each other.
	if len(node.paths) < 2 || node.paths[0] != "PUT /config" ||
		node.paths[1] != "POST /chart/import" {

		t.Fatalf("reached %v, want the settings then the structure", node.paths)
	}
	content := node.paths[2:]
	for _, want := range []string{
		"PATCH /chart/units/engineering",
		"PATCH /chart/seats/cto", "PATCH /chart/seats/ops",
	} {
		if !slices.Contains(content, want) {
			t.Errorf("%q was never written (all: %v)", want, node.paths)
		}
	}
	if len(content) != 3 {
		t.Errorf("wrote %v, want exactly one record per object", content)
	}
	// THE CHART DID NOT REACH /config, which is the refusal this split
	// exists to avoid: a body carrying one is refused whole, by name.
	if bytes.Contains(node.settings, []byte("\nroles:")) ||
		bytes.Contains(node.settings, []byte("\nunits:")) {

		t.Errorf("the chart reached the settings surface:\n%s", node.settings)
	}
	// AND THE IMPORT IS KEYED, so a second run of the same file is a no-op
	// every node reaches the same way rather than a rewrite of every row.
	if rev, _ := node.imported["revision"].(string); !strings.HasPrefix(rev, "file:") {
		t.Errorf("the import is keyed on %q, want the chart's own content hash", rev)
	}
}

// answeringChart is a chart client whose node answers every request the same
// way, for the refusal cases.
func answeringChart(t *testing.T, status int, raw []byte) *chartClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	return &chartClient{base: server.URL, http: httpx.Client(apiTimeout)}
}

// A REFUSAL IS RENDERED AS WHAT THE NODE SAID.
//
// Every refusal on that surface names the rule it broke — a seat that moved,
// an address somebody holds, a fleet mid-upgrade — and a client that printed
// the status alone would throw away the one sentence that says what to do.
func TestAChartRefusalCarriesTheNodesOwnSentence(t *testing.T) {
	t.Parallel()
	client := answeringChart(t, http.StatusConflict, []byte(
		`{"error":"fleet_mixed_version","detail":"node-2 is still running protocol 3"}`))
	_, err := client.ImportStructure(context.Background(), "file:abc", nil)
	if err == nil {
		t.Fatal("a refusal was reported as a success")
	}
	if !strings.Contains(err.Error(), "node-2 is still running protocol 3") {
		t.Errorf("the error drops what the node said: %v", err)
	}
}
