package statelog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SNAPSHOT PROTOCOL AT LAYOUT 0 IS THE ONE A BUILD BEFORE PARTITIONS SPEAKS,
// in both directions.
//
// A donor and a joiner of two builds share one fleet through a rolling upgrade,
// and an artefact, an offer request and a fetch are payloads they exchange: a
// node that falls below a trimmed log's floor mid-upgrade must be able to adopt
// from a donor of either build, since the trim counts both builds' artefacts
// toward its two donors. The shapes below are that build's own, byte for byte:
// a request naming no layout or partition, a version-2 manifest naming neither,
// and a fetch whose body is the subject to deliver to.

// previousManifest is a manifest exactly as a build before partitions writes
// and reads one — no layout and no partition.
type previousManifest struct {
	V             int                                `json:"v"`
	TakenAt       time.Time                          `json:"taken_at"`
	NodeID        string                             `json:"node_id"`
	EngineVersion string                             `json:"engine_version"`
	Migrations    []string                           `json:"migrations"`
	Scrubbed      []string                           `json:"scrubbed"`
	Domains       map[string]statelog.DomainPosition `json:"domains"`
	Bytes         int64                              `json:"bytes"`
	SHA256        string                             `json:"sha256"`
	Artifact      string                             `json:"artifact"`
}

// previousOffer is an offer as that build answers and reads one.
type previousOffer struct {
	Manifest previousManifest `json:"manifest"`
	Fetch    string           `json:"fetch"`
}

// A PREVIOUS BUILD'S JOINER IS OFFERED AND STREAMED LAYOUT 0'S ARTEFACT by this
// build's donor: its request names no partition, which at a layout-0 donor can
// only mean the whole estate; the offer is a manifest it reads — version 2, its
// logs under their domains' bare names; and its fetch, a body that is the
// deliver subject and no headers, is streamed the donor's newest artefact.
func TestAPreviousBuildsJoinerAdoptsFromThisBuildAtLayoutZero(t *testing.T) {
	t.Parallel()
	h := newTransferHarness(t, 4096)
	ask := []byte(`{"node_id":"old","need":{"probe":1},"generations":{"probe":1}}`)
	reply, err := h.nc.Request(statelog.SubjectOffer, ask, 5*time.Second)
	if err != nil {
		t.Fatalf("a previous build's offer request went unanswered: %v", err)
	}
	var offer previousOffer
	if err := json.Unmarshal(reply.Data, &offer); err != nil {
		t.Fatalf("the offer is not one a previous build decodes: %v", err)
	}
	if offer.Manifest.V != 2 {
		t.Errorf("the offer is a version %d manifest, and a previous build reads 2 alone",
			offer.Manifest.V)
	}
	if _, named := offer.Manifest.Domains["probe"]; !named {
		t.Errorf("the offer names %v, and a previous build refuses one that does not "+
			"name its domain under its own name", offer.Manifest.Domains)
	}

	got := fetchAsPreviousBuild(t, h.nc, offer.Fetch)
	want, err := os.ReadFile(h.artefact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("a previous build's fetch was streamed %d byte(s), want the artefact's %d",
			len(got), len(want))
	}
}

// fetchAsPreviousBuild fetches from subject as a build before partitions does —
// the body is the subject to deliver to, and there are no headers — and returns
// what was streamed, failing unless the donor ends the transfer with 204.
func fetchAsPreviousBuild(t *testing.T, nc *nats.Conn, subject string) []byte {
	t.Helper()
	deliver := nats.NewInbox()
	sub, err := nc.SubscribeSync(deliver)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.PublishRequest(subject, nats.NewInbox(), []byte(deliver)); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("the transfer stopped: %v", err)
		}
		if status := msg.Header.Get("Status"); status != "" {
			if status != "204" {
				t.Fatalf("the transfer ended %s: %s", status, msg.Header.Get("Description"))
			}
			return got
		}
		got = append(got, msg.Data...)
		if msg.Reply != "" {
			if err := nc.Publish(msg.Reply, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// THIS BUILD'S JOINER ADOPTS FROM A PREVIOUS BUILD'S DONOR at layout 0: the
// donor's version-2 manifest names no partition, which is read as the one file
// that build ever held — `estate.000` — and is usable; and the fetch's body is
// the subject to deliver to, which is all that donor reads of it.
func TestThisBuildsJoinerAdoptsFromAPreviousBuildAtLayoutZero(t *testing.T) {
	t.Parallel()
	h := newTransferHarness(t, 4096)
	body, err := os.ReadFile(h.artefact)
	if err != nil {
		t.Fatal(err)
	}
	fetchSubject := statelog.SubjectFetchPrefix + "old-donor"
	old := previousOffer{Fetch: fetchSubject, Manifest: previousManifest{
		V: 2, TakenAt: h.manifest.TakenAt, NodeID: "old-donor", Domains: h.manifest.Domains,
		Bytes: h.manifest.Bytes, SHA256: h.manifest.SHA256, Artifact: h.manifest.Artifact,
	}}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var offer statelog.Offer
	if err := json.Unmarshal(raw, &offer); err != nil {
		t.Fatalf("a previous build's offer does not decode: %v", err)
	}
	if offer.Manifest.Partition != statelog.EstatePartition.String() || offer.Manifest.Layout != 0 {
		t.Fatalf("a manifest naming no partition reads as %q of layout %d, want layout 0's %s",
			offer.Manifest.Partition, offer.Manifest.Layout, statelog.EstatePartition)
	}
	build := map[string]statelog.Registered{"probe": {Domain: probeDomain{}, Log: logOf(probeDomain{}),
		Spec: specOf(probeDomain{})}}
	req := statelog.OfferRequest{NodeID: "joiner", Partition: statelog.EstatePartition.String(),
		Need: map[string]uint64{"probe": 1}}
	if err := offer.Usable(req, build, nil); err != nil {
		t.Fatalf("a previous build's layout-0 artefact is refused: %v", err)
	}

	// A PREVIOUS BUILD'S DONOR, which reads the fetch's body as the subject
	// to deliver to and nothing else.
	fetches, err := h.nc.Subscribe(fetchSubject, func(msg *nats.Msg) {
		deliver := string(msg.Data)
		chunk := nats.NewMsg(deliver)
		chunk.Data = body
		_ = h.nc.PublishMsg(chunk)
		end := nats.NewMsg(deliver)
		end.Header.Set("Status", strconv.Itoa(204))
		_ = h.nc.PublishMsg(end)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fetches.Unsubscribe() })
	dest := filepath.Join(t.TempDir(), "adopt.part")
	n, err := statelog.FetchArtefact(t.Context(), h.nc, offer, dest)
	if err != nil {
		t.Fatalf("this build's fetch from a previous build's donor: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) || !bytes.Equal(got, body) {
		t.Errorf("fetched %d byte(s), want the artefact's %d", n, len(body))
	}
}

// A MANIFEST A PREVIOUS BUILD LEFT ON THIS NODE'S DISK IS STILL ITS ARTEFACT: a
// node upgraded in place keeps advertising — and donating — the copy it took
// before the upgrade, as layout 0's one partition.
func TestAPreviousBuildsManifestOnDiskIsLayoutZeros(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(previousManifest{V: 2, NodeID: "node-a", Artifact: "snapshot-1-7.db",
		Domains: map[string]statelog.DomainPosition{"probe": {Seq: 7, Generation: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot-1-7.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := statelog.ReadManifest(path)
	if err != nil {
		t.Fatalf("a previous build's manifest is unreadable: %v", err)
	}
	if m.Partition != statelog.EstatePartition.String() || m.Layout != 0 {
		t.Errorf("it reads as %q of layout %d, want layout 0's %s", m.Partition, m.Layout,
			statelog.EstatePartition)
	}

	// AND A MANIFEST NAMING NO PARTITION OF A DIVIDED LAYOUT — which no build
	// wrote — is not read as anything.
	raw = bytes.Replace(raw, []byte(`"v":2`), []byte(`"v":2,"layout":1`), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := statelog.ReadManifest(path); err == nil {
		t.Error("a manifest of layout 1 naming no partition was read")
	}
}

// AND A DONOR RUNNING A DIVIDED LAYOUT STREAMS A PREVIOUS BUILD NOTHING: its
// fetch names no partition, and answered with one partition's file that build
// would install it as its whole estate.
func TestADividedDonorRefusesAPreviousBuildsFetch(t *testing.T) {
	t.Parallel()
	h := newTransferHarness(t, 4096)
	layout := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{"probe"}},
	}}
	served := statelog.PartitionID{Space: statelog.SpaceTracker}
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "divided", Layout: layout, Serves: statelog.ServesOnly(served).Serving,
		Dial: func(context.Context) (*nats.Conn, error) { return h.nc, nil },
		Newest: func(p statelog.PartitionID) (statelog.Manifest, bool) {
			return statelog.Manifest{V: statelog.ManifestVersion, Layout: 1, Partition: p.String(),
				Artifact: filepath.Base(h.artefact)}, true
		},
		Path: func(statelog.Manifest) string { return h.artefact },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = donor.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	deliver := nats.NewInbox()
	sub, err := h.nc.SubscribeSync(deliver)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := h.nc.PublishRequest(statelog.SubjectFetchPrefix+"divided", nats.NewInbox(),
			[]byte(deliver)); err != nil {
			t.Fatal(err)
		}
		msg, err := sub.NextMsg(200 * time.Millisecond)
		if err == nil {
			if status := msg.Header.Get("Status"); status != "400" {
				t.Fatalf("a divided donor answered a previous build's fetch %q with %d "+
					"byte(s), want it refused", status, len(msg.Data))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the divided donor never answered the fetch")
		}
	}
}
