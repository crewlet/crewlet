package coordtest

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// someInstructions are instructions the contract stores, carrying the
// characters an HTML escaping encoder would rewrite.
const someInstructions = `{"length":"brief","text":"Say <it> & stop, ünïcode"}`

var instructionsCases = []fleetCase{
	{name: "instructions are saved only at the revision read, and read back in their own bytes", fn: func(h *fleetHarness) {
		save := func(handle, doc string, revision uint64) (uint64, bool) {
			h.t.Helper()
			landed, ok, err := h.f.SaveSeatInstructions(h.ctx, handle, json.RawMessage(doc), h.now(), revision)
			if err != nil {
				h.t.Fatalf("SaveSeatInstructions(%s at %d): %v", handle, revision, err)
			}
			return landed, ok
		}
		first, ok := save("pm", someInstructions, 0)
		if !ok || first == 0 {
			h.t.Fatalf("the first save answered %d, %v", first, ok)
		}
		if _, created := save("pm", `{}`, 0); created {
			h.t.Fatal("a create landed over stored instructions")
		}
		s, revision, found, err := h.f.SeatInstructions(h.ctx, "pm")
		if err != nil || !found || revision != first || string(s.Instructions) != someInstructions {
			h.t.Fatalf("read back %s at %d (%v, %v), want them as saved at %d",
				s.Instructions, revision, found, err, first)
		}
		if _, ok := save("pm", `{}`, first); !ok {
			h.t.Fatal("a save at the revision read was refused")
		}
		if _, ok := save("pm", someInstructions, first); ok {
			h.t.Fatal("a save at a revision that moved landed")
		}
		if _, _, found, _ := h.f.SeatInstructions(h.ctx, "sre"); found {
			h.t.Fatal("one seat's save gave another instructions")
		}
	}},

	{name: "instructions the contract refuses are never stored", fn: func(h *fleetHarness) {
		for name, doc := range map[string]string{
			"an array":       `[]`,
			"null":           `null`,
			"not JSON":       `{"text":`,
			"past the bound": `{"text":"` + strings.Repeat("x", coord.MaxAssistantInstructionsBytes) + `"}`,
		} {
			_, ok, err := h.f.SaveSeatInstructions(h.ctx, "pm", json.RawMessage(doc), h.now(), 0)
			if !errors.Is(err, coord.ErrInvalidAssistantInstructions) || ok {
				h.t.Errorf("%s saved (%v, %v), want ErrInvalidAssistantInstructions", name, ok, err)
			}
		}
		if _, _, err := h.f.SaveSeatInstructions(h.ctx, "", json.RawMessage(`{}`), h.now(), 0); err == nil {
			h.t.Error("instructions were saved for no seat")
		}
	}},

	{name: "instructions are forgotten only once their seat is absent long enough, keeping the stamp across saves", fn: func(h *fleetHarness) {
		at := h.now().Add(-time.Hour)
		first, _, err := h.f.SaveSeatInstructions(h.ctx, "pm", json.RawMessage(someInstructions), h.now(), 0)
		if err != nil {
			h.t.Fatal(err)
		}
		if gone, err := h.f.ForgetSeatInstructions(h.ctx, "pm", h.now()); err != nil || gone {
			h.t.Fatalf("unstamped instructions forgotten (%v, %v)", gone, err)
		}
		if exists, err := h.f.SetSeatInstructionsAbsence(h.ctx, "pm", at); err != nil || !exists {
			h.t.Fatalf("SetSeatInstructionsAbsence = (%v, %v)", exists, err)
		}
		_, stamped, _, _ := h.f.SeatInstructions(h.ctx, "pm")
		if stamped <= first {
			h.t.Fatalf("the stamp left the revision at %d", stamped)
		}
		if _, ok, err := h.f.SaveSeatInstructions(h.ctx, "pm", json.RawMessage(`{}`), h.now(), stamped); err != nil || !ok {
			h.t.Fatalf("a save at the stamped revision = (%v, %v)", ok, err)
		}
		owners, err := h.f.SeatInstructionsOwners(h.ctx)
		if want := []coord.SeatInstructionsOwner{{Handle: "pm", AbsentSince: at}}; err != nil || !slices.Equal(owners, want) {
			h.t.Fatalf("owners read %v (%v), want %v", owners, err, want)
		}
		if gone, err := h.f.ForgetSeatInstructions(h.ctx, "pm", at.Add(-time.Second)); err != nil || gone {
			h.t.Fatalf("instructions absent since after the bound forgotten (%v, %v)", gone, err)
		}
		if gone, err := h.f.ForgetSeatInstructions(h.ctx, "pm", at); err != nil || !gone {
			h.t.Fatalf("instructions absent since the bound kept (%v, %v)", gone, err)
		}
		if _, _, found, _ := h.f.SeatInstructions(h.ctx, "pm"); found {
			h.t.Fatal("forgotten instructions still read")
		}
	}},
}
