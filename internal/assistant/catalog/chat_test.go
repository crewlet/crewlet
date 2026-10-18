package catalog_test

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// roomPath and threadPath are where [assistanttest.World] holds its chat room
// and the message in it.
var (
	roomPath   = "/chatlet/channels/" + assistanttest.RecordID
	threadPath = roomPath + "/threads/" + assistanttest.RecordID
)

// chatWorld is [assistanttest.World] with its room's channel and its message
// changed by the two functions, either nil.
func chatWorld(room, message func(map[string]any)) map[string]any {
	world := assistanttest.World()
	if room != nil {
		room(world[roomPath].(map[string]any)["channel"].(map[string]any))
	}
	if message != nil {
		message(world[threadPath].(map[string]any)["root"].(map[string]any))
	}
	return world
}

// sentArgs are the arguments a preview's request carries to its act tool.
func sentArgs(t *testing.T, p proposal.Preview) map[string]any {
	t.Helper()
	var body struct {
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(p.Request.Body, &body); err != nil {
		t.Fatalf("the request body %s: %v", p.Request.Body, err)
	}
	return body.Args
}

// A ROOM'S MEMBERSHIP CHANGE REMOVES ONLY WHAT IT NAMES (ADR-0055).
//
// A membership is stated whole, so the card shows it as the change it makes to
// the room as it is now: who is added and who is removed, each a row, the
// removals said again at the top of the card, which is destructive, and who
// reads the room after, by name, since only its members read a private room
// (D09). A member given by handle alone keeps how they follow the room,
// written into the request; a member the call leaves out without naming them
// in removes refuses the step, naming them, and so does a removal named for a
// member who stays or for somebody not in the room. A member who stays may be
// a seat the chart no longer lists, and one who joins may not. A change that
// moves nobody, a unit's own room and a direct conversation are refused before
// any card. Mutation: drop removalsNamed or the summary, send removes to the
// tool, let a bare handle reset how a member follows, or add a handle no seat
// holds, and a case here fails.
func TestARoomMembershipChangeRemovesOnlyWhatItNames(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	members, _ := catalog.Lookup("set_chat_room_members")
	p, err := members.Preview(t.Context(), read, wednesday,
		assistanttest.ProposalArgs["set_chat_room_members"])
	if err != nil {
		t.Fatal(err)
	}
	got := rows(p)
	if added := got["members.engineer"]; added.Kind != wire.ChangeAdded ||
		added.To != "Backend engineer (engineer)" {
		t.Errorf("the engineer joins as %+v", added)
	}
	if removed := got["members.ada"]; removed.Kind != wire.ChangeRemoved ||
		removed.From != "Ada Lovelace (ada)" {
		t.Errorf("ada leaves as %+v", removed)
	}
	if kept, shown := got["members.pm"]; shown {
		t.Errorf("pm stays as they follow the room and is a row: %+v", kept)
	}
	if !slices.Equal(p.Removes, []string{"Ada Lovelace (ada)"}) ||
		p.Summary != "This removes Ada Lovelace (ada) from #launch." ||
		p.Risk != wire.RiskDestructive {
		t.Errorf("the card leads with %q (%q) at risk %q", p.Summary, p.Removes, p.Risk)
	}
	if !slices.Equal(p.Audience, []string{"Product manager (pm)", "Backend engineer (engineer)"}) {
		t.Errorf("the private room's readers after the change are %q", p.Audience)
	}
	sent := sentArgs(t, p)
	want := []any{map[string]any{"handle": "pm", "follow_all": true},
		map[string]any{"handle": "engineer", "follow_all": false}}
	if !reflect.DeepEqual(sent["members"], want) {
		t.Errorf("the request states the membership %v, want %v", sent["members"], want)
	}
	if _, given := sent[catalog.RemovesArgument]; given {
		t.Errorf("the request sends removes to the tool: %v", sent)
	}

	refusals := map[string]struct {
		args map[string]any
		says string
	}{
		"a member left out and not named": {map[string]any{"channel": assistanttest.RecordID,
			"members": []any{map[string]any{"handle": "pm"}}}, "would remove ada from #launch"},
		"a removal of a member who stays": {map[string]any{"channel": assistanttest.RecordID,
			"members": []any{map[string]any{"handle": "pm"}, map[string]any{"handle": "ada"}},
			"removes": []any{"pm"}}, "names pm, who stays in #launch"},
		"a removal of somebody not in the room": {map[string]any{"channel": assistanttest.RecordID,
			"members": []any{map[string]any{"handle": "pm"}, map[string]any{"handle": "ada"},
				map[string]any{"handle": "engineer"}},
			"removes": []any{"qa"}}, "names qa, who is not in #launch"},
	}
	for name, c := range refusals {
		_, err := members.Preview(t.Context(), read, wednesday, c.args)
		var refused *catalog.PreviewRefusal
		if !errors.As(err, &refused) || refused.Refusal.Code != "undeclared_removal" ||
			!strings.Contains(refused.Refusal.Message, c.says) {
			t.Errorf("%s answered %v, want a refusal saying %q", name, err, c.says)
		}
	}

	p, err = members.Preview(t.Context(), read, wednesday, map[string]any{
		"channel": assistanttest.RecordID, "members": []any{
			map[string]any{"handle": "pm", "follow_all": false}, map[string]any{"handle": "ada"}}})
	if err != nil {
		t.Fatal(err)
	}
	if moved := rows(p)["members.pm"]; moved.Kind != wire.ChangeChanged ||
		moved.From != "Follows the whole room" || moved.To != "Follows only what names them" {
		t.Errorf("pm's following moves as %+v", moved)
	}
	if p.Risk != "" || p.Summary != "" {
		t.Errorf("a change that takes nobody out reads %q at risk %q", p.Summary, p.Risk)
	}
	// A SEAT THE CHART NO LONGER LISTS STAYS AS THE ROOM HOLDS IT, and a
	// handle no seat holds does not join.
	departed := chatWorld(nil, nil)
	departed[roomPath].(map[string]any)["members"] = append(
		departed[roomPath].(map[string]any)["members"].([]any), map[string]any{"handle": "former-qa"})
	if _, err := members.Preview(t.Context(), reader(departed), wednesday, map[string]any{
		"channel": assistanttest.RecordID, "members": []any{map[string]any{"handle": "pm"},
			map[string]any{"handle": "engineer"}, map[string]any{"handle": "former-qa"}},
		"removes": []any{"ada"}}); err != nil {
		t.Errorf("a room holding a seat the chart no longer lists answered %v", err)
	}
	_, err = members.Preview(t.Context(), read, wednesday, map[string]any{
		"channel": assistanttest.RecordID, "members": []any{map[string]any{"handle": "pm"},
			map[string]any{"handle": "ada"}, map[string]any{"handle": "enginer"}}})
	if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(),
		`"enginer" is not one of the seats you can see`) {
		t.Errorf("a handle no seat holds joining answered %v", err)
	}
	if _, err := members.Preview(t.Context(), read, wednesday, map[string]any{
		"channel": assistanttest.RecordID, "members": []any{map[string]any{"handle": "pm"},
			map[string]any{"handle": "ada"}}}); !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Errorf("a membership that moves nobody answered %v", err)
	}
	for _, kind := range []string{"unit", "dm"} {
		world := chatWorld(func(room map[string]any) { room["kind"] = kind }, nil)
		if _, err := members.Preview(t.Context(), reader(world), wednesday,
			assistanttest.ProposalArgs["set_chat_room_members"]); !errors.Is(err,
			catalog.ErrInvalidArguments) {
			t.Errorf("a %s room's membership change answered %v", kind, err)
		}
	}
}

// EVERY FAULT OF A MEMBERSHIP IS ONE REFUSAL (ADR-0059). A membership that
// names two joiners no seat is, a member by no handle and one twice is refused
// once, each fault by where the call wrote it and the seats listed once; and a
// membership that would remove a member it does not declare, and declares a
// member who stays and one who is not in the room, is refused once naming all
// three. The preview stopped at the first of each, so every fault cost the
// model an attempt. Mutation: return at the first joiner, the first blank
// handle or the first repeat, or at the first removal `removes` names but the
// change does not make, and this fails.
func TestEveryFaultOfAMembershipIsOneRefusal(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	members, _ := catalog.Lookup("set_chat_room_members")
	_, err := members.Preview(t.Context(), read, wednesday, map[string]any{
		"channel": assistanttest.RecordID, "members": []any{map[string]any{"handle": "pm"},
			map[string]any{"handle": "ada"}, map[string]any{"handle": "enginer"},
			map[string]any{"handle": " "}, map[string]any{"handle": "qa-lead"},
			map[string]any{"handle": "pm"}}})
	if !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Fatalf("a membership with four faults answered %v", err)
	}
	for _, want := range []string{
		`members[2].handle: "enginer" is not one of the seats you can see`,
		`members[4].handle: "qa-lead" is not one of the seats you can see`,
		"members[3].handle is empty",
		"members[0] and members[5] each name pm",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := strings.Count(err.Error(), "The seats are:"); n != 1 {
		t.Errorf("the refusal lists the seats %d times, want once: %v", n, err)
	}

	_, err = members.Preview(t.Context(), read, wednesday, map[string]any{
		"channel": assistanttest.RecordID, "members": []any{map[string]any{"handle": "pm"}},
		"removes": []any{"pm", "qa"}})
	var refused *catalog.PreviewRefusal
	if !errors.As(err, &refused) || refused.Refusal.Code != "undeclared_removal" ||
		!strings.Contains(refused.Refusal.Message, "would remove ada from #launch") ||
		!strings.Contains(refused.Refusal.Message,
			"`removes` names pm, who stays in #launch, and qa, who is not in #launch") ||
		!strings.Contains(refused.Refusal.Hint, "Include them in members") ||
		!strings.Contains(refused.Refusal.Hint, "List in removes only") {
		t.Errorf("an undeclared removal beside two unmade ones answered %v", err)
	}
}

// A CHAT CHANGE IS BOUND TO THE ROOM OR THE MESSAGE IT WAS SHOWN AGAINST
// (ADR-0051).
//
// A change of a room binds the room as its card shows it, its settings and
// its whole membership, and an edit or a delete binds the message's position.
// Each re-reads as itself until somebody changes what the card showed, and
// then as another, so the approval is refused as stale; a room or a message
// gone since is gone. A message said in the room meanwhile moves the room's
// own revision and changes nothing the card showed, so it leaves the change
// standing: bound to that revision, every change to a room anybody talks in
// would go stale before it was approved. Mutation: bind the room's
// `revision`, bind no revision, or bind an edit to the room rather than the
// message, and a case here fails.
func TestAChatChangeIsBoundToWhatItWasShownAgainst(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	update, _ := catalog.Lookup("update_chat_room")
	p, err := update.Preview(t.Context(), read, wednesday, assistanttest.ProposalArgs["update_chat_room"])
	if err != nil {
		t.Fatal(err)
	}
	bound, err := proposal.ParseRevision(p.SourceRevision)
	if err != nil || bound.Kind != proposal.RevisionRoom || bound.ID != assistanttest.RecordID {
		t.Fatalf("a room's change binds %q (%v), want the room", p.SourceRevision, err)
	}
	spoken := chatWorld(nil, nil)
	spoken[roomPath].(map[string]any)["revision"] = 9
	spoken[roomPath].(map[string]any)["message_seq"] = 13
	reordered := chatWorld(nil, nil)
	members := reordered[roomPath].(map[string]any)["members"].([]any)
	members[0], members[1] = members[1], members[0]
	for name, world := range map[string]map[string]any{
		"unchanged":                         assistanttest.World(),
		"a message said in it":              spoken,
		"its members read in another order": reordered,
	} {
		if now, err := catalog.Revision(t.Context(), reader(world), p.SourceRevision); err != nil ||
			now != p.SourceRevision {
			t.Errorf("a room %s re-reads as %q (%v), want the revision it was shown at %q",
				name, now, err, p.SourceRevision)
		}
	}
	joined := chatWorld(nil, nil)
	joined[roomPath].(map[string]any)["members"] = append(
		joined[roomPath].(map[string]any)["members"].([]any), map[string]any{"handle": "engineer"})
	refollowed := chatWorld(nil, nil)
	refollowed[roomPath].(map[string]any)["members"].([]any)[0].(map[string]any)["follow_all"] = false
	for name, world := range map[string]map[string]any{
		"whose topic moved": chatWorld(func(room map[string]any) { room["topic"] = "Monday" }, nil),
		"archived": chatWorld(func(room map[string]any) {
			room["archived_at"] = "2031-04-02T09:00:00Z"
		}, nil),
		"somebody joined":                   joined,
		"whose member follows it otherwise": refollowed,
	} {
		if now, err := catalog.Revision(t.Context(), reader(world), p.SourceRevision); err != nil ||
			now == p.SourceRevision {
			t.Errorf("a room %s re-reads as %q (%v), the revision it was shown at", name, now, err)
		}
	}

	edit, _ := catalog.Lookup("edit_chat_message")
	p, err = edit.Preview(t.Context(), read, wednesday, assistanttest.ProposalArgs["edit_chat_message"])
	if err != nil {
		t.Fatal(err)
	}
	if want := "message:" + assistanttest.RecordID + "/" + assistanttest.RecordID + "@1:42"; p.SourceRevision != want {
		t.Errorf("an edit binds %q, want the message at its position %q", p.SourceRevision, want)
	}
	if now, err := catalog.Revision(t.Context(), reader(spoken), p.SourceRevision); err != nil ||
		now != p.SourceRevision {
		t.Errorf("a message whose room was spoken in re-reads as %q, %v", now, err)
	}
	edited := chatWorld(nil, func(root map[string]any) {
		root["position"] = map[string]any{"stream": "chat", "generation": 1, "seq": 43}
	})
	if now, err := catalog.Revision(t.Context(), reader(edited), p.SourceRevision); err != nil ||
		now == p.SourceRevision {
		t.Errorf("a message edited since re-reads as %q, %v", now, err)
	}

	gone := assistanttest.World()
	delete(gone, roomPath)
	delete(gone, threadPath)
	for _, tool := range []string{"update_chat_room", "edit_chat_message"} {
		write, _ := catalog.Lookup(tool)
		p, err := write.Preview(t.Context(), read, wednesday, assistanttest.ProposalArgs[tool])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Revision(t.Context(), reader(gone), p.SourceRevision); !errors.Is(err,
			proposal.ErrGone) {
			t.Errorf("%s's object gone re-reads as %v, want gone", tool, err)
		}
	}
}

// A CHAT CARD NAMES THE ROOM AND WHO READS IT (D09).
//
// A post names the room as people do and who will read it, the members of a
// private room by name and the whole company for a public one; a room closed
// to new messages refuses a post before any card. A room the call names as
// people write it ("#launch") is read by that name, and the write is sent to
// the id the read answered. A reply quotes the message it answers, a direct
// message names everyone it reaches, an edit shows the old words beside the
// new, a delete the words it takes down, redacted, and a leave the person
// going, each of those two destructive and saying what goes above the rows. A
// message already taken back refuses an edit and a delete, and somebody else's
// message refuses an edit. Mutation: name the room by its id, send the name
// the call gave in place of the room's id, show the call's own words in place
// of the stored ones, or draw a delete or a leave as an ordinary change, and a
// case here fails.
func TestAChatCardNamesTheRoomAndWhoReadsIt(t *testing.T) {
	t.Parallel()
	preview := func(name string, world map[string]any, args map[string]any) (proposal.Preview, error) {
		tool, _ := catalog.Lookup(name)
		if args == nil {
			args = assistanttest.ProposalArgs[name]
		}
		return tool.Preview(t.Context(), reader(world), wednesday, args)
	}

	p, err := preview("post_chat_message", assistanttest.World(), nil)
	if err != nil || p.Target != "#launch" || rows(p)["room"].To != "#launch" ||
		rows(p)["body"].To != "Ship it on Friday." ||
		!slices.Equal(p.Audience, []string{"Product manager (pm)", "Ada Lovelace (ada)"}) {
		t.Errorf("a post previews %+v, %v", p, err)
	}
	public := chatWorld(func(room map[string]any) { room["kind"] = "public" }, nil)
	if p, err := preview("post_chat_message", public, nil); err != nil ||
		!slices.Equal(p.Audience, []string{"The whole company"}) {
		t.Errorf("a post in a public room names %q, %v", p.Audience, err)
	}
	archived := chatWorld(func(room map[string]any) {
		room["archived_at"] = "2031-04-01T09:00:00Z"
	}, nil)
	if _, err := preview("post_chat_message", archived, nil); !errors.Is(err,
		catalog.ErrInvalidArguments) {
		t.Errorf("a post in an archived room answered %v", err)
	}
	// A ROOM NAMED AS PEOPLE WRITE IT is read by that name, which the
	// room's read resolves as it resolves an id, and the post is sent to the
	// id that read answered, so the card and the write name one room.
	byName := assistanttest.World()
	for _, path := range []string{"%23launch", "launch", strings.ToUpper(assistanttest.RecordID)} {
		byName["/chatlet/channels/"+path] = byName[roomPath]
	}
	for _, named := range []string{"#launch", "launch", strings.ToUpper(assistanttest.RecordID)} {
		p, err := preview("post_chat_message", byName,
			map[string]any{"channel": named, "body": "Ship it."})
		if err != nil || p.Target != "#launch" ||
			sentArgs(t, p)["channel"] != assistanttest.RecordID {
			t.Errorf("a post to the room %q previews %q sending %v (%v), want #launch "+
				"by its id", named, p.Target, p.Request.Body, err)
		}
	}
	// EVERY CHAT WRITE ON A ROOM takes it by name, and reads the message it
	// is about in the room the name resolved to.
	for _, name := range []string{"reply_in_chat_thread", "edit_chat_message",
		"delete_chat_message", "update_chat_room", "set_chat_room_members",
		"leave_chat_room"} {
		args := maps.Clone(assistanttest.ProposalArgs[name])
		args["channel"] = "#launch"
		p, err := preview(name, byName, args)
		if err != nil || sentArgs(t, p)["channel"] != assistanttest.RecordID {
			t.Errorf("%s in the room #launch sends %s (%v), want the room's id", name,
				p.Request.Body, err)
		}
	}
	// A NAME NO ROOM THE PERSON MAY READ ANSWERS is the read's own not
	// found, as an id is.
	_, err = preview("post_chat_message", byName,
		map[string]any{"channel": "#elsewhere", "body": "Ship it."})
	var refused *catalog.PreviewRefusal
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Errorf("a post to a room nobody it may read is called answered %v", err)
	}

	if p, err := preview("reply_in_chat_thread", assistanttest.World(), nil); err != nil ||
		rows(p)["reply_to"].To != "Product manager (pm): The old words." {
		t.Errorf("a reply previews %+v, %v", p, err)
	}
	if p, err := preview("send_direct_message", assistanttest.World(), nil); err != nil ||
		!slices.Equal(rows(p)["to"].Items, []string{"Ada Lovelace (ada)"}) ||
		!slices.Equal(p.Audience, []string{"Product manager (pm)", "Ada Lovelace (ada)"}) {
		t.Errorf("a direct message previews %+v, %v", p, err)
	}

	p, err = preview("edit_chat_message", assistanttest.World(), nil)
	if body := rows(p)["body"]; err != nil || body.Kind != wire.ChangeChanged ||
		body.From != "The old words." || body.To != "The new words." {
		t.Errorf("an edit previews %+v, %v", p.Changes, err)
	}
	if _, shown := rows(p)["links"]; shown {
		t.Error("an edit that leaves the links alone shows them changing")
	}
	theirs := chatWorld(nil, func(root map[string]any) {
		root["message"].(map[string]any)["author"] = "ada"
	})
	if _, err := preview("edit_chat_message", theirs, nil); !errors.Is(err,
		catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), "Ada Lovelace (ada) wrote") {
		t.Errorf("an edit of somebody else's message answered %v", err)
	}

	secret := "ghp_" + strings.Repeat("c", 36)
	leaked := chatWorld(nil, func(root map[string]any) {
		root["message"].(map[string]any)["body"] = "The key is " + secret
	})
	p, err = preview("delete_chat_message", leaked, nil)
	if body := rows(p)["body"]; err != nil || body.Kind != wire.ChangeRemoved ||
		strings.Contains(body.From, secret) || !strings.Contains(body.From, "[REDACTED") {
		t.Errorf("a delete previews %+v, %v", p.Changes, err)
	}
	if links := rows(p)["links"]; links.Kind != wire.ChangeRemoved ||
		!slices.Equal(links.FromItems, []string{"https://example.com/plan"}) {
		t.Errorf("a delete takes down the links as %+v", links)
	}
	if p.Risk != wire.RiskDestructive ||
		p.Summary != "This deletes the message Product manager (pm) wrote in #launch, for "+
			"everyone who can read it." {
		t.Errorf("a delete leads with %q at risk %q", p.Summary, p.Risk)
	}
	taken := chatWorld(nil, func(root map[string]any) {
		message := root["message"].(map[string]any)
		message["body"], message["deleted_at"] = "", "2031-04-02T09:00:00Z"
	})
	for _, name := range []string{"edit_chat_message", "delete_chat_message"} {
		if _, err := preview(name, taken, nil); !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s of a message taken back answered %v", name, err)
		}
	}

	p, err = preview("leave_chat_room", assistanttest.World(), nil)
	if err != nil || rows(p)["member"].From != "Product manager (pm)" ||
		!slices.Equal(p.Audience, []string{"Ada Lovelace (ada)"}) ||
		!strings.Contains(p.Note, "no longer read") ||
		p.Summary != "This removes you from #launch." || p.Risk != wire.RiskDestructive {
		t.Errorf("leaving previews %+v, %v", p, err)
	}
}
