package coordtest

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- what one key holds ------------------------------------------------- //

// valueCases hold every backend to ONE bound on a value, [coord.MaxValueBytes],
// and to one answer past it, [coord.ErrValueTooLarge]: refused before
// anything is written, and never an outage. The KV backend refuses a value its
// leader's read could not carry back across a leaf's link; a twin that stored
// it would certify a write the store refuses, and a backend that called the
// refusal unavailable would have its caller retry a write no retry lands.
var valueCases = []fleetCase{
	{"a value past what a key holds is refused as too large, never as an outage, and nothing is written", func(h *fleetHarness) {
		past := make([]byte, coord.MaxValueBytes+1)
		created, err := h.f.CreateSandboxRun(h.ctx, "turn-too-large", past)
		h.tooLarge("a run's create", err)
		if created {
			h.t.Error("a run whose value is past what a key holds reads as created")
		}
		if _, found := h.run("turn-too-large"); found {
			h.t.Error("a run whose value is past what a key holds was stored")
		}

		h.createRun("turn-1", `{"status":"running"}`)
		_, changed, err := h.f.MutateSandboxRun(h.ctx, "turn-1",
			func(coord.Record) (coord.SandboxRunChange, error) {
				return coord.SandboxRunChange{Put: past}, nil
			})
		h.tooLarge("a run's update", err)
		if record, _ := h.run("turn-1"); changed || string(record.Value) != `{"status":"running"}` {
			h.t.Errorf("a run's update past what a key holds changed it (%t) to %d bytes, want it "+
				"as it was", changed, len(record.Value))
		}
		_, changed, err = h.f.MutateSandboxRunPart(h.ctx, "turn-1", coord.RunLog,
			func(coord.Record, bool) ([]byte, error) { return past, nil })
		h.tooLarge("a run's part", err)
		if changed {
			h.t.Error("a run's part past what a key holds reads as written")
		}
	}},
	{"a value at what a key holds is stored whole", func(h *fleetHarness) {
		at := []byte(`"` + strings.Repeat("x", coord.MaxValueBytes-2) + `"`)
		if !h.createRun("turn-at-the-bound", string(at)) {
			h.t.Fatal("a run whose value is what a key holds was not created")
		}
		if record, found := h.run("turn-at-the-bound"); !found || len(record.Value) != len(at) {
			h.t.Errorf("a run whose value is what a key holds reads back as %d bytes (found %t), "+
				"want its %d", len(record.Value), found, len(at))
		}
	}},
	{"an activation whose pointer is past what a key holds is refused as too large, and the pointer stays", func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("activate the first revision: %v", err)
		}
		body := []byte(`{"v":"` + strings.Repeat("x", coord.MaxValueBytes) + `"}`)
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: body, At: h.now(), Expect: "rev-1", Origin: anOrigin})
		h.tooLarge("an activation", err)
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-1" {
			h.t.Errorf("the pointer after an activation past what a key holds = %+v (found %t, %v), "+
				"want rev-1 as it was", target, found, err)
		}
	}},
}

// tooLarge fails the case unless err is a refusal of a value past what a key
// holds, and nothing that reads as an outage or as a write that may have
// landed.
func (h *fleetHarness) tooLarge(what string, err error) {
	h.t.Helper()
	switch {
	case !errors.Is(err, coord.ErrValueTooLarge):
		h.t.Errorf("%s past what a key holds = %v, want coord.ErrValueTooLarge", what, err)
	case errors.Is(err, coord.ErrUnavailable), errors.Is(err, coord.ErrWriteUnknown):
		h.t.Errorf("%s past what a key holds = %v: a refusal made before anything was sent read "+
			"as an outage", what, err)
	}
}
