package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
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

// operatorCall sends one authenticated request to a node's API and decodes
// its JSON answer.
func operatorCall(t *testing.T, n *node, method, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, n.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e2eOperatorToken)
	resp, err := n.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// placedObjects is the placement block a node's fleet view shows, and false
// while it shows no placed map.
func placedObjects(t *testing.T, n *node) (map[string]any, bool) {
	t.Helper()
	status, body := operatorCall(t, n, http.MethodGet, "/fleet")
	objects, _ := body["objects"].(map[string]any)
	if status != http.StatusOK || objects["state"] != "placed" {
		return nil, false
	}
	return objects, true
}

// A HOLD PLACED THROUGH ONE NODE READS ON ANOTHER, and is released the same
// way: the gesture is a compare-and-set on the one record every node places
// by, made through the adapter `crewlet run` mounts, and the fleet view on a
// member that made no gesture reads the stored map rather than its own memory
// of it. Taking a member out crosses the same seam and moves data as well, so
// it has a case of its own ([TestAMemberTakenOutHandsItsChunksToTheRest]).
func TestAHoldPlacedThroughOneNodeReadsOnAnother(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)
	waitFor(t, "the placement map to place on every member", func() bool {
		objects, placed := placedObjects(t, c.nodes[0])
		members, _ := objects["members"].([]any)
		return placed && len(members) == fleetSize
	}, func() string {
		status, body := operatorCall(t, c.nodes[0], http.MethodGet, "/fleet")
		return fmt.Sprintf("%d %v", status, body["objects"])
	})

	status, answer := operatorCall(t, c.nodes[0], http.MethodPost,
		"/objects/hold?for=30m&reason=e2e")
	if status != http.StatusOK || answer["landed"] != true {
		t.Fatalf("holding the map answered %d %v", status, answer)
	}
	objects, _ := placedObjects(t, c.nodes[1])
	if hold, _ := objects["hold"].(map[string]any); hold["by"] != e2eOperatorID ||
		hold["reason"] != "e2e" {
		t.Errorf("the other member's fleet view reads the hold as %v", objects["hold"])
	}

	status, answer = operatorCall(t, c.nodes[1], http.MethodPost, "/objects/release")
	if status != http.StatusOK || answer["landed"] != true {
		t.Fatalf("releasing the hold answered %d %v", status, answer)
	}
	if objects, _ := placedObjects(t, c.nodes[0]); objects["hold"] != nil {
		t.Errorf("the released hold still reads %v", objects["hold"])
	}
}

// fleetObjects is a closure rendering member 0's view of the placement map,
// for a wait that timed out to say what the fleet looked like.
func (c *cluster) fleetObjects(t *testing.T) func() string {
	return func() string {
		status, body := operatorCall(t, c.nodes[0], http.MethodGet, "/fleet")
		return fmt.Sprintf("%d %v", status, body["objects"])
	}
}

// placedOnEvery waits for the first map to place on every member, and answers
// the fleet view's block for it.
func (c *cluster) placedOnEvery(t *testing.T) map[string]any {
	t.Helper()
	var objects map[string]any
	waitFor(t, "the placement map to place on every member", func() bool {
		got, placed := placedObjects(t, c.nodes[0])
		members, _ := got["members"].([]any)
		objects = got
		return placed && len(members) == len(c.nodes)
	}, c.fleetObjects(t))
	return objects
}

// applyObjects applies a revision of the harness company carrying the given
// `objects:` block on every member, at one activation instant — what every
// node's reconciler does with an activation, so whichever member holds the
// map duty reads it from the company it runs.
//
// A FRESH DOCUMENT per member, never the live epoch's config edited in place:
// each member's company names its own scripted model.
func (c *cluster) applyObjects(t *testing.T, block string) {
	t.Helper()
	activated := time.Now()
	for i, n := range c.nodes {
		cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, n.model.url) + block))
		if err != nil {
			t.Fatalf("company config: %v", err)
		}
		if _, _, err := n.engine.Apply(t.Context(), cfg, activated); err != nil {
			t.Fatalf("member %d's Apply: %v", i, err)
		}
	}
}

// memberOf is one member's row in a fleet view's placement block.
func memberOf(objects map[string]any, node string) map[string]any {
	members, _ := objects["members"].([]any)
	for _, raw := range members {
		if member, _ := raw.(map[string]any); member["node"] == node {
			return member
		}
	}
	return nil
}

// THE MAP'S COPIES AND ITS FAILURE DOMAIN ARE THE COMPANY'S — ADR-0020.
//
// The harness boots every member with `stream.replicas` at the fleet's size
// and a company that names no object block, so the map first asks for the
// company's default and places it on every member. At three members that
// default and the broker's stream.replicas are the same number, so the first
// arm cannot tell which one the map read — whether an unset count is the
// company's default and never a node's setting is pinned where the two can
// differ, on an engine with no bootstrap to read at all
// (internal/engine's TestTheMapTakesTheCompanyStampedWithItsActivation).
//
// The second arm is what tells them apart here: a revision naming one copy
// spread across a label reaches the map with no restart, on whichever member
// holds the duty. A map reading its copies off the node holding that duty —
// which it did, and which dropped a fleet to one copy the day a node at the
// default of one held it — would still ask for three.
func TestTheMapsCopiesAreTheCompanys(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)
	objects := c.placedOnEvery(t)

	if c.nodes[0].engine.Company().Config.Objects != (config.Objects{}) {
		t.Fatal("the harness company names an objects block, so the default " +
			"below proves nothing about where the count comes from")
	}
	if objects["replicas"] != float64(config.DefaultObjectReplicas) ||
		objects["copies"] != float64(fleetSize) || objects["failure_domain"] != nil {
		t.Fatalf("a fleet of %d whose company names no object block asks for %v "+
			"copies, places %v, across %v; want the company default of %d, placed "+
			"on every member, across nothing", fleetSize, objects["replicas"],
			objects["copies"], objects["failure_domain"], config.DefaultObjectReplicas)
	}

	if config.DefaultObjectReplicas == 1 {
		t.Fatal("the company default is one copy, so a revision naming one " +
			"cannot tell the company's count from the default's")
	}
	c.applyObjects(t, "objects:\n  replicas: 1\n  failure_domain: zone\n")
	waitFor(t, "the map to take the company's one copy across zones", func() bool {
		got, placed := placedObjects(t, c.nodes[1])
		return placed && got["replicas"] == float64(1) && got["copies"] == float64(1) &&
			got["failure_domain"] == "zone"
	}, c.fleetObjects(t))

	// NO MEMBER CARRIES THE LABEL, so each is a domain of its own — the
	// degradation that never collides — rather than all of them sharing the
	// empty one, which would have put the fleet in one domain.
	objects, _ = placedObjects(t, c.nodes[0])
	if objects["distinct_domains"] != float64(fleetSize) || objects["domain_limited"] != false {
		t.Errorf("%d unlabelled members span %v domains, limited %v; want %d, false",
			fleetSize, objects["distinct_domains"], objects["domain_limited"], fleetSize)
	}
}

// A MEMBER TAKEN OUT HANDS ITS CHUNKS TO THE REST WHILE EVERY MEMBER STILL
// SERVES THE FILE.
//
// Taking a member out makes a planned removal a COPY rather than a recovery:
// the map places nothing on it, and the members left fetch every chunk the
// out member held alone FROM the out member, which keeps serving throughout —
// so the file downloads whole through every one of them before, during and
// after.
//
// On one copy, because a member taken out moves something FROM ITSELF only
// when some chunk is held by it alone, and at the company default of three
// copies on three members every member holds every chunk. Deleting the out
// member's copies afterwards is its collector's, on the hour, under a
// confirmation internal/objstore/upkeep's suite certifies against real disks —
// so that half is not waited for here.
func TestAMemberTakenOutHandsItsChunksToTheRest(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)
	c.placedOnEvery(t)
	c.applyObjects(t, "objects:\n  replicas: 1\n")
	var epoch float64
	waitFor(t, "the map to take the company's one copy", func() bool {
		got, placed := placedObjects(t, c.nodes[1])
		epoch, _ = got["epoch"].(float64)
		return placed && got["replicas"] == float64(1) && got["copies"] == float64(1)
	}, c.fleetObjects(t))

	// EVERY MEMBER PLACES BY THAT MAP before anything is written: an upload
	// through a member still placing by the old one would put a copy on
	// every member, and taking any of them out would move nothing.
	client := c.nodes[0].engine.Objects()
	probe := []objstore.Hash{objstore.HashOf([]byte("which map do you place by"))}
	waitFor(t, "every member to place by the one-copy map", func() bool {
		for _, n := range c.nodes {
			held, err := client.Has(t.Context(), n.id, probe, false)
			if err != nil || float64(held.Epoch) < epoch {
				return false
			}
		}
		return true
	}, c.fleetObjects(t))

	content := bytes.Repeat([]byte("region,quarter,revenue\nemea,q3,1200\n"), 180_000)
	var answer map[string]any
	waitFor(t, "an upload to be stored", func() bool {
		status, body := upload(t, c.nodes[0], "ENG", "reports/q4.csv", content)
		answer = body
		return status == http.StatusOK
	})
	if answer["outcome"] != string(statelog.OutcomeApplied) {
		t.Fatalf("the upload answered %v", answer)
	}
	detail, err := c.nodes[0].engine.Tracker().File(t.Context(), "ENG", "reports/q4.csv",
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	var hashes []objstore.Hash
	for _, chunk := range detail.File.Manifest().Chunks {
		hashes = append(hashes, chunk.Hash)
	}
	holdings := func() map[string]transfer.Holding {
		out := map[string]transfer.Holding{}
		for _, n := range c.nodes {
			held, err := client.Has(t.Context(), n.id, hashes, false)
			if err != nil {
				t.Fatalf("ask %s which chunks it holds: %v", n.id, err)
			}
			out[n.id] = held
		}
		return out
	}

	// THE MEMBER TO TAKE OUT is the one holding the most chunks nobody else
	// does — on one copy every chunk is on exactly one member, and the file
	// is seven chunks, so some member always holds several.
	alone := map[string]int{}
	before := holdings()
	for i := range hashes {
		var holders []string
		for node, held := range before {
			if held.Held[i] {
				holders = append(holders, node)
			}
		}
		if len(holders) != 1 {
			t.Fatalf("chunk %d of a one-copy file is held by %v", i, holders)
		}
		alone[holders[0]]++
	}
	out := c.nodes[0]
	for _, n := range c.nodes[1:] {
		if alone[n.id] > alone[out.id] {
			out = n
		}
	}
	var stay []*node
	for _, n := range c.nodes {
		if n != out {
			stay = append(stay, n)
		}
	}

	// THE MEMBERS THAT STAY ARE ONES THE MAP COUNTS PRESENT, which is the
	// gesture's own precondition — taking out the last present member would
	// leave every write nowhere to land, and it is refused — and where the
	// out member's chunks can go.
	for _, n := range stay {
		waitFor(t, fmt.Sprintf("the map to count %s present", n.id), func() bool {
			got, placed := placedObjects(t, stay[0])
			return placed && memberOf(got, n.id) != nil && memberOf(got, n.id)["absence"] == nil
		}, c.fleetObjects(t))
	}
	t.Logf("%s holds %d of the file's %d chunks alone; taking it out", out.id,
		alone[out.id], len(hashes))
	status, taken := operatorCall(t, stay[0], http.MethodPost,
		fmt.Sprintf("/objects/out/%s?confirm=%s&reason=e2e", out.id, out.id))
	if status != http.StatusOK || taken["landed"] != true {
		t.Fatalf("taking %s out answered %d %v", out.id, status, taken)
	}

	// THE MEMBERS LEFT HOLD EVERY CHUNK BETWEEN THEM, the out member's
	// fetched by their repair from the member taken out: each chunk placed
	// on exactly one of them — one copy — held where it is placed, and
	// placed on the member taken out not at all.
	waitFor(t, fmt.Sprintf("the members left to hold every chunk %s held", out.id), func() bool {
		now := holdings()
		for i := range hashes {
			if now[out.id].Placed[i] {
				return false
			}
			placedOn := 0
			for _, n := range stay {
				if now[n.id].Placed[i] {
					if !now[n.id].Held[i] {
						return false
					}
					placedOn++
				}
			}
			if placedOn != 1 {
				return false
			}
		}
		return true
	}, c.fleetObjects(t), func() string { return fmt.Sprintf("%+v", holdings()) })

	objects, _ := placedObjects(t, stay[0])
	if member := memberOf(objects, out.id); member["out"] != true ||
		member["share_percent"] != float64(0) || member["out_reason"] != "e2e" {
		t.Errorf("the fleet view shows the member taken out as %v", member)
	}

	// AND THE FILE IS WHOLE THROUGH EVERY MEMBER — the one taken out still
	// serves, reading what it no longer holds from the members that do.
	for _, n := range c.nodes {
		if status, got := download(t, n, "ENG", "reports/q4.csv"); status != http.StatusOK ||
			!bytes.Equal(got, content) {
			t.Errorf("a download through %s answered %d with %d bytes of %d",
				n.id, status, len(got), len(content))
		}
	}
}
