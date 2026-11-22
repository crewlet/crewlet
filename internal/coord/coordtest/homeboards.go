package coordtest

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// aBoard is a board the contract stores, carrying the characters an HTML
// escaping encoder would rewrite, so a round trip that changed its bytes shows.
const aBoard = `{"widgets":[{"id":"n-1","kind":"note","text":"a <b> & c, ünïcode"}]}`

func (h *fleetHarness) saveHomeBoard(handle, board string, revision uint64) (uint64, bool) {
	h.t.Helper()
	landed, ok, err := h.f.SaveHomeBoard(h.ctx, handle, json.RawMessage(board), h.now(), revision)
	if err != nil {
		h.t.Fatalf("SaveHomeBoard(%s at %d): %v", handle, revision, err)
	}
	return landed, ok
}

func (h *fleetHarness) homeBoard(handle string) (coord.HomeBoard, uint64, bool) {
	h.t.Helper()
	b, revision, found, err := h.f.HomeBoard(h.ctx, handle)
	if err != nil {
		h.t.Fatalf("HomeBoard(%s): %v", handle, err)
	}
	return b, revision, found
}

func (h *fleetHarness) homeBoardOwners() []coord.HomeBoardOwner {
	h.t.Helper()
	owners, err := h.f.HomeBoardOwners(h.ctx)
	if err != nil {
		h.t.Fatalf("HomeBoardOwners: %v", err)
	}
	return owners
}

func (h *fleetHarness) forgetHomeBoard(handle string, absentBy time.Time, want bool) {
	h.t.Helper()
	gone, err := h.f.ForgetHomeBoard(h.ctx, handle, absentBy)
	if err != nil || gone != want {
		h.t.Fatalf("ForgetHomeBoard(%s, %v) = (%v, %v), want %v", handle, absentBy, gone, err, want)
	}
}

func (h *fleetHarness) stampHomeBoard(handle string, since time.Time) bool {
	h.t.Helper()
	exists, err := h.f.SetHomeBoardAbsence(h.ctx, handle, since)
	if err != nil {
		h.t.Fatalf("SetHomeBoardAbsence(%s, %v): %v", handle, since, err)
	}
	return exists
}

var homeBoardCases = []fleetCase{
	{name: "a board is saved only at the revision read, and reads back in its own bytes", fn: func(h *fleetHarness) {
		if _, revision, found := h.homeBoard("pm"); found || revision != 0 {
			h.t.Fatalf("an unsaved board read as found %v at %d", found, revision)
		}
		first, ok := h.saveHomeBoard("pm", aBoard, 0)
		if !ok || first == 0 {
			h.t.Fatalf("the first save answered %d, %v", first, ok)
		}
		// ZERO IS "NONE IS STORED": a second screen creating one loses.
		if _, ok := h.saveHomeBoard("pm", `{"widgets":[]}`, 0); ok {
			h.t.Fatal("a create landed over a stored board")
		}
		b, revision, found := h.homeBoard("pm")
		if !found || revision != first || string(b.Board) != aBoard || !b.UpdatedAt.Equal(h.now()) {
			h.t.Fatalf("read back %s at %d (%v, saved %v), want the board as saved at %d",
				b.Board, revision, found, b.UpdatedAt, first)
		}
		second, ok := h.saveHomeBoard("pm", `{"widgets":[]}`, first)
		if !ok || second <= first {
			h.t.Fatalf("a save at the revision read answered %d, %v, want past %d", second, ok, first)
		}
		// AN OLDER SCREEN would put back what this one removed.
		if _, ok := h.saveHomeBoard("pm", aBoard, first); ok {
			h.t.Fatal("a save at a revision that moved landed")
		}
		if b, revision, _ := h.homeBoard("pm"); revision != second || string(b.Board) != `{"widgets":[]}` {
			h.t.Fatalf("after a refused save the board reads %s at %d, want the second save's", b.Board, revision)
		}
		if _, revision, found := h.homeBoard("sre"); found || revision != 0 {
			h.t.Fatal("one seat's save gave another a board")
		}
	}},

	{name: "a board the contract refuses is never stored", fn: func(h *fleetHarness) {
		for name, board := range map[string]string{
			"an array":       `[{"widgets":[]}]`,
			"null":           `null`,
			"not JSON":       `{"widgets":`,
			"no widgets":     `{"layout":[]}`,
			"widgets object": `{"widgets":{}}`,
			"past the bound": `{"widgets":["` + strings.Repeat("x", coord.MaxHomeBoardBytes) + `"]}`,
		} {
			_, ok, err := h.f.SaveHomeBoard(h.ctx, "pm", json.RawMessage(board), h.now(), 0)
			if !errors.Is(err, coord.ErrInvalidHomeBoard) || ok {
				h.t.Errorf("%s saved (%v, %v), want ErrInvalidHomeBoard", name, ok, err)
			}
		}
		if got := h.homeBoardOwners(); len(got) != 0 {
			h.t.Fatalf("refused boards were stored: %v", got)
		}
	}},

	{name: "a save keeps the absence stamp, which only the reconcile moves", fn: func(h *fleetHarness) {
		at := h.now().Add(-time.Hour)
		first, _ := h.saveHomeBoard("pm", aBoard, 0)
		if !h.stampHomeBoard("pm", at) {
			h.t.Fatal("SetHomeBoardAbsence found no board just saved")
		}
		// THE STAMP MOVES THE REVISION, so a screen holding the old one rereads.
		_, stamped, _ := h.homeBoard("pm")
		if _, ok := h.saveHomeBoard("pm", `{"widgets":[]}`, first); ok || stamped <= first {
			h.t.Fatalf("a save at the revision before the stamp landed (stamp at %d, read at %d)", stamped, first)
		}
		// A SAVE FROM A NODE STILL ON AN OLDER REVISION is no evidence the
		// seat is back: only the reconcile, reading the fleet's, clears it.
		if _, ok := h.saveHomeBoard("pm", `{"widgets":[]}`, stamped); !ok {
			h.t.Fatal("a save at the stamped revision was refused")
		}
		want := []coord.HomeBoardOwner{{Handle: "pm", AbsentSince: at}}
		if got := h.homeBoardOwners(); !slices.Equal(got, want) {
			h.t.Fatalf("after a save the owners read %v, want %v", got, want)
		}
	}},

	{name: "a board is forgotten only once its absence is as old as the bound", fn: func(h *fleetHarness) {
		at := h.now()
		h.saveHomeBoard("pm", aBoard, 0)
		if got := h.homeBoardOwners(); !slices.Equal(got, []coord.HomeBoardOwner{{Handle: "pm"}}) {
			h.t.Fatalf("HomeBoardOwners answered %v, want pm with no absence", got)
		}
		// NEVER FOUND MISSING: no bound forgets it.
		h.forgetHomeBoard("pm", at.Add(time.Hour), false)
		h.stampHomeBoard("pm", at)
		got := h.homeBoardOwners()
		if len(got) != 1 || !got[0].AbsentSince.Equal(at) || got[0].AbsentSince.Location() != time.UTC {
			h.t.Fatalf("HomeBoardOwners answered %v, want pm absent since %v in UTC", got, at)
		}
		h.forgetHomeBoard("pm", at.Add(-time.Second), false)
		h.forgetHomeBoard("pm", at, true)
		h.forgetHomeBoard("pm", at, false)
		if _, _, found := h.homeBoard("pm"); found {
			h.t.Fatal("a forgotten board still reads")
		}
		if got := h.homeBoardOwners(); len(got) != 0 {
			h.t.Fatalf("a forgotten board is still listed: %v", got)
		}
	}},

	{name: "a board's absence keeps its first sighting, and a return clears it", fn: func(h *fleetHarness) {
		at := h.now()
		h.saveHomeBoard("pm", aBoard, 0)
		h.stampHomeBoard("pm", at)
		h.stampHomeBoard("pm", at.Add(time.Hour))
		if got := h.homeBoardOwners(); len(got) != 1 || !got[0].AbsentSince.Equal(at) {
			h.t.Fatalf("after a second sighting the stamp is %v, want the first, %v", got, at)
		}
		h.stampHomeBoard("pm", time.Time{})
		if got := h.homeBoardOwners(); len(got) != 1 || !got[0].AbsentSince.IsZero() {
			h.t.Fatalf("a cleared absence lists as %v", got)
		}
		// A CLEARED STAMP CANNOT BE FORGOTTEN ON: a peer found the seat back.
		h.forgetHomeBoard("pm", at.Add(2*time.Hour), false)
		if b, _, found := h.homeBoard("pm"); !found || string(b.Board) != aBoard {
			h.t.Fatalf("a seat found back lost its board: %s (%v)", b.Board, found)
		}
	}},

	{name: "an absence is stamped only on a board that exists", fn: func(h *fleetHarness) {
		if h.stampHomeBoard("nobody", h.now()) {
			h.t.Fatal("SetHomeBoardAbsence reported a board for a seat that saved none")
		}
		if got := h.homeBoardOwners(); len(got) != 0 {
			h.t.Fatalf("stamping an absence created a board: %v", got)
		}
	}},

	{name: "a board forgotten and saved again lands past every revision it reported", fn: func(h *fleetHarness) {
		h.saveHomeBoard("pm", aBoard, 0)
		h.stampHomeBoard("pm", h.now())
		_, stamped, _ := h.homeBoard("pm")
		h.forgetHomeBoard("pm", h.now(), true)
		// A SCREEN STILL HOLDING THE OLD REVISION must lose, not win against
		// a board it never read.
		if _, ok := h.saveHomeBoard("pm", aBoard, stamped); ok {
			h.t.Fatal("a save at a forgotten board's revision landed")
		}
		again, ok := h.saveHomeBoard("pm", aBoard, 0)
		if !ok || again <= stamped {
			h.t.Fatalf("the board saved again landed at %d (%v), want past %d", again, ok, stamped)
		}
		if got := h.homeBoardOwners(); len(got) != 1 || !got[0].AbsentSince.IsZero() {
			h.t.Fatalf("the board saved again lists as %v, want no absence", got)
		}
	}},

	{name: "of saves from several nodes at one revision exactly one lands", fn: func(h *fleetHarness) {
		read, _ := h.saveHomeBoard("pm", aBoard, 0)
		nodes := h.nodes(4)
		var wg sync.WaitGroup
		landed := make(chan uint64, len(nodes))
		for i, node := range nodes {
			wg.Go(func() {
				board := `{"widgets":[],"by":` + itoa(i) + `}`
				revision, ok, err := node.SaveHomeBoard(h.ctx, "pm", json.RawMessage(board), h.now(), read)
				if err != nil {
					h.t.Errorf("node %d's save failed: %v", i, err)
				}
				if ok {
					landed <- revision
				}
			})
		}
		wg.Wait()
		close(landed)
		var won []uint64
		for revision := range landed {
			won = append(won, revision)
		}
		if len(won) != 1 {
			h.t.Fatalf("%d saves at revision %d landed (%v), want exactly one", len(won), read, won)
		}
		if _, revision, _ := h.homeBoard("pm"); revision != won[0] {
			h.t.Fatalf("the board reads at %d, want the one save that landed, %d", revision, won[0])
		}
	}},

	{name: "every Home board verb needs its seat", fn: func(h *fleetHarness) {
		if _, _, _, err := h.f.HomeBoard(h.ctx, ""); err == nil {
			h.t.Error("a board was read for no seat")
		}
		if _, _, err := h.f.SaveHomeBoard(h.ctx, "", json.RawMessage(aBoard), h.now(), 0); err == nil {
			h.t.Error("a board was saved for no seat")
		}
		if _, err := h.f.SetHomeBoardAbsence(h.ctx, "", h.now()); err == nil {
			h.t.Error("an absence was stamped for no seat")
		}
		if _, err := h.f.ForgetHomeBoard(h.ctx, "", h.now()); err == nil {
			h.t.Error("a board was forgotten for no seat")
		}
	}},
}
