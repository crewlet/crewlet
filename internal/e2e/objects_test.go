package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A company's files, end to end.
//
// The unit suites certify each half against fakes — the chunk transfer over
// the in-memory broker, the passes over real disks, the tracker's file rows
// over a real log. What only a fleet can show is the claim the object store
// exists for: the row naming a file reaches every node through the log while
// the bytes are placed, so a file uploaded on one node is served, whole and
// verified, by another.

// e2eOperatorID and e2eOperatorToken are the credential a cluster member's
// API accepts writes under.
const (
	e2eOperatorID    = "e2e-ops"
	e2eOperatorToken = "e2e-operator-token-0123456789"
)

// upload PUTs a file through a node's byte route.
func upload(t *testing.T, n *node, project, path string, content []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		fmt.Sprintf("%s/work/files/%s/%s", n.server.URL, project, path), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e2eOperatorToken)
	req.Header.Set("Content-Type", "text/csv")
	resp, err := n.server.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// download GETs a file's bytes through a node's byte route.
func download(t *testing.T, n *node, project, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("%s/work/files/%s/%s", n.server.URL, project, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := n.server.Client().Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the download: %v", err)
	}
	return resp.StatusCode, got
}

// A FILE UPLOADED TO ONE NODE DOWNLOADS FROM ANOTHER, byte for byte.
//
// Several chunks, so the download reassembles a manifest rather than handing
// back one message; and from the member the upload did not touch, so what it
// serves came through the log (the row) and the object store (the bytes)
// rather than from anything the first member still holds in memory.
func TestAFileUploadedToOneNodeDownloadsFromAnother(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)
	content := bytes.Repeat([]byte("region,quarter,revenue\nemea,q3,1200\n"), 90_000)
	if len(content) <= 2*objstore.ChunkSize {
		t.Fatalf("the fixture is %d bytes, under three chunks", len(content))
	}

	// RETRIED UNTIL STORED: the first placement map is written by whichever
	// member claims the map duty on its first tick, and an upload before it
	// is refused as unavailable rather than stored nowhere.
	var answer map[string]any
	waitFor(t, "an upload to be stored", func() bool {
		status, body := upload(t, c.nodes[0], "ENG", "reports/q3.csv", content)
		answer = body
		return status == http.StatusOK
	})
	if answer["outcome"] != string(statelog.OutcomeApplied) {
		t.Fatalf("the upload answered %v", answer)
	}

	waitFor(t, "the other member to serve the file", func() bool {
		status, got := download(t, c.nodes[1], "ENG", "reports/q3.csv")
		return status == http.StatusOK && bytes.Equal(got, content)
	})
	detail, err := c.nodes[1].engine.Tracker().File(t.Context(), "ENG", "reports/q3.csv",
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	chunks := (len(content) + objstore.ChunkSize - 1) / objstore.ChunkSize
	if detail.File.UpdatedBy != e2eOperatorID || detail.File.Size != int64(len(content)) ||
		len(detail.File.Chunks) != chunks {
		t.Errorf("the other member's row: by %q, %d bytes, %d chunks",
			detail.File.UpdatedBy, detail.File.Size, len(detail.File.Chunks))
	}
}

// A SEAT ON A NODE THAT HOLDS NO DATA WRITES A FILE THE FLEET KEEPS. Its row
// is written through a data node and its bytes stored on one, and the data
// node serves both — while the node that ran the turn keeps neither.
func TestASeatOnAStatelessNodeWritesAFileTheDataNodeServes(t *testing.T) {
	p := startStatelessPair(t)
	waitFor(t, "the stateless node to be admitted by a data node", hydrated(t, p.agent.engine))
	waitForSeat(t, p.agent, "ceo")
	// THE FLEET'S FIRST MAP, which the data node writes within a second of
	// booting: a turn that ran before it would be told, correctly, that no
	// data node holds objects yet — a fresh fleet's one-second window, not
	// the path under test.
	waitFor(t, "the fleet's first placement map", func() bool {
		_, found, err := p.data.engine.Backends().Fleet.ObjectMap(t.Context())
		return err == nil && found
	})
	const body = "# Launch plan\n\nShip the migration on Thursday.\n"

	p.agent.model.callOnExecute(tracker.WriteProjectFileTool, map[string]any{
		"project": "ENG", "path": "plans/launch.md", "content": body,
	})
	wakeWithMessage(t, p.agent, "ceo")

	var detail tracker.FileDetail
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("what the seat's tools answered: %q", p.agent.model.toolResults())
		}
	})
	waitFor(t, "the file on the data node", func() bool {
		got, err := p.data.engine.Tracker().File(t.Context(), "ENG", "plans/launch.md",
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			return false
		}
		detail = got
		return true
	})
	if detail.File.UpdatedBy != "ceo" {
		t.Errorf("the file is attributed to %q", detail.File.UpdatedBy)
	}
	rc, err := p.data.engine.Objects().Open(t.Context(), detail.File.Manifest())
	if err != nil {
		t.Fatalf("open the file's bytes on the data node: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read the file's bytes on the data node: %v", err)
	}
	if string(got) != body {
		t.Errorf("the data node holds %q", got)
	}
}
