package catalog

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
)

// A GENERIC CHANGE IS NAMED FOR WHAT IT DOES TO WHAT THE PERSON CALLS IT: a card
// titled by method asks a founder to review JSON, so one rule serves every
// route, from the diff its rows are drawn from.
//   - The NOUN is [resources]' where the path says it wrong, else the path's
//     ([RequestTitle]).
//   - A list of named members is titled for those a change adds or removes
//     ("Add seat Technical Writer"); any other change by verb and record name.
//   - A POST is titled by its declared [authz.Effect] ([called], [acted]); a
//     record is named where its path cannot say it ([generic.named]), never by
//     an opaque id. DESTRUCTIVE ([wire.RiskDestructive]): see [resource.Goes].
// Nothing here is the model's: risk comes from the route's declaration and its
// verb's row, names from the representation and a read of what it acts on.

// resource is what a person calls the records one route family serves.
type resource struct {
	// Path is the route's path pattern, as the table holds it.
	Path string
	// Noun is one record, in the lower case a title says it in ("seat").
	Noun string
	// Kind is the dashboard object one record here is, and ID the path
	// wildcard holding its id: its card names it as a [wire.Ref].
	Kind wire.ResourceKind
	ID   string
	// Record is where a read's answer holds the record's identity (its id,
	// title, subtitle and page), so a generic read of it is its typed card.
	Record *cards.Entity
	// Members are the record's lists of named members, by field, each with
	// what one member is ("roles" holds seats).
	Members map[string]memberList
	// Title, where set, names a change the diff cannot, from what it sends
	// and what it acts on ([subject]): a create whose body names what it
	// creates by reference (a brand kit's version), an erase by how many
	// messages it takes from which room.
	Title func(s subject) (title, verb string)
	// Goes, where set, says what a change here takes away for good, the
	// sentence its card leads with, or "" for one that takes nothing (a
	// room's horizon set to keep everything): what the card's risk is
	// decided by on a route whose verb nothing undoes.
	//
	// A change is DESTRUCTIVE ([wire.RiskDestructive]) when it deletes, removes
	// a member or fields of what it replaces, or NOTHING UNDOES it whatever its
	// method: a verb row marked irreversible ([authz.IrreversibleOf]: a purge,
	// a room's horizon, which is a PUT). Its summary says what goes and its
	// note whether it can be reverted, so a removal is never one struck-through
	// row among sixty.
	Goes func(s subject) wire.Phrase
	// Of is the path of the record a change here is about, where that is not
	// what its own path names: a room's horizon is the room's. A best-effort
	// read of it names the record on the card ([generic.named]).
	Of string
	// Named, where set, is what a read of a record here calls it, where its
	// own name field does not say it as people do ("#general").
	Named func(doc any) string
	// Action, for a POST that does not create, is what it does to what its
	// path names, in the words its title starts with ("change the
	// dependencies of"), where the path's last segment does not say it
	// ([acted]); Noun is then what it acts on, where the path says that
	// wrong.
	Action string
	// Summary and Note, where set, are said of every change on the route.
	Summary, Note string
	// Singleton is a resource there is one of, titled by its noun alone:
	// the company configuration is not "company configuration Acme".
	Singleton bool
}

// memberList is what each member of a record's list is: its noun and its kind.
type memberList struct {
	Noun string
	Kind wire.ResourceKind
}

// The member lists a configuration document holds.
var (
	seatMembers = memberList{Noun: "seat", Kind: wire.ResourceSeat}
	unitMembers = memberList{Noun: "unit", Kind: wire.ResourceUnit}
)

// historyNote is what a change to a route that keeps a revision history can
// say about being undone: every revision stays, and any one of them can be
// reverted to (`POST /config/revisions/{id}/revert`).
const historyNote = "The previous version stays in the configuration history, so " +
	"this can be reverted."

// noUndoNote is what a destructive change to a route that keeps no history
// says: the assistant has no undo for it.
const noUndoNote = "This cannot be undone from Ask Crewlet."

// readsNote is what a POST that changes nothing says ([authz.EffectReads]):
// the person approves a question, not a change.
const readsNote = "This changes nothing: it only asks."

// resources are the route families whose path does not say what a person
// calls them. A route not here is named by its path ([RequestTitle]), which
// is right for most: a project is a project.
var resources = []resource{
	{Path: "/config", Noun: "company configuration", Singleton: true,
		Members: map[string]memberList{"roles": seatMembers, "units": unitMembers}},
	{Path: "/config/roles/{id}", Noun: "seat", Kind: wire.ResourceSeat, ID: "id"},
	{Path: "/config/units/{id}", Noun: "unit", Kind: wire.ResourceUnit, ID: "id",
		Members: map[string]memberList{"roles": seatMembers},
		Record:  &cards.Entity{ID: "id", Title: "name", Subtitle: "mission"}},
	// THE RECORDS A READ IS DRAWN AS by kind, where its answer is the record.
	{Path: "/artifacts/{id}", Noun: "artifact", Kind: wire.ResourceArtifact, ID: "id",
		Record: &cards.Entity{ID: "id", Title: "title", Subtitle: "kind"}},
	{Path: "/agents/{id}", Noun: "agent", Kind: wire.ResourceSeat, ID: "id",
		Record: &cards.Entity{ID: "handle", Title: "role", Subtitle: "handle"}},
	{Path: "/worklet/{id}", Noun: "task", Kind: wire.ResourceTask, ID: "id",
		Record: &cards.Entity{ID: "task.key", Key: "task.key", Title: "task.title"}},
	{Path: "/worklet/projects/{key}", Noun: "project", Kind: wire.ResourceProject, ID: "key",
		Record: &cards.Entity{ID: "key", Key: "key", Title: "name", Subtitle: "purpose"}},
	{Path: "/doclet/pages/{id}", Noun: "page", Kind: wire.ResourcePage, ID: "id",
		Record: &cards.Entity{Root: "page", ID: "id", Title: "title"}},
	{Path: "/atlas/{id}", Noun: "atlas entry", Kind: wire.ResourceAtlasEntry, ID: "id",
		Record: &cards.Entity{Root: "entry", ID: "id", Title: "name", Subtitle: "ref.kind"}},
	{Path: "/config/llm-providers/{id}", Noun: "model provider"},
	{Path: "/config/mcp-servers/{id}", Noun: "MCP server"},
	{Path: "/config/charter", Noun: "company charter", Singleton: true},
	{Path: "/brand/adoptions", Noun: "brand", Singleton: true,
		Title: func(s subject) (string, string) {
			if v := cards.Text(s.body["version"]); v != "" {
				return "Adopt version " + v + " of the brand kit", "adopt"
			}
			return "Adopt a brand kit version", "adopt"
		},
		Summary: "Everyone in the company, every page and every agent uses the adopted " +
			"version as the company's brand.",
		Note: "Adopting another version, or withdrawing the brand, changes it again."},
	{Path: "/brand/adoptions/current", Noun: "company brand", Singleton: true,
		Summary: "The company has no brand until another version is adopted."},
	// A ROOM IS CALLED BY ITS NAME, as every sentence anybody writes about it
	// does ("#general"), never by its id; a direct conversation has none.
	{Path: "/chatlet/channels/{channel_id}", Noun: "room", Kind: wire.ResourceRoom,
		ID: "channel_id", Record: &cards.Entity{Root: "channel", ID: "id", Title: "name",
			TitlePrefix: "#", Key: "name", KeyPrefix: "#", Room: "id"},
		Named: func(doc any) string {
			if name := value(doc, "channel.name"); name != "" {
				return "#" + name
			}
			return ""
		}},
	// CHAT'S THREE DESTRUCTIONS, each read from what it sends: an erase's
	// messages in its body, a prune's cutoff in its query, a horizon in its
	// body, each of them about the room ([chatRoom]).
	{Path: "/chatlet/channels/{channel_id}/erase", Of: "/chatlet/channels/{channel_id}",
		Title: chatEraseTitle, Goes: chatEraseGoes},
	{Path: "/chatlet/channels/{channel_id}/prune", Of: "/chatlet/channels/{channel_id}",
		Title: chatPruneTitle, Goes: chatPruneGoes},
	{Path: "/chatlet/channels/{channel_id}/retention", Of: "/chatlet/channels/{channel_id}",
		Title: chatRetentionTitle, Goes: chatRetentionGoes},
	// THE POSTS THAT DO NOT CREATE, where the path's last segment does not
	// say what they do or to what ([acted]).
	{Path: "/config/revisions/{id}/revert", Action: "revert the configuration to"},
	{Path: "/fleet/broker/remove-peer/{peer}", Action: "remove"},
	{Path: "/mcp-servers/{name}/retry", Action: "restart", Noun: "MCP server"},
	{Path: "/mcp-servers/{name}/check", Action: "test", Noun: "MCP server"},
	{Path: "/mcp-servers/{name}/changes", Action: "review the changed tools of", Noun: "MCP server"},
	{Path: "/mcp-checks", Action: "test", Noun: "an MCP server before it is saved", Singleton: true},
	{Path: "/mcp-probe", Action: "check", Noun: "an MCP server's address", Singleton: true},
	{Path: "/worklet/items/{key}/depend", Action: "change the dependencies of"},
	{Path: "/worklet/items/{key}/relate", Action: "change the relations of"},
	{Path: "/worklet/projects/{key}/tags", Action: "change the tags of"},
	{Path: "/worklet/retention/ack", Action: "acknowledge", Noun: "a backup of a log",
		Singleton: true},
	{Path: "/worklet/retention/capacity", Action: "set", Noun: "a log's capacity",
		Singleton: true},
	{Path: "/worklet/retention/maintenance/abandon", Action: "abandon",
		Noun: "a log's maintenance", Singleton: true},
	{Path: "/worklet/retention/maintenance/exclude", Action: "exclude a node from",
		Noun: "a log's maintenance", Singleton: true},
	{Path: "/worklet/retention/maintenance/retire", Action: "retire",
		Noun: "a node id from every capacity window", Singleton: true},
	{Path: "/worklet/retention/reanchor", Action: "re-anchor", Noun: "a log", Singleton: true},
	{Path: "/artifacts/checks", Action: "check", Noun: "an artifact's files", Singleton: true},
	{Path: "/artifacts/folder-moves", Action: "move", Noun: "artifacts into folders",
		Singleton: true},
	{Path: "/artifacts/{id}/work-items", Action: "link a task to", Noun: "artifact"},
	{Path: "/artifacts/{id}/data/writes", Action: "change the data of", Noun: "artifact"},
	{Path: "/artifacts/{id}/data/undo", Action: "undo a change to the data of",
		Noun: "artifact"},
	{Path: "/artifacts/{id}/data/state", Action: "save your view of", Noun: "artifact"},
}

// chatRoom is the room a chat destruction is about, as its card says it: its
// name where a read of it answered one ("#general"), else in neutral words,
// never by its id. A room the person may not read answers exactly as one
// nobody holds, so its card says no more than the read did.
func chatRoom(s subject) string { return firstOf(s.named, "the room") }

// chatRoomRef is [chatRoom] as a sentence names it: the room as a ref where a
// read named it.
func chatRoomRef(s subject) wire.Phrase {
	if len(s.ref) > 0 {
		return s.ref
	}
	return wire.Plain(chatRoom(s))
}

// chatEraseTitle titles an erase by how many messages it takes and from
// where: "Erase 2 messages from #general".
func chatEraseTitle(s subject) (string, string) {
	return "Erase " + erased(s) + " from " + chatRoom(s), "erase"
}

// chatEraseGoes is what an erase takes.
func chatEraseGoes(s subject) wire.Phrase {
	return wire.Plain("This erases " + erased(s) + " from ").Then(chatRoomRef(s)).
		Plain(" for good.")
}

// erased is how many messages an erase's body names, each once, as a phrase.
func erased(s subject) string {
	ids := map[string]bool{}
	list, _ := s.body["message_ids"].([]any)
	for _, id := range list {
		if v := strings.TrimSpace(cards.Text(id)); v != "" {
			ids[v] = true
		}
	}
	switch len(ids) {
	case 0:
		return "messages"
	case 1:
		return "a message"
	}
	return strconv.Itoa(len(ids)) + " messages"
}

// chatPruneTitle titles a prune by the instant everything before it goes:
// "Delete what #general said before 1 January 2031".
func chatPruneTitle(s subject) (string, string) {
	return "Delete what " + chatRoom(s) + " said before " + cutoff(s), "delete"
}

// chatPruneGoes is what a prune takes.
func chatPruneGoes(s subject) wire.Phrase {
	return wire.Plain("This deletes everything said in ").Then(chatRoomRef(s)).
		Plain(" before " + cutoff(s) + " for good.")
}

// cutoff is a prune's `?cutoff=` as a person reads an instant: the date, and
// the time where it is not the start of that day, in the zone it was written
// in.
func cutoff(s subject) string {
	raw := strings.TrimSpace(s.query.Get("cutoff"))
	at, err := time.Parse(time.RFC3339Nano, raw)
	switch {
	case raw == "":
		return "the cutoff"
	case err != nil:
		return oneLineValue(raw)
	case at.Hour() == 0 && at.Minute() == 0 && at.Second() == 0 && at.Nanosecond() == 0:
		return at.Format("2 January 2006")
	}
	return at.Format("2 January 2006, 15:04 MST")
}

// chatRetentionTitle titles a room's horizon by what it keeps: "Keep
// #general's messages for 7 days" or "for ever", or "Return #general to the
// company's setting" where its own is taken back.
func chatRetentionTitle(s subject) (string, string) {
	room := chatRoom(s)
	days, set, ok := retained(s)
	switch {
	case !ok:
		return "Change how long " + room + " keeps messages", "change"
	case !set:
		return "Return " + room + " to the company's setting", "return"
	case days == 0:
		return "Keep " + possessive(room) + " messages for ever", "keep"
	}
	return "Keep " + possessive(room) + " messages for " + daysPhrase(days), "keep"
}

// chatRetentionGoes is what a room's horizon takes: whatever is older than
// what it keeps from now on, which a horizon kept for ever takes nothing of.
func chatRetentionGoes(s subject) wire.Phrase {
	room := chatRoomRef(s)
	days, set, ok := retained(s)
	switch {
	case !ok:
		return wire.Plain("This changes how long ").Then(room).Plain(" keeps messages, " +
			"and what it no longer keeps is deleted for good.")
	case !set:
		return wire.Plain("From now on ").Then(room).Plain(" keeps messages only as long " +
			"as the company's setting does, and anything older is deleted for good.")
	case days == 0:
		return nil
	}
	return wire.Plain("From now on ").Then(room).Plain(" keeps messages for " +
		daysPhrase(days) + ", and anything older is deleted for good.")
}

// retained is a horizon's `retention_days`: a whole number of days, 0
// keeping everything (days, set), or null, which takes the room back to the
// company's (not set). ok is false for anything else, nothing included,
// which the route refuses and the card names no horizon for.
func retained(s subject) (days int, set, ok bool) {
	v, stated := s.body["retention_days"]
	switch {
	case !stated:
		return 0, false, false
	case v == nil:
		return 0, false, true
	}
	days, err := strconv.Atoi(cards.Text(v))
	return days, true, err == nil && days >= 0
}

// daysPhrase is a number of days as a sentence says it.
func daysPhrase(days int) string {
	if days == 1 {
		return "1 day"
	}
	return strconv.Itoa(days) + " days"
}

// possessive is a name with "'s" after it.
func possessive(name string) string { return name + "'s" }

// resourceOf is the resource a route's path names, if [resources] holds it.
func resourceOf(route Route) (resource, bool) { return resourceAt(route.Path()) }

// resourceAt is the resource a path pattern names, if [resources] holds it.
func resourceAt(path string) (resource, bool) {
	i := slices.IndexFunc(resources, func(r resource) bool { return r.Path == path })
	if i < 0 {
		return resource{}, false
	}
	return resources[i], true
}

// nameFields are the fields a record names itself by, for a person, in the
// order they are asked for.
var nameFields = []string{"name", "title", "label"}

// recordName is the name a record goes by: its first name field, else its
// identity ([identityKeys]), never an opaque id ([opaqueID]), which names
// nothing to anybody, so a record known only by one is named some other way.
func recordName(v any) string {
	record, _ := v.(map[string]any)
	for _, k := range append(slices.Clone(nameFields), identityKeys...) {
		s := strings.TrimSpace(cards.Text(record[k]))
		if s != "" && isPlainText(record[k]) && !opaqueID(s) {
			return oneLineValue(s)
		}
	}
	return ""
}

func isPlainText(v any) bool { _, ok := v.(string); return ok }

// representationName is the name a read's answer gives the record it is
// about: the answer's own ([recordName]), else that of the one record it
// holds under a field, as a detail answer carries its record beside facts
// about it (`{"page": {"title": ...}, "revision": 4}`). Never a body's: a
// change's body is the record itself, and a field of it holding an object
// is a setting, not the record.
func representationName(doc any) string {
	if name := recordName(doc); name != "" {
		return name
	}
	answer, _ := doc.(map[string]any)
	var held []any
	for _, v := range answer {
		if isRecord(v) {
			held = append(held, v)
		}
	}
	if len(held) != 1 {
		return ""
	}
	return recordName(held[0])
}

// described is a generic change as the person reads it: its title and
// approve label, what it does where the title does not say it, whether it
// can be reverted, its risk, and the record it acts on ("item ENG-1"), the
// record a ref where its kind is known.
type described struct {
	Title, ApproveLabel string
	Summary, Note       wire.Phrase
	Object              wire.Phrase
	Risk                wire.ProposalRisk
}

// subject is what a change is shown against beyond its route and its diff:
// the body and the query it sends, the name a best-effort read of the record
// it acts on answered ("#general"), or "" where nothing could be read, and
// that record as a ref where its kind is known.
type subject struct {
	body  map[string]any
	query url.Values
	named string
	ref   wire.Phrase
}

// describe names the change req makes on route to the resource read as before
// (nil where nothing was read, or nothing is there), leaving it as after (nil
// for a delete); s is what it sends and what it acts on, removals are the
// fields a replace removes, address the path's values and l the labeller its
// rows were drawn with.
//
// NEVER AN OPAQUE ID: a record is named by what it is called (its name field,
// a best-effort read of it), else by a key a person types (a task's ENG-1),
// else in neutral words ("the page"), never by the id its path carries.
func describe(route Route, req string, creates bool, before, after any, s subject,
	removals []string, address map[string]string, l labeller) described {

	if route.Effect == authz.EffectCalls {
		return called(address["tool"])
	}
	res, known := resourceOf(route)
	noun := res.Noun
	if !known || noun == "" {
		noun = lowerFirst(cards.Singular(pathNoun(route.Path())))
	}
	name := ""
	if !res.Singleton {
		name = firstOf(recordName(after), representationName(before), s.named,
			addressValue(address))
	}
	own := refOf(res, address, name)
	if rec, ok := recordOf(route, req); ok && rec.Path != res.Path {
		s.ref = refOf(rec, address, s.named)
	}
	d := described{Object: withRef(phrased(noun, name), own)}
	var verb, action string
	switch {
	case res.Title != nil:
		d.Title, verb = res.Title(s)
	case req == http.MethodDelete:
		d.Title, verb = titled("Delete", noun, name), "delete"
	case req == http.MethodPost && acts(route.Effect):
		var object string
		action, object = acted(route, res, address, s.named)
		d.Object = withRef(object, s.ref)
		d.Title = capitalised(strings.TrimSpace(action + " " + object))
		verb, _, _ = strings.Cut(action, " ")
		if route.Effect == authz.EffectReads {
			d.Note = wire.Plain(readsNote)
		}
	case req == http.MethodPost || creates:
		d.Title, verb = titled("Create", noun, name), "create"
	default:
		d.Title, verb = titled("Change", noun, name), "save"
		if t, v, ok := memberTitle(res, noun, name, before, after, l); ok {
			d.Title, verb = t, v
		}
	}
	d.ApproveLabel = "Approve and " + verb
	d.Summary = wire.Plain(res.Summary)
	if res.Note != "" {
		d.Note = wire.Plain(res.Note)
	}

	gone, gonePaths := removedMembers(res, before, after, l)
	// WHAT NOTHING UNDOES IS DESTRUCTIVE, by what the route declares it does
	// (a POST that destroys) or by its verb's own row (authz.IrreversibleOf:
	// a purge, a room's horizon); a family the registry reads further says
	// what such a change takes, and one that takes nothing is not.
	irreversible, _ := authz.IrreversibleOf(route.Action)
	destroys := irreversible || (req == http.MethodPost && route.Effect == authz.EffectDestroys)
	var goes wire.Phrase
	if res.Goes != nil {
		goes = res.Goes(s)
		destroys = destroys && len(goes) > 0
	}
	destructive := req == http.MethodDelete || destroys || len(gone) > 0 || len(removals) > 0
	if !destructive {
		return d
	}
	d.Risk = wire.RiskDestructive
	// WHAT GOES LEADS THE CARD, above every row (ADR-0055): the members a
	// change removes by name, then every other field a replace removes,
	// which only the person's own request may (the call's `removes`).
	var said []wire.Phrase
	switch {
	case len(goes) > 0:
		said = append(said, goes)
	case req == http.MethodDelete:
		said = append(said, wire.Plain("This deletes ").Then(d.Object).Plain("."))
	case destroys && action != "":
		said = append(said, wire.Plain("This "+thirdPerson(action)+" ").Then(d.Object).Plain("."))
	case destroys:
		said = append(said, wire.Plain("This changes ").Then(d.Object).Plain(" for good."))
	case len(gone) > 0:
		from := wire.Phrase(nil)
		if name != "" {
			from = wire.Plain(" from ").Then(withRef(noun+" "+name, own))
		}
		said = append(said, wire.Plain("This removes ").Then(andPhrases(gone)).Then(from).Plain("."))
	}
	var fields []string
	for _, r := range removals {
		if !covered(r, gonePaths) {
			fields = append(fields, l.label(r))
		}
	}
	if len(fields) > 0 {
		also := "This also removes "
		if len(said) == 0 {
			also = "This removes "
		}
		said = append(said, wire.Plain(also+inWords(fields)+". Approve only if you asked for that."))
	}
	if len(said) > 0 {
		d.Summary = wire.Join(append(said, d.Summary), " ")
		if n := len(d.Summary); n > 0 && d.Summary[n-1].Text == " " {
			d.Summary = d.Summary[:n-1]
		}
	}
	if len(d.Note) == 0 {
		d.Note = wire.Plain(noUndoNote)
		if route.Summary {
			d.Note = wire.Plain(historyNote)
		}
	}
	return d
}

// readRecord is the identity of the one record a read on route answers, where
// [resources] says its kind: its id, title, subtitle and page, so its card is
// that record's ([cards.Infer]); nil for any other route.
func readRecord(route string) *cards.Entity {
	method, path, _ := strings.Cut(strings.TrimSpace(route), " ")
	res, ok := resourceAt(path)
	if method != http.MethodGet || !ok || res.Record == nil || res.Kind == "" {
		return nil
	}
	e := *res.Record
	e.Kind = res.Kind
	if screen, param, ok := refScreen(res.Kind); ok {
		e.Open = &cards.Open{Screen: screen, Params: map[string]string{param: e.ID}}
	}
	return &e
}

// recordOf is the record a change on route is about where [resources] says
// its kind: the route's own, else the one its family is about ([resource.Of])
// or a POST acts on ([actedOn]).
func recordOf(route Route, method string) (resource, bool) {
	res, _ := resourceOf(route)
	if res.Kind != "" {
		return res, true
	}
	path := res.Of
	if path == "" && method == http.MethodPost && acts(route.Effect) {
		path = actedOn(route.Path())
	}
	rec, ok := resourceAt(path)
	return rec, ok && path != "" && rec.Kind != ""
}

// refOf is res's record at address as a ref, shown as name; nil where its
// kind, its id or a name a person reads is unknown, so no ref shows an id.
func refOf(res resource, address map[string]string, name string) wire.Phrase {
	id := address[res.ID]
	if res.Kind == "" || res.ID == "" || id == "" || name == "" {
		return nil
	}
	return naming(res.Kind, id, name)
}

// withRef is words that end in a record's name, the name as ref; the words
// alone where ref is nil or they do not end in its name.
func withRef(words string, ref wire.Phrase) wire.Phrase {
	name := ref.Text()
	if len(ref) == 0 || !strings.HasSuffix(words, name) {
		return wire.Plain(words)
	}
	return wire.Plain(strings.TrimSuffix(words, name)).Then(ref)
}

// firstOf is the first of values that says something, or "".
func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// phrased is a record as a sentence names it: its noun and its name ("item
// ENG-1"), or the noun alone where it has no name a person reads ("the
// page"), never an opaque id in its place.
func phrased(noun, name string) string {
	if name == "" {
		return strings.TrimSpace("the " + noun)
	}
	return strings.TrimSpace(noun + " " + name)
}

// called is a call on the act route as the person reads it: titled by the tool
// it calls, said as a person writes it ("React to message", "Open DM"), and
// approved by the tool's verb, the first word of every operator tool's name
// ("Approve and react").
//
// A CALL, NOT A CREATE: the act route's path names a tool, which alone decides
// whether the call adds, rewrites or takes away, so titling it a create would
// tell the person a replacing tool only adds. The preview reads nothing for a
// call, so nothing about what it removes is computed here: a write whose effect
// turns on what is there is a curated write, whose preview reads it, and a
// call one of them makes is refused ([curatedCall]).
func called(tool string) described {
	verb, _, _ := strings.Cut(tool, "_")
	return described{Title: RequestTitle(ActRoute, map[string]string{"tool": tool}),
		ApproveLabel: "Approve and " + verb}
}

// acts reports a POST that does not create: one that rewrites, destroys or
// only reads what its path names, which [acted] titles.
func acts(e authz.Effect) bool {
	return e == authz.EffectRewrites || e == authz.EffectDestroys || e == authz.EffectReads
}

// acted is a POST that does not create as its title says it: the action its
// route takes and what it takes it on ("purge", "item ENG-1"). The action is
// the registry's where the path's last segment does not say it
// ([resource.Action]), else that segment in words. What it acts on is the
// registry's noun where it names one, else the wildcard the action segment
// picks ("node n-2"), else the record the path names before it
// ([pathObject]); named is what a best-effort read of that record called it,
// which names it before any value of its path does.
func acted(route Route, res resource, address map[string]string, named string) (action,
	object string) {

	segments := strings.Split(strings.Trim(route.Path(), "/"), "/")
	at := lastLiteral(segments)
	action = res.Action
	if action == "" && at >= 0 {
		action = lowerFirst(cards.Humanise(segments[at]))
	}
	switch {
	case res.Noun != "" && res.Singleton:
		object = res.Noun
	case res.Noun != "":
		object = phrased(res.Noun, firstOf(named, addressValue(address)))
	case at >= 0 && at+1 < len(segments):
		name, _ := wildcardName(segments[at+1])
		object = phrased(lowerFirst(cards.Humanise(name)), firstOf(named, shown(address[name])))
	case at >= 0:
		object = pathObject("/"+strings.Join(segments[:at], "/"), address, named)
	}
	return action, object
}

// pathObject is the record a path pattern names, as a sentence says it: the
// registry's noun where it has one, else the path's last literal segment, in
// the singular where a wildcard picks one of it, with its name ("item
// ENG-1", "revision 12", "objects"): named where a read found one, else the
// value the call filled in where a person reads it ([shown]).
func pathObject(path string, address map[string]string, named string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	at := lastLiteral(segments)
	if at < 0 {
		return ""
	}
	value, picked := "", false
	if at+1 < len(segments) {
		if name, wild := wildcardName(segments[at+1]); wild {
			value, picked = firstOf(named, shown(address[name])), true
		}
	}
	if r, ok := resourceAt(path); ok {
		if r.Singleton {
			return r.Noun
		}
		return phrased(r.Noun, value)
	}
	noun := lowerFirst(cards.Humanise(segments[at]))
	if !picked {
		return noun
	}
	return phrased(cards.Singular(noun), value)
}

// shown is one path value as a card may say it: a key or a handle a person
// types, on one line, and never an opaque id, which reads as nothing to
// anybody and is "" here so the record is named some other way.
func shown(v string) string {
	if opaqueID(v) {
		return ""
	}
	return oneLineValue(v)
}

// opaqueID reports a value that is an id no person reads: a UUID.
func opaqueID(v string) bool {
	_, err := uuid.Parse(strings.TrimSpace(v))
	return err == nil
}

// lastLiteral is the index of a path's last segment that is no wildcard, or -1.
func lastLiteral(segments []string) int {
	for i := len(segments) - 1; i >= 0; i-- {
		if _, wild := wildcardName(segments[i]); !wild && segments[i] != "" {
			return i
		}
	}
	return -1
}

// thirdPerson is an action as a sentence about "this" says it, its first word
// conjugated: "purge" is "purges", "erase messages in" is "erases messages
// in".
func thirdPerson(action string) string {
	verb, rest, _ := strings.Cut(action, " ")
	switch {
	case verb == "":
		return action
	case strings.HasSuffix(verb, "s"), strings.HasSuffix(verb, "x"),
		strings.HasSuffix(verb, "z"), strings.HasSuffix(verb, "ch"),
		strings.HasSuffix(verb, "sh"):
		verb += "es"
	case len(verb) > 1 && strings.HasSuffix(verb, "y") &&
		!strings.ContainsRune("aeiou", rune(verb[len(verb)-2])):
		verb = verb[:len(verb)-1] + "ies"
	default:
		verb += "s"
	}
	return strings.TrimSpace(verb + " " + rest)
}

// titled is a title: the verb, the noun and the record's name ("Change seat QA
// Engineer"), or the verb and the noun where the record has no name.
func titled(verb, noun, name string) string {
	return strings.TrimSpace(verb + " " + noun + " " + name)
}

// addressValue is the record a path names, where it names one a person reads:
// its single path value, unless that is an opaque id ([shown]).
func addressValue(address map[string]string) string {
	if len(address) != 1 {
		return ""
	}
	for _, v := range address {
		return shown(v)
	}
	return ""
}

// pathNoun is the last literal segment of a path pattern, in words: what its
// records are called where [resources] says nothing.
func pathNoun(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if _, wild := wildcardName(segments[i]); !wild && segments[i] != "" {
			return cards.Humanise(segments[i])
		}
	}
	return "Record"
}

// memberTitle titles a change by the members it adds or removes, where the
// resource holds named members and the change adds or removes some of one
// kind and nothing else of the record's own: "Add seat Technical Writer",
// "Remove 2 seats from unit Product". False for any other change.
func memberTitle(res resource, noun, name string, before, after any, l labeller) (string, string, bool) {
	b, _ := before.(map[string]any)
	a, _ := after.(map[string]any)
	if len(res.Members) == 0 || b == nil || a == nil {
		return "", "", false
	}
	// ONLY A CHANGE TO THE MEMBERS: one that also changes the record's own
	// fields is a change to the record, titled as one.
	if !equalJSON(without(b, res.Members), without(a, res.Members)) {
		return "", "", false
	}
	var added, removed []string
	member := ""
	for _, field := range slices.Sorted(maps.Keys(res.Members)) {
		was, _ := b[field].([]any)
		is, _ := a[field].([]any)
		plus, minus := memberMoves(field, was, is, l)
		if len(plus)+len(minus) == 0 {
			continue
		}
		if member != "" {
			return "", "", false
		}
		member, added, removed = res.Members[field].Noun, plus, minus
	}
	switch {
	case member == "" || (len(added) > 0 && len(removed) > 0):
		return "", "", false
	case len(added) == 1:
		return "Add " + member + " " + added[0], "add", true
	case len(removed) == 1:
		return "Remove " + member + " " + removed[0], "remove", true
	case len(added) > 1:
		return "Add " + strconv.Itoa(len(added)) + " " + member + "s to " +
			strings.TrimSpace(noun+" "+name), "add", true
	}
	return "Remove " + strconv.Itoa(len(removed)) + " " + member + "s from " +
		strings.TrimSpace(noun+" "+name), "remove", true
}

// memberMoves are the members a list at field gains and loses, by identity,
// each by the name a person knows it by.
func memberMoves(field string, before, after []any, l labeller) (added, removed []string) {
	was, wasKeyed := keyed(before)
	is, isKeyed := keyed(after)
	if (!wasKeyed && len(before) > 0) || (!isKeyed && len(after) > 0) {
		return nil, nil
	}
	for _, e := range is {
		if find(was, e.id) == nil {
			added = append(added, l.member(join(field, e.id), e.id))
		}
	}
	for _, e := range was {
		if find(is, e.id) == nil {
			removed = append(removed, l.member(join(field, e.id), e.id))
		}
	}
	return added, removed
}

// removedMembers are the named members a change removes from the resource,
// each as its noun and the member as a ref ("seat QA Engineer"), and their
// paths, across every member list it holds.
func removedMembers(res resource, before, after any, l labeller) (names []wire.Phrase,
	paths []string) {

	b, _ := before.(map[string]any)
	a, _ := after.(map[string]any)
	if b == nil {
		return nil, nil
	}
	for _, field := range slices.Sorted(maps.Keys(res.Members)) {
		was, wasKeyed := keyed(asList(b[field]))
		is, isKeyed := keyed(asList(a[field]))
		if !wasKeyed || (!isKeyed && len(asList(a[field])) > 0) {
			continue
		}
		m := res.Members[field]
		for _, e := range was {
			if find(is, e.id) == nil {
				names = append(names, wire.Plain(m.Noun+" ").Then(
					naming(m.Kind, e.id, l.member(join(field, e.id), e.id))))
				paths = append(paths, join(field, e.id))
			}
		}
	}
	return names, paths
}

func asList(v any) []any { list, _ := v.([]any); return list }

// without is a record without the given fields, for comparing what else of
// it a change moves.
func without(record map[string]any, fields map[string]memberList) map[string]any {
	out := make(map[string]any, len(record))
	for k, v := range record {
		if _, member := fields[k]; !member {
			out[k] = v
		}
	}
	return out
}

// labeller names a change's rows as a person reads them: a member of a list
// by the name it carries rather than the identity it is matched by
// ("Technical Writer › Goal", not "Roles › tech writer › goal"), and a field
// that names other members (a seat's `manages`, a unit's `lead`) by their
// names.
type labeller struct {
	// names are the display names of every named list member, by the
	// path a row's field starts with ([join] of the list and the id).
	names map[string]string
	// ids are the same members' names by their identity alone, for the
	// fields that refer to them.
	ids map[string]string
}

// refFields are the fields whose values are other members' identities: a
// seat's `manages` and a unit's `lead` hold handles a person reads as names.
var refFields = []string{"manages", "lead"}

// newLabeller reads the named members of every keyed list in docs.
func newLabeller(docs ...any) labeller {
	l := labeller{names: map[string]string{}, ids: map[string]string{}}
	for _, d := range docs {
		l.walk("", d)
	}
	return l
}

func (l labeller) walk(field string, v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			l.walk(join(field, k), e)
		}
	case []any:
		list, ok := keyed(t)
		if !ok {
			return
		}
		for _, e := range list {
			path := join(field, e.id)
			if name := recordName(e.value); name != "" && name != e.id {
				l.names[path] = name
				l.ids[e.id] = name
			}
			l.walk(path, e.value)
		}
	}
}

// member is the name of the list member at path, else its identity.
func (l labeller) member(path, id string) string {
	if name, ok := l.names[path]; ok {
		return name
	}
	return id
}

// label is a row's field as a person reads it: the named member it lies
// under by its name, and the rest in words ([cards.FieldLabel]).
func (l labeller) label(field string) string {
	best := ""
	for prefix := range l.names {
		if (field == prefix || strings.HasPrefix(field, prefix+".")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return cards.FieldLabel(field)
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(field, best), ".")
	if rest == "" {
		return l.names[best]
	}
	return l.names[best] + " › " + cards.FieldLabel(rest)
}

// value is a row's value as a person reads it: a field that refers to other
// seats by handle names each as a ref by its name ([refFields]).
func (l labeller) value(field, v string) wire.Phrase {
	if !slices.Contains(refFields, lastKey(field)) || v == "" {
		return wire.Plain(v)
	}
	return naming(wire.ResourceSeat, v, firstOf(l.ids[v], v))
}

// items is [labeller.value] over a list's items.
func (l labeller) items(field string, items []string) []wire.Phrase {
	out := make([]wire.Phrase, len(items))
	for i, item := range items {
		out[i] = l.value(field, item)
	}
	return out
}

// lastKey is the last key of a dotted field path.
func lastKey(field string) string {
	if i := strings.LastIndexByte(field, '.'); i >= 0 {
		return field[i+1:]
	}
	return field
}

// moreRows is the row a bounded card counts the rest of its rows in.
func moreRows(n int, technical bool) string {
	line := fmt.Sprintf("%d more fields change.", n)
	if technical {
		line += " Technical details has the whole change."
	}
	return line
}

// capitalisedOf is a phrase whose first run, where it is words, opens with a
// capital, for a value that opens a row.
func capitalisedOf(p wire.Phrase) wire.Phrase {
	if len(p) == 0 || p[0].Ref != nil {
		return p
	}
	return wire.Plain(capitalised(p[0].Text)).Then(p[1:])
}

// capitalised is s with its first letter in capitals, for a value that opens
// a row.
func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
