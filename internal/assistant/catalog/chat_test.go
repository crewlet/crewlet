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
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
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
// tool, drop the membership the card read from the request's expectation,
// let a bare handle reset how a member follows, or add a handle no seat
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
	// THE ROOM AS THE CARD READ IT is the write's expectation, every member's
	// following said, so a change made after the read is refused by the tool.
	asRead := []any{map[string]any{"handle": "pm", "follow_all": true},
		map[string]any{"handle": "ada", "follow_all": false}}
	if !reflect.DeepEqual(sent["expected_members"], asRead) {
		t.Errorf("the request expects the membership %v, want the room as the card read it %v",
			sent["expected_members"], asRead)
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
// three, the two it declares each in a sentence of its own. The preview
// stopped at the first of each, so every fault cost the model an attempt.
// Mutation: return at the first joiner, the first blank handle or the first
// repeat, or at the first removal `removes` names but the change does not
// make, or join who stays and who is not in the room into one list again, and
// this fails.
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
			"`removes` names pm, who stays in #launch. `removes` also names qa, who is "+
				"not in #launch.") ||
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

// A CHAT READ SAYS THE ROOM AS PEOPLE WRITE IT.
//
// A read about one room takes it by its name ("#launch") as by its id, sent
// as the room's questions read it, and its step and its card's title say
// where the read is: "#launch" where the call did, and where the call named it
// by its id, the room as the turn reads it, a direct conversation by the
// people in it beside the reader, never the id; with nothing to read it by,
// "the room". So two direct conversations, which only an id names, are two
// titles and two reads to the drawer. Its card opens the chat screen at the
// room the answer carries, labelled by what it opens; a room is drawn with the
// "#" every screen draws and is keyed by it, so the answer's "#launch" is its
// chip, and the room's card heads a direct conversation, which has no name and
// so no key, by its people, as the rail draws one. Mutation: say the call's id
// in the step, title two conversations alike, open the screen by anything but
// the answer's room, draw or key a room without its "#", key a conversation,
// or head one by its id or by what it is alone, and a case here fails.
func TestAChatReadSaysTheRoomAsPeopleWriteIt(t *testing.T) {
	t.Parallel()
	room := assistanttest.RecordID
	withAda, withEngineer := "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e21",
		"0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e22"
	world := assistanttest.World()
	for id, other := range map[string]string{withAda: "ada", withEngineer: "engineer"} {
		world["/chatlet/channels/"+id] = map[string]any{"channel": map[string]any{"id": id,
			"kind": "dm"}, "members": []any{map[string]any{"handle": "pm"},
			map[string]any{"handle": other}}}
	}
	names := catalog.NewResolver(reader(world))
	said := cards.Options{SeatName: func(h string) (string, bool) {
		return names.SeatName(t.Context(), h)
	}, Room: func(ref string) (cards.Room, bool) { return names.Room(t.Context(), ref) }}
	get, _ := catalog.Lookup("get_chat_room")
	messages, _ := catalog.Lookup("read_chat_messages")
	for _, c := range []struct {
		ref      string
		o        cards.Options
		get, msg string
	}{
		{"#launch", cards.Options{}, "Reading #launch", "Reading the messages, in #launch"},
		{"Launch", cards.Options{}, "Reading #launch", "Reading the messages, in #launch"},
		{room, said, "Reading the room", "Reading the messages, in #launch"},
		{withAda, said, "Reading the room", "Reading the messages, with Ada Lovelace"},
		{withEngineer, said, "Reading the room", "Reading the messages, with Backend engineer"},
		{room, cards.Options{}, "Reading the room", "Reading the messages"},
	} {
		if got := get.StepLabel(map[string]any{"channel": c.ref}, c.o); got != c.get {
			t.Errorf("a read of the room %q is the step %q, want %q", c.ref, got, c.get)
		}
		if got := messages.StepLabel(map[string]any{"channel": c.ref}, c.o); got != c.msg {
			t.Errorf("a read of %q's messages is the step %q, want %q", c.ref, got, c.msg)
		}
	}
	// A PERSON WHOSE OWN SEAT CANNOT BE READ (one the chart no longer holds,
	// which chat still answers as) is still told two conversations apart:
	// each by all of its people.
	unseated := maps.Clone(world)
	delete(unseated, "/viewer")
	blind := catalog.NewResolver(reader(unseated))
	for id, want := range map[string]string{withAda: "Product manager and Ada Lovelace",
		withEngineer: "Product manager and Backend engineer"} {
		if r, ok := blind.Room(t.Context(), id); !ok || r.With != want {
			t.Errorf("a conversation read by a person whose seat cannot be read is said as "+
				"%+v (%t), want with %s", r, ok, want)
		}
	}
	req, err := messages.Request(map[string]any{"channel": "#launch"})
	if err != nil || req.Path != "/chatlet/channels/%23launch/messages" {
		t.Errorf("the messages of #launch are asked as %s (%v)", req.Path, err)
	}

	shape := func(tool catalog.Tool, args catalog.Args, o cards.Options, answer string) wire.Card {
		t.Helper()
		res, err := catalog.Shape(tool, args, exec.Response{Status: http.StatusOK,
			Body: []byte(answer)}, provenance.NewBoundary(), wednesday, o)
		if err != nil || len(res.Cards) == 0 {
			t.Fatalf("%s shaped %+v (%v)", tool.Name, res, err)
		}
		return res.Cards[0]
	}
	card := shape(messages, catalog.Args{"channel": "#launch"}, cards.Options{},
		`{"channel_id":"`+room+`","messages":[{"message":{"id":"m1","author":"pm",`+
			`"body":"Ship it.","created_at":"2031-04-16T14:00:00Z"}}],"next_cursor":"41"}`)
	if table := card.Table; table == nil || table.Title != "Messages, in #launch" ||
		table.Open == nil || table.Open.Href != "#/chatlet/"+room || table.Open.Label != "Open room" {
		t.Errorf("the messages of #launch are carded as %+v", card.Table)
	}
	var titles []string
	for _, id := range []string{withAda, withEngineer} {
		titles = append(titles, shape(messages, catalog.Args{"channel": id}, said,
			`{"channel_id":"`+id+`","messages":[{"message":{"id":"m1","author":"pm",`+
				`"body":"Hi.","created_at":"2031-04-16T14:00:00Z"}}]}`).Table.Title)
	}
	if !slices.Equal(titles, []string{"Messages, with Ada Lovelace",
		"Messages, with Backend engineer"}) {
		t.Errorf("two direct conversations read by their ids are titled %q", titles)
	}
	card = shape(get, catalog.Args{"channel": "#launch"}, cards.Options{}, `{"channel":{"id":"`+
		room+`","kind":"public","name":"launch"}}`)
	if entity := card.Entity; entity == nil || entity.Title != "#launch" ||
		entity.Key != "#launch" || entity.Kind != wire.ResourceRoom {
		t.Errorf("a room is headed %+v", card.Entity)
	}
	rooms, _ := catalog.Lookup("list_chat_rooms")
	card = shape(rooms, catalog.Args{}, cards.Options{}, `{"channels":[{"channel":{"id":"`+room+
		`","kind":"public","name":"launch"},"unread":2},{"channel":{"id":"`+room+
		`","kind":"dm"},"participants":["pm","ada"],"unread":0}]}`)
	if table := card.Table; table == nil || len(table.Rows) != 2 || table.Rows[0][0] != "#launch" ||
		table.Rows[1][0] != nil || table.Rows[1][1] != "pm, ada" {
		t.Errorf("the rail is carded as %+v", card.Table)
	}
	direct := `{"channel":{"id":"` + withAda + `","kind":"dm"},"members":[{"handle":"pm"},` +
		`{"handle":"ada"}]}`
	card = shape(get, catalog.Args{"channel": withAda}, said, direct)
	if entity := card.Entity; entity == nil || entity.Title != "Direct conversation with Ada Lovelace" ||
		entity.Key != "" || entity.Kind != wire.ResourceRoom || len(entity.Items) != 2 ||
		entity.Open == nil || entity.Open.Href != "#/chatlet/"+withAda {
		t.Errorf("a direct conversation is carded as %+v", card.Entity)
	}
	if entity := shape(get, catalog.Args{"channel": withAda}, cards.Options{}, direct).Entity; entity == nil ||
		entity.Title != "Direct conversation" {
		t.Errorf("a direct conversation no read can say again is headed %+v", entity)
	}
}

// A CHAT READ IS TOLD APART BY EVERYTHING IT ASKS: a search by its words and
// whose they are, a thread by its first words, and the rooms by whether they
// are only those with something unread; and it is one page, asked for one
// (ADR-0061). Mutation: drop search_chat's author from its scope, head a
// thread by "Thread" alone or by all of its first message, or let the rooms
// read ask the whole rail, and a case here fails.
func TestAChatReadIsToldApartByWhatItAsks(t *testing.T) {
	t.Parallel()
	search, _ := catalog.Lookup("search_chat")
	if a, b := search.StepLabel(map[string]any{"q": "launch", "author": "ada"}, cards.Options{}),
		search.StepLabel(map[string]any{"q": "launch", "author": "engineer"},
			cards.Options{}); a != "Searching chat, “launch”, by ada" || a == b {
		t.Errorf("one search by two authors is the steps %q and %q", a, b)
	}

	thread, _ := catalog.Lookup("get_chat_thread")
	world := assistanttest.World()
	answer := func(body string) []byte {
		root := world["/chatlet/channels/"+assistanttest.RecordID+"/threads/"+
			assistanttest.RecordID].(map[string]any)["root"].(map[string]any)
		root["message"].(map[string]any)["body"] = body
		raw, err := json.Marshal(world["/chatlet/channels/"+assistanttest.RecordID+"/threads/"+
			assistanttest.RecordID])
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	args := catalog.Args{"channel": assistanttest.RecordID, "message_id": assistanttest.RecordID}
	heading := func(body string) string {
		t.Helper()
		res, err := catalog.Shape(thread, args, exec.Response{Status: http.StatusOK,
			Body: answer(body)}, provenance.NewBoundary(), wednesday, cards.Options{})
		if err != nil || len(res.Cards) == 0 || res.Cards[0].Entity == nil {
			t.Fatalf("a thread starting %q shaped %+v (%v)", body, res.Cards, err)
		}
		return res.Cards[0].Entity.Title
	}
	if got := heading("Can we ship\nthe launch copy?"); got != "Thread: Can we ship the launch copy?" {
		t.Errorf("a thread is headed %q, want its first words on one line", got)
	}
	if got := heading(strings.Repeat("word ", 40)); !strings.HasPrefix(got, "Thread: word word") ||
		!strings.HasSuffix(got, "…") || len(got) > len("Thread: ")+80 {
		t.Errorf("a thread of a long first message is headed %q, want its first words, marked "+
			"where they are cut", got)
	}
	if got := heading(""); got != "Thread" {
		t.Errorf("a thread whose first message was taken back is headed %q", got)
	}

	rooms, _ := catalog.Lookup("list_chat_rooms")
	req, err := rooms.Request(map[string]any{"unread": true})
	if err != nil || req.Query.Get("limit") != "15" || req.Query.Get("unread") != "true" {
		t.Errorf("the rooms with something unread are asked as %s?%s (%v)", req.Path,
			req.Query.Encode(), err)
	}
	if got := rooms.StepLabel(map[string]any{"unread": true}, cards.Options{}); got !=
		"Reading your chat rooms, with unread messages" {
		t.Errorf("the rooms with something unread are the step %q", got)
	}
}
