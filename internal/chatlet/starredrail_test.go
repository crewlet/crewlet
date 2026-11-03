package chatlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A STARRED ROOM THE READER MAY READ IS IN THEIR RAIL, WHETHER OR NOT THEY ARE IN IT.
//
// The rail listed memberships alone. A team's room is readable by every seat
// and joined only by being in the team, so somebody outside it (the company's
// founder, above the team's lead) found it only through Browse rooms, and the
// star the room's menu offered them kept it nowhere: the Starred section is
// drawn from the same rail. A star is the person's own wish to keep a room to
// hand, so it now lists the room, marked as not theirs, while a private room
// they starred and were then taken out of answers as a room that does not
// exist, uncounted.
//
// Mutation: drop the starred rooms from the rail, mark them as memberships,
// list a private room the reader may not read, count it as unreadable, or
// list stars under With, and a step here goes red.
func TestAStarredRoomTheReaderMayReadIsInTheirRail(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("miles", "jane"))
	team := r.unitRoom("team", "core", "pm", "eng")
	mine := r.room(person("miles"), "general")
	private, err := r.store.CreateChannel(t.Context(), person("jane"), chatlet.NewChannel{
		Name: "deal-room", Kind: chatlet.KindPrivate, Members: members("miles"),
	})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	stars := chatlet.ChannelsQuery{
		Starred: []string{team.Channel.ID, private.Channel.ID, mine.Channel.ID, team.Channel.ID},
	}

	// WHILE HE MAY READ IT, the private room he starred is his and listed.
	before, err := r.reader().Channels(t.Context(), "miles", stars, fresh)
	if err != nil {
		t.Fatalf("read miles's rail before he leaves: %v", err)
	}
	if !slices.ContainsFunc(before.Channels, func(c chatlet.ChannelSummary) bool {
		return c.Channel.ID == private.Channel.ID && c.Member
	}) {
		t.Fatal("the private room miles is in and starred is not in his rail")
	}

	// TAKEN OUT OF IT, he may no longer read it, and the same star lists
	// nothing: the rail asks the one visibility rule, not the star.
	if _, err := r.store.Leave(t.Context(), person("miles"), private.Channel.ID); err != nil {
		t.Fatalf("miles leaves the private room: %v", err)
	}
	r.drain()

	rail, err := r.reader().Channels(t.Context(), "miles", stars, fresh)
	if err != nil {
		t.Fatalf("read miles's rail: %v", err)
	}
	listed := map[string]bool{}
	var names []string
	for _, room := range rail.Channels {
		listed[room.Channel.Name] = room.Member
		names = append(names, room.Channel.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"general", "team"}) {
		t.Fatalf("miles's rail lists %v, want #general (his) and #team (starred), once each, "+
			"and not the private room he left", names)
	}
	if member := listed["team"]; member {
		t.Error("the starred team room is listed as a membership: no message reaches miles from it")
	}
	if member := listed["general"]; !member {
		t.Error("miles's own room, which he also starred, is listed as not his")
	}
	if rail.Unreadable != 0 {
		t.Errorf("the rail counts %d rooms it could not present: the private room miles left "+
			"must answer as one that does not exist", rail.Unreadable)
	}

	common, err := r.reader().Channels(t.Context(), "miles", chatlet.ChannelsQuery{
		With: "pm", Starred: []string{team.Channel.ID},
	}, fresh)
	if err != nil {
		t.Fatalf("read miles's rooms with pm: %v", err)
	}
	if len(common.Channels) != 0 {
		t.Errorf("miles shares %d rooms with pm, want none: a star is not a membership two "+
			"seats share", len(common.Channels))
	}
}
