package statelog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// refusals keeps what a runner witnessed.
type refusals struct {
	mu   sync.Mutex
	seen []statelog.Refusal
}

func (r *refusals) RecordRefused(_ context.Context, refusal statelog.Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, refusal)
}

func (r *refusals) all() []statelog.Refusal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// witnessedRunner rebuilds the harness's runner with a witness, over the same
// database, applier and broker — the shape of a node process starting.
func (h *applyHarness) witnessedRunner(w statelog.Witness) {
	h.t.Helper()
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:     probeDomain{},
		Verifier:   testVerifier(h.t, probeDomain{}),
		Applier:    h.applier,
		Fetch:      h.fetch,
		DB:         h.db.Replicated(),
		Generation: 1,
		Metrics:    h.metrics,
		Witness:    w,
	})
	if err != nil {
		h.t.Fatalf("NewRunner: %v", err)
	}
	h.runner = runner
}

// offerFramed queues bytes exactly as given, which is what a writer that is not
// this fleet puts on the broker.
func (f *probeFetch) offerFramed(seq uint64, framed []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, statelog.Message{
		Seq:      seq,
		StoredAt: time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(seq) * time.Second),
		Payload:  framed,
		Ack: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.acked[seq]++
			return nil
		},
	})
}

// sealedUnder frames the probe envelope under a key this fleet's test ring
// does not hold.
func sealedUnder(t *testing.T, keyID string, e statelog.Envelope) []byte {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := statelog.NewSigner(probeDomain{}.Name(),
		statelog.OneKey(keyID, "material-this-node-lacks-"+keyID))
	if err != nil {
		t.Fatal(err)
	}
	return signer.Seal(body)
}

// A RECORD SIGNED UNDER A KEY THIS NODE LACKS IS WITNESSED ONCE PER KEY, AND
// AGAIN BY THE NEXT PROCESS THAT MEETS IT.
//
// Two records under one unknown id are one fact — an operator's keyring is
// missing that key — and a row per record would put a rotation's whole
// backlog on the feed. A second id is a second fact. And a restart meets the
// retained records only through the reprocess, never through a decode, so a
// witness wired to the decode alone would say nothing on the one boot an
// operator is watching for it.
//
// Mutations: drop the dedupe and the first run witnesses three; drop the
// reprocess arm and the second process witnesses none.
func TestAnUnverifiableRecordIsWitnessedOncePerKey(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	first := &refusals{}
	h.witnessedRunner(first)
	h.fetch.offerFramed(1, sealedUnder(t, "k9", env(1, "edit", "a", "op-1", 1)))
	h.fetch.offerFramed(2, sealedUnder(t, "k9", env(2, "edit", "b", "op-2", 1)))
	h.fetch.offerFramed(3, sealedUnder(t, "k8", env(3, "edit", "c", "op-3", 1)))
	h.fetch.offer(4, env(4, "edit", "d", "op-4", 1))
	if err := h.run(4); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 3 {
		t.Fatalf("retained %d, want the 3 this node cannot authenticate", got)
	}
	got := first.all()
	want := []statelog.Refusal{
		{Domain: probeDomain{}.Name(), Verdict: statelog.KeyUnknown, KeyID: "k9",
			Held: []string{"k1"}, Position: statelog.Position{
				Stream: probeStream, Generation: 1, Seq: 1}},
		{Domain: probeDomain{}.Name(), Verdict: statelog.KeyUnknown, KeyID: "k8",
			Held: []string{"k1"}, Position: statelog.Position{
				Stream: probeStream, Generation: 1, Seq: 3}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("witnessed %+v\nwant      %+v", got, want)
	}

	// THE NEXT PROCESS, which meets them only through the reprocess.
	second := &refusals{}
	h.witnessedRunner(second)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	for len(second.all()) < 2 && ctx.Err() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-errs
	keys := []string{}
	for _, r := range second.all() {
		keys = append(keys, r.KeyID)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"k8", "k9"}) {
		t.Errorf("the restarted runner witnessed %v, want k8 and k9 once each", keys)
	}
}

// A TAMPERED RECORD IS WITNESSED BEFORE THE APPLIER STOPS, naming the key its
// frame claims — or none, when the bytes are not a frame at all.
//
// Mutation: witness only after the stop and nothing is witnessed, because the
// decode returns on the stop.
func TestATamperedRecordIsWitnessedAsTheApplierStops(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		framed func() []byte
		keyID  string
	}{
		{"a MAC that fails under a held key", func() []byte {
			body, _ := json.Marshal(env(1, "edit", "a", "op-1", 1))
			framed := probeSeal(body)
			framed[len(framed)-1] ^= 0xFF
			return framed
		}, "k1"},
		{"bytes that are not a frame", func() []byte {
			body, _ := json.Marshal(env(1, "edit", "a", "op-1", 1))
			return body
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t, probeDomain{})
			seen := &refusals{}
			h.witnessedRunner(seen)
			h.fetch.offerFramed(1, tc.framed())
			if err := h.run(1); !errors.Is(err, statelog.ErrStopped) {
				t.Fatalf("the applier went on past a tampered record: %v", err)
			}
			got := seen.all()
			if len(got) != 1 || got[0].Verdict != statelog.Tampered ||
				got[0].KeyID != tc.keyID || got[0].Position.Seq != 1 ||
				got[0].Domain != (probeDomain{}).Name() {
				t.Errorf("witnessed %+v, want one tampered refusal naming %q at 1",
					got, tc.keyID)
			}
		})
	}
}

// THE KEY ID IS WHATEVER THE FRAME SAYS, SO WHAT A WITNESS HEARS IS CAPPED.
//
// Anything that can reach a cluster port can write frames naming a fresh id
// each, and every id would otherwise be a row on the audit feed that a writer
// who is not the fleet authored. Past [statelog.MaxWitnessedKeys] the records
// are still retained, logged and counted — only the feed stops hearing about
// new ids.
//
// Mutation: drop the cap and every id is witnessed.
func TestTheWitnessHearsAtMostTheCappedNumberOfKeys(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	seen := &refusals{}
	h.witnessedRunner(seen)
	total := statelog.MaxWitnessedKeys + 4
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("forged-%02d", i)
		h.fetch.offerFramed(uint64(i), sealedUnder(t, id,
			env(uint64(i), "edit", id, "op-"+id, 1)))
	}
	if err := h.run(uint64(total)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != int64(total) {
		t.Fatalf("retained %d, want all %d — the cap is on the feed, never on "+
			"what the node keeps", got, total)
	}
	if got := len(seen.all()); got != statelog.MaxWitnessedKeys {
		t.Errorf("the witness heard %d ids, want the cap of %d", got,
			statelog.MaxWitnessedKeys)
	}
}

// The bytes a frame carries after its id are not needed to report the id, and a
// frame too short to carry an id reports none.
func TestARefusalNamesNoKeyForATruncatedFrame(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	seen := &refusals{}
	h.witnessedRunner(seen)
	framed := sealedUnder(t, "k9", env(1, "edit", "a", "op-1", 1))
	h.fetch.offerFramed(1, bytes.Clone(framed[:5]))
	if err := h.run(1); !errors.Is(err, statelog.ErrStopped) {
		t.Fatalf("run: %v", err)
	}
	if got := seen.all(); len(got) != 1 || got[0].KeyID != "" {
		t.Errorf("witnessed %+v, want one refusal naming no key", got)
	}
}
