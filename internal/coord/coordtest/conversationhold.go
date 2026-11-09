package coordtest

import "github.com/crewlet/crewlet/internal/coord"

var conversationHoldCases = []fleetCase{
	{name: "a conversation is held once, listed per seat and lifted only at the revision read", fn: func(h *fleetHarness) {
		swe, qa := seatID("swe"), seatID("qa")
		hold := func(seat string, conversation string) coord.ConversationHold {
			return coord.ConversationHold{Seat: seatID(seat), Conversation: conversation,
				Kind: "context_exhausted", Basis: "b1", At: h.now()}
		}
		for _, bad := range []coord.ConversationHold{
			{Seat: swe, Kind: "context_exhausted", Basis: "b1", At: h.now()},
			{Seat: swe, Conversation: "chat:room/1", Basis: "b1", At: h.now()},
			{Seat: swe, Conversation: "chat:room/1", Kind: "context_exhausted", Basis: "b1"},
		} {
			if ok, err := h.f.CreateConversationHold(h.ctx, bad); err == nil || ok {
				h.t.Errorf("CreateConversationHold(%+v) = (%v, %v), want a refusal", bad, ok, err)
			}
		}
		for i, want := range []bool{true, false} {
			if ok, err := h.f.CreateConversationHold(h.ctx, hold("swe", "chat:room/1")); err != nil || ok != want {
				h.t.Fatalf("hold %d answered %v (%v), want %v", i, ok, err, want)
			}
		}
		for _, h2 := range []coord.ConversationHold{hold("swe", "work:W-7"), hold("qa", "chat:room/1")} {
			if ok, err := h.f.CreateConversationHold(h.ctx, h2); err != nil || !ok {
				h.t.Fatalf("hold %+v: %v (%v)", h2, ok, err)
			}
		}
		held, err := h.f.ConversationHolds(h.ctx, swe)
		if err != nil || len(held) != 2 {
			h.t.Fatalf("swe's holds are %+v (%v), want its two", held, err)
		}
		for _, got := range held {
			if got.Seat != swe || got.Revision == 0 || got.Kind != "context_exhausted" || got.Basis != "b1" {
				h.t.Errorf("a hold read back as %+v", got)
			}
		}
		first := held[0]
		if ok, err := h.f.DeleteConversationHold(h.ctx, swe, first.Conversation, first.Revision+100); err != nil || ok {
			h.t.Fatalf("a lift at a revision that moved answered %v (%v), want refused", ok, err)
		}
		if ok, err := h.f.DeleteConversationHold(h.ctx, swe, first.Conversation, first.Revision); err != nil || !ok {
			h.t.Fatalf("a lift at the revision read answered %v (%v)", ok, err)
		}
		if left, _ := h.f.ConversationHolds(h.ctx, swe); len(left) != 1 || left[0].Conversation == first.Conversation {
			h.t.Errorf("after one lift swe's holds are %+v", left)
		}
		if other, _ := h.f.ConversationHolds(h.ctx, qa); len(other) != 1 {
			h.t.Errorf("lifting swe's hold took qa's: %+v", other)
		}
	}},
}
