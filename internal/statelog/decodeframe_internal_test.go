package statelog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// THE APPLIER IS HANDED THE BODY AND THE DEFERRED TABLE KEEPS THE FRAME.
//
// This is ADR-0018 at the one field every applier reads, and it is the case
// the suite above it structurally cannot provide: [statelogtest] builds a
// [Record] by hand and hands it straight to an applier, so it certifies what a
// domain does with a record and never what the framework puts in one.
//
// It shipped the other way round, and the failure is worth naming because it
// looks like anything but a signature bug. Every domain's applier decodes
// `Record.Payload`, so with the frame still on it every record in the company
// came back `invalid character 'c' looking for beginning of value` from inside
// the apply transaction. The applier retried the same record for ever, its
// position never moved, health reported the node merely BEHIND rather than
// broken, and the seats it was holding were shed to a peer that would have
// done exactly the same thing.
func TestTheApplierIsHandedTheBodyAndTheTableTheFrame(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Envelope{
		V: 1, Kind: "widget", Subject: Subject{Kind: "widget", ID: "w1"},
		OpID: "op-1", Gen: 1, Scope: ScopeSet{Paths: []string{"widget/w1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ring := OneKey("k1", "material-one")
	signer, err := NewSigner("probe", ring)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := NewVerifier("probe", ring)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	framed := signer.Seal(body)

	// THE CONTROL that makes this case able to fail: the frame is opaque
	// to the domain's own decoder, so handing it on is not merely untidy.
	// Without this the assertions below would pass against a framework
	// that never framed anything.
	if _, err := (frameProbeDomain{}).Envelope(framed); err == nil {
		t.Fatal("the domain's decoder read the frame, so handing it the framed " +
			"bytes would be invisible and this case proves nothing")
	}

	r := &Runner{
		domain:   frameProbeDomain{},
		verifier: verifier,
		spec:     frameProbeDomain{}.Stream(),
		gen:      1,
		logger:   slog.New(slog.DiscardHandler),
		now:      func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	got, err := r.decode(t.Context(), []Message{{
		Seq: 7, Payload: framed, StoredAt: time.Unix(1_700_000_000, 0).UTC(),
	}})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decode returned %d records, want 1", len(got))
	}
	if !bytes.Equal(got[0].Payload, body) {
		t.Errorf("the applier would be handed %q, want the unframed body %q — "+
			"every domain decodes this field, so the frame here fails every "+
			"record in the company inside the apply transaction",
			got[0].Payload, body)
	}
	if !bytes.Equal(got[0].framed, framed) {
		t.Errorf("the deferred table would keep %q, want the record as published "+
			"%q — a retained record is re-verified by the build that can finally "+
			"read it, and it cannot be if the frame was dropped",
			got[0].framed, framed)
	}
	if got[0].verdict != Verified {
		t.Errorf("verdict = %q, want %q", got[0].verdict, Verified)
	}
}

// frameProbeDomain is the smallest domain a decode needs: an envelope reader
// and a stream to name.
type frameProbeDomain struct{}

func (frameProbeDomain) Name() string { return "probe" }
func (frameProbeDomain) Stream() StreamSpec {
	return StreamSpec{Name: "CREWLET_PROBE_LOG", Replay: ReplayStrict}
}
func (frameProbeDomain) RecordVersion() int { return 1 }
func (frameProbeDomain) Envelope(payload []byte) (Envelope, error) {
	var env Envelope
	err := json.Unmarshal(payload, &env)
	return env, err
}
func (frameProbeDomain) InstallsGate(Envelope) bool { return false }
func (frameProbeDomain) Tables() map[string]TableClass {
	return map[string]TableClass{"probe_rows": Replicated}
}
func (frameProbeDomain) DeferredTable() string { return "probe_log_deferred" }
func (frameProbeDomain) ScopeIndex() string    { return "probe_deferred_scope" }
func (frameProbeDomain) OpsTable() string      { return "probe_ops" }
func (frameProbeDomain) ReadinessInput() bool  { return true }
func (frameProbeDomain) ClaimsIdentity() bool  { return true }
