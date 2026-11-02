package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
)

// EVERY UNIT CHANNEL CONFIG ACCEPTS IS A NAME CHATLET ACCEPTS.
//
// The grammar is written twice, because neither package may import the
// other: here, where an author is refused while still typing the org chart,
// and in internal/chatlet, where the engine opens the unit's room under that
// name. Wider here, and a company validates and then the unit's room is never
// created: the chart apply is best effort per unit, so the apply succeeds and
// logs `chat_unit_room_not_reconciled` at every apply and every restart while
// the unit has nowhere to talk. Each copy is tested against itself and nothing
// compared them — which is the shape internal/textcut and internal/whsec each
// record the cost of.
//
// ONE DIRECTION, because that is the rule roles.go states: unitChannel is the
// INTERSECTION of the Mattermost, Slack and Chatlet grammars rather than Chatlet's own,
// so a company keeps its chart when it changes chat
// backend. Today Chatlet's is the narrowest and the two sets are equal, but a
// Chatlet grammar that widened past a vendor's limit must not drag config
// with it — config accepting fewer names than chat does is the intersection
// working, not a defect.
//
// AND EVERY ONE IS ITS OWN ADDRESS: [chatlet.NormalizeName] leaves it unchanged,
// which is what lets config compare two units' channels as written.
//
// The probes are built rather than listed, so a character nobody thought to
// write a case for still has one: every byte class an author reaches for, at
// the start and in the middle, plus both edges of the length bound.
func TestEveryUnitChannelConfigAcceptsIsANameChatletAccepts(t *testing.T) {
	t.Parallel()
	var probes []string
	for _, c := range []string{
		"a", "z", "0", "9", "-", "_", ".", " ", "#", "*", ">", "A", "Z", "é", "@", "/",
	} {
		probes = append(probes, c, c+"room", "room"+c, "ro"+c+"om")
	}
	for _, n := range []int{1, chatlet.MaxChannelName - 1, chatlet.MaxChannelName, chatlet.MaxChannelName + 1} {
		probes = append(probes, strings.Repeat("e", n), "1"+strings.Repeat("-", n-1))
	}

	accepted := 0
	for _, channel := range probes {
		c := unitsCompany(config.Unit{Name: "Core", Channel: channel})
		err := c.Validate()
		if err != nil && strings.Contains(err.Error(), "units[0].channel") {
			continue
		}
		accepted++
		if !chatlet.ValidName(channel) {
			t.Errorf("channel %q: config accepts it and Chatlet refuses it "+
				"as a room name, so the unit's room would never be created", channel)
		}
		// AND IT IS ALREADY THE ADDRESS. Two units sharing a Chatlet room
		// are refused by comparing what they WROTE (validateUnitRooms),
		// which is the comparison the room's address makes only while
		// nothing config accepts is changed by the normalisation.
		if got := chatlet.NormalizeName(channel); got != channel {
			t.Errorf("channel %q: config accepts it and its address is %q, so "+
				"two units naming one room in two spellings would pass the "+
				"shared-room refusal", channel, got)
		}
	}
	// A GRAMMAR THAT REFUSED EVERYTHING would pass the loop above, and the
	// author would have no unit channel at all.
	if accepted == 0 {
		t.Fatal("config refused every probe, so this case measured nothing")
	}
}
