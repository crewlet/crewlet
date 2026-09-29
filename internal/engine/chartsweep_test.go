package engine_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
)

// A SEALED VALUE NOTHING NAMES ANY MORE IS COLLECTED — AND ONLY THAT ONE, AND
// ONLY A GRACE AFTER THE SWEEP FIRST SAW NOTHING NAME IT.
//
// Rotating a sealed token seals the new one under the rotating write's own
// name and moves the row onto it, which leaves the previous value in the
// store, and the store has no retention of its own: before the sweep it
// outlived the company. It goes once the grace has passed — counted from when
// the sweep first saw nothing name it, never from when it was written, because
// a value sealed months ago stops being named the instant the rotation
// applies, and a peer that has not applied the rotation yet still resolves the
// old reference. The values the rows still name — the same seat's address, a
// colleague's token, the rotated token — stay.
func TestTheSweepCollectsASealedValueNothingNames(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)
	writer := e.ChartWriter()
	if _, err := writer.WriteBatch(t.Context(), "test:hire:cfo", chart.Batch{
		Operations: []chart.Operation{{Kind: chart.OpCreateSeat,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "cfo"},
			SeatKind: chart.SeatAgent}},
	}); err != nil {
		t.Fatalf("hire: %v", err)
	}
	give := func(opID, email, token string) {
		t.Helper()
		if _, err := writer.WriteSeat(t.Context(), opID, chart.SeatContent{
			Handle: "cfo", Name: "CFO", Email: email,
			Runtime: json.RawMessage(`{"llm":["zulu"],"mcp_env":{"tracker":` +
				`{"SEAT_TOKEN":"` + token + `"}}}`),
		}); err != nil {
			t.Fatalf("write the seat: %v", err)
		}
	}
	// named is what the running company's seat references in two fields,
	// once this node has applied a write that put want there.
	named := func(handle string, want func(email, token string) bool) (email, token string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if role := e.Company().Org.Role(handle); role != nil {
				email, token = role.Email, role.MCPEnv["tracker"]["SEAT_TOKEN"]
				if want(email, token) {
					return email, token
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never reached the state the case waits for: (%q, %q)",
					handle, email, token)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	sealedName := func(ref string) string {
		t.Helper()
		name, whole := envref.Whole(ref)
		if !whole || !chart.OwnsSecret(name) {
			t.Fatalf("%q is not a sealed reference", ref)
		}
		return name
	}
	both := func(email, token string) bool { return email != "" && token != "" }

	give("test:content:cfo:1", "cfo@example.com", "cfo-token")
	emailRef, firstRef := named("cfo", both)
	email, first := sealedName(emailRef), sealedName(firstRef)
	_, devRef := named("dev", func(_, token string) bool { return token != "" })
	dev := sealedName(devRef)

	store := fleetsecrets.New(e.Backends().Fleet, nil)
	stored := func(name string) bool {
		t.Helper()
		_, found, err := store.Describe(t.Context(), name)
		if err != nil {
			t.Fatalf("describe %s: %v", name, err)
		}
		return found
	}
	for _, name := range []string{first, email, dev} {
		if !stored(name) {
			t.Fatalf("%s is not in the store before anything was rotated", name)
		}
	}

	// THE ROTATION: a new literal, and the address handed back as read.
	give("test:content:cfo:2", emailRef, "cfo-token-rotated")
	_, secondRef := named("cfo", func(_, token string) bool { return token != firstRef })
	second := sealedName(secondRef)
	if second == first {
		t.Fatalf("the rotation sealed under the name the row already named, %s — "+
			"a write that never landed would have replaced the live credential", first)
	}

	sweep := func(at time.Time) int64 {
		t.Helper()
		n, err := engine.CollectChartSealsForTest(t.Context(), e, at)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		return n
	}
	// THE FIRST SIGHTING, which starts the grace: the superseded token was
	// written long before, and nothing is collected on the tick that first
	// sees nothing name it.
	start := time.Now()
	if n := sweep(start); n != 0 {
		t.Fatalf("the sweep's first sight of an unnamed value collected %d — "+
			"the grace runs from when nothing named it, and a peer that has "+
			"not applied the rotation still resolves the old reference", n)
	}
	if n := sweep(start.Add(chart.SealGrace / 2)); n != 0 {
		t.Fatalf("the sweep inside the grace collected %d values", n)
	}
	if n := sweep(start.Add(chart.SealGrace + time.Minute)); n != 1 {
		t.Errorf("the sweep past the grace collected %d values, want the one "+
			"superseded token", n)
	}
	if stored(first) {
		t.Errorf("%s is still in the store with nothing naming it", first)
	}
	for _, name := range []string{second, email, dev} {
		if !stored(name) {
			t.Errorf("the sweep deleted %s, which a row still names", name)
		}
	}

	// A FILE EXPORTED BEFORE THE ROTATION names the collected value. Written
	// back, it was accepted and the seat resolved an empty credential with the
	// value unrecoverable; it is refused, naming the field.
	_, err := writer.WriteSeat(t.Context(), "test:content:cfo:stale", chart.SeatContent{
		Handle: "cfo", Name: "CFO", Email: emailRef,
		Runtime: json.RawMessage(`{"llm":["zulu"],"mcp_env":{"tracker":` +
			`{"SEAT_TOKEN":"` + firstRef + `"}}}`),
	})
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("writing back a reference to a collected value answered %v, "+
			"want a refusal naming the field", err)
	}
	// AND ONE WHOSE VALUE IS STILL HELD — the rotation undone by restoring the
	// old file before the sweep took anything — lands and resolves.
	give("test:content:cfo:3", emailRef, "${CFO_TOKEN}")
	named("cfo", func(_, token string) bool { return token == "${CFO_TOKEN}" })
	if n := sweep(time.Now()); n != 0 {
		t.Fatalf("a sweep inside the grace collected %d values", n)
	}
	give("test:content:cfo:restore", emailRef, secondRef)
	named("cfo", func(_, token string) bool { return token == secondRef })
	if got := e.Resolve(secondRef); got != "cfo-token-rotated" {
		t.Errorf("the restored reference resolves to %q, want the value it names", got)
	}
}
