package statelog_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// witnessed keeps what a runner witnessed.
type witnessed struct {
	mu   sync.Mutex
	seen []statelog.Refusal
}

func (r *witnessed) RecordRefused(_ context.Context, refusal statelog.Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, refusal)
}

func (r *witnessed) all() []statelog.Refusal {
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
		Log:        h.fetch,
		Node:       h.db,
		DB:         h.db.Replicated(),
		Checkpoint: statelog.Position{Generation: 1},
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
	first := &witnessed{}
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
	second := &witnessed{}
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
			seen := &witnessed{}
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
	seen := &witnessed{}
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
	seen := &witnessed{}
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

// keyedRunner rebuilds the harness's runner over the same database, applier
// and broker with a verifier over ring — the shape of a node restarting after
// an operator added a key to its secrets.keys.
func (h *applyHarness) keyedRunner(ring statelog.Keyring) {
	h.t.Helper()
	verifier, err := statelog.NewVerifier(probeDomain{}.Name(), ring)
	if err != nil {
		h.t.Fatalf("NewVerifier: %v", err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:     probeDomain{},
		Verifier:   verifier,
		Applier:    h.applier,
		Fetch:      h.fetch,
		Log:        h.fetch,
		Node:       h.db,
		DB:         h.db.Replicated(),
		Checkpoint: statelog.Position{Generation: 1},
		Metrics:    h.metrics,
	})
	if err != nil {
		h.t.Fatalf("NewRunner: %v", err)
	}
	h.runner = runner
}

// probeRowAt reports whether the probe applier wrote a row for seq.
func (h *applyHarness) probeRowAt(seq uint64) bool {
	h.t.Helper()
	var count int
	at := statelog.Position{Stream: probeStream, Generation: 1, Seq: seq}
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM probe_rows WHERE position = ?`, at.Packed()).Scan(&count)
	}); err != nil {
		h.t.Fatalf("read the probe rows: %v", err)
	}
	return count == 1
}

// A RECORD RETAINED UNDER A KEY THIS NODE LACKED IS APPLIED ONCE THE KEY
// ARRIVES.
//
// That is the whole of what "retained" promises: an unknown key id is a
// rotation that reached another node first, so the record is kept rather than
// refused, and the node that restarts with the key applies it — through the
// reprocess, which is the one place a restarted process meets a retained
// record again. The Verifier's own case proves the bytes open under the key;
// this proves the RUNNER acts on it: the row lands, the operation is held and
// the deferral is released, while the neighbour that verified all along was
// applied in the first pass rather than waiting behind it.
//
// Mutations: count a Verified record as kept in the reprocess, or drop the
// re-verify and hand the stored frame to the envelope decode, and either way
// the record stays retained with its key in hand.
func TestARetainedRecordAppliesOnceItsKeyArrives(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offerFramed(1, sealedUnder(t, "k9", env(1, "edit", "a", "op-1", 1)))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 1 {
		t.Fatalf("retained %d, want the one record signed under k9", got)
	}
	if !h.probeRowAt(2) {
		t.Fatal("the record signed under a held key was not applied beside " +
			"the one this node could not authenticate")
	}
	if h.probeRowAt(1) {
		t.Fatal("a record signed under a key this node lacks was applied")
	}

	// THE KEY ARRIVES: k9 with the material the writer signed under,
	// beside the key this node already held.
	h.keyedRunner(statelog.Keyring{ActiveID: "k1", Keys: []statelog.Key{
		{ID: "k1", Material: "test-material"},
		{ID: "k9", Material: "material-this-node-lacks-k9"},
	}})
	if err := h.boot(0); err != nil {
		t.Fatalf("the reprocess did not release the record: %v", err)
	}
	if !h.probeRowAt(1) {
		t.Error("the retained record was released without its row being written")
	}
	if _, held, err := h.runner.Op(t.Context(), "op-1"); err != nil || !held {
		t.Errorf("Op(op-1) = held %v, %v: the reprocessed record's operation "+
			"is not in the ledger, so its writer's retry would apply it twice",
			held, err)
	}
}

// AND A RETAINED RECORD THAT FAILS UNDER THE KEY IT NAMES STOPS THE NODE.
//
// Filed under an unknown key, a record is only a claim that some node holds
// that key. When the key arrives and the bytes do not verify under it, the
// claim was false — the record was written by something that is not this
// fleet — and applying it, or retaining it for ever, would both be wrong.
//
// Mutation: treat a reprocessed Tampered like KeyUnknown and the node goes on
// retaining a forgery with the key in hand.
func TestARetainedRecordThatFailsUnderItsArrivedKeyStopsTheNode(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offerFramed(1, sealedUnder(t, "k9", env(1, "edit", "a", "op-1", 1)))
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 1 {
		t.Fatalf("retained %d, want the one record signed under k9", got)
	}

	// A k9 THAT IS NOT THE ONE THE RECORD WAS SIGNED UNDER.
	h.keyedRunner(statelog.Keyring{ActiveID: "k1", Keys: []statelog.Key{
		{ID: "k1", Material: "test-material"},
		{ID: "k9", Material: "the-fleets-real-k9"},
	}})
	if err := h.boot(0); !errors.Is(err, statelog.ErrStopped) {
		t.Fatalf("reprocessing a record that fails under its named key: err = %v, "+
			"want ErrStopped", err)
	}
	if h.probeRowAt(1) {
		t.Error("a record that failed under the key it names was applied")
	}
	if got := h.retainedCount(); got != 1 {
		t.Errorf("retained %d after the stop, want the forgery left where it "+
			"was for an operator to see", got)
	}
}
