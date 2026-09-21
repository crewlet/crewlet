package engine

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHANGE FEED IS THE SECOND READER OF A SIGNED LOG, and these cases are
// what say it reads it the way the framework requires: the frame first, and a
// domain decoder handed the body or nothing.
//
// It shipped the other way for exactly one commit, and the failure is worth
// naming because it looks like anything but a signature bug. The feed handed a
// domain's `Envelope` the raw stream bytes, which now begin with four magic
// characters, so every record in the company came back `invalid character 'c'
// looking for beginning of value` — and the feed's own rule for a record it
// cannot decode is to acknowledge and skip it. Every notification in the
// company stopped, the log blamed the records, and the applier beside it was
// applying the same bytes without complaint.
func TestTheChangeFeedOpensTheFrameBeforeADomainDecodes(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(statelog.Envelope{V: 1, Kind: "task.create", OpID: "op-1", Gen: 3})
	if err != nil {
		t.Fatal(err)
	}
	ring := statelog.OneKey("k1", "material-one")
	signer, err := statelog.NewSigner("tracker", ring)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	framed := signer.Seal(body)
	verifier, err := statelog.NewVerifier("tracker", ring)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// THE CONTROL THAT MAKES THIS CASE ABLE TO FAIL: the frame is opaque
	// to a domain decoder, so a reader that skipped Open is not merely
	// impolite — it gets nothing through. Without this the case below
	// would pass against a feed that never framed anything at all.
	if _, err := decodeEnvelope(framed); err == nil {
		t.Fatal("a domain decoder read the frame, so skipping the verifier would " +
			"be invisible and this case proves nothing")
	}

	env, got, verdict, err := openRecord(verifier, decodeEnvelope, framed)
	if err != nil {
		t.Fatalf("openRecord: %v", err)
	}
	if verdict != statelog.Verified {
		t.Fatalf("verdict = %q, want %q", verdict, statelog.Verified)
	}
	if env.OpID != "op-1" || env.Gen != 3 {
		t.Errorf("envelope = %+v, want the one that was signed", env)
	}
	// THE BODY, NOT THE FRAME. The wake's own translators decode this
	// again in packages that have never heard of a signature, so a framed
	// payload travelling out of here fails there instead.
	if !bytes.Equal(got, body) {
		t.Errorf("the record travelling to the wake is %q, want the unframed body %q",
			got, body)
	}
}

// TestAnUnverifiedRecordReachesNoDomainDecoder. Both dispositions hand back
// nothing: the feed has nowhere to file a record it cannot attribute, so
// unlike the applier it has no reason to hold its body.
func TestAnUnverifiedRecordReachesNoDomainDecoder(t *testing.T) {
	t.Parallel()
	signer, err := statelog.NewSigner("tracker", statelog.OneKey("k2", "material-two"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	framed := signer.Seal([]byte(`{"v":1,"op_id":"op-1"}`))
	verifier, err := statelog.NewVerifier("tracker", statelog.OneKey("k1", "material-one"))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	for name, tc := range map[string]struct {
		payload []byte
		want    statelog.Verdict
	}{
		"a key this node does not hold": {framed, statelog.KeyUnknown},
		"bytes that are not a frame":    {[]byte(`{"v":1,"op_id":"op-1"}`), statelog.Tampered},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var reached bool
			decode := func(b []byte) (statelog.Envelope, error) {
				reached = true
				return decodeEnvelope(b)
			}
			env, body, verdict, err := openRecord(verifier, decode, tc.payload)
			if err != nil {
				t.Fatalf("openRecord: %v", err)
			}
			if verdict != tc.want {
				t.Errorf("verdict = %q, want %q", verdict, tc.want)
			}
			if reached {
				t.Error("the domain's decoder was handed bytes this node could not " +
					"authenticate")
			}
			if body != nil || env.OpID != "" {
				t.Errorf("an unverified record yielded %+v and %d bytes — a wake "+
					"derived from either is the engine acting on an instruction it "+
					"cannot attribute", env, len(body))
			}
		})
	}
}

// TestAnUndecodableEnvelopeIsTheEnvelopesFailureNotTheFrames, because the two
// have different dispositions at the caller and one error channel for both
// would make them one event.
func TestAnUndecodableEnvelopeIsTheEnvelopesFailureNotTheFrames(t *testing.T) {
	t.Parallel()
	ring := statelog.OneKey("k1", "material-one")
	signer, err := statelog.NewSigner("tracker", ring)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := statelog.NewVerifier("tracker", ring)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	_, _, verdict, err := openRecord(verifier, decodeEnvelope, signer.Seal([]byte("not json")))
	if err == nil {
		t.Fatal("a record this build cannot decode came back clean")
	}
	if verdict != statelog.Verified {
		t.Errorf("verdict = %q, want %q — the frame opened, and a caller that read "+
			"this as unverified would log a forged record where there is none",
			verdict, statelog.Verified)
	}
}

func decodeEnvelope(payload []byte) (statelog.Envelope, error) {
	var env statelog.Envelope
	err := json.Unmarshal(payload, &env)
	return env, err
}
