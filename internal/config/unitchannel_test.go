package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
)

// EVERY UNIT CHANNEL CONFIG ACCEPTS IS A NAME CHATLET ACCEPTS.
//
// The grammar is written twice (neither package may import the other): here,
// where an author is refused while typing, and in internal/chatlet, which opens
// the unit's room. Wider here, and a company validates but the room is never
// created, logging `chat_unit_room_not_reconciled` at every apply.
//
// ONE DIRECTION, per roles.go: unitChannel is the INTERSECTION of the
// Mattermost, Slack and Chatlet grammars, so a chart survives a chat backend
// change; accepting fewer names than chat does is not a defect. EVERY ONE IS
// ITS OWN ADDRESS: [chatlet.NormalizeName] leaves it unchanged. Probes are
// built, not listed: every byte class at the start and middle, both length edges.
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
