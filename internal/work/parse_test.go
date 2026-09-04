package work_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/work"
)

// registry builds a party registry over a small org, so routing is tested
// against the same resolution the engine uses.
func registry(t *testing.T) *notify.Registry {
	t.Helper()
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Engineering", Type: org.UnitTypeTeam, Lead: "Lead", Project: "ENG",
			Roles: []*org.Role{
				{Name: "Lead", DeclaredHandle: "lead"},
				{Name: "Engineer", DeclaredHandle: "eng"},
				{Name: "Ops", DeclaredHandle: "ops"},
				{
					Name: "Jane Founder", Kind: org.KindHuman, DeclaredHandle: "jane",
					Contact: &org.HumanContact{CrewletOperatorID: "founder"},
				},
			},
		}},
	}
	o.Normalize()
	return notify.NewRegistry(o)
}

// delivery renders a change as the feed relays it.
func delivery(t *testing.T, change work.Change) types.RawWebhook {
	t.Helper()
	data, err := work.EncodeChange(change)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	return types.RawWebhook{Body: body, Handle: change.Actor}
}

func change(kind work.ChangeKind, snap work.Snapshot) work.Change {
	return work.Change{
		V: 1, ID: "c1", ItemID: "i1", Kind: kind, Actor: "jane",
		ActorKind: work.AuthorHuman, Snapshot: snap,
		CreatedAt:    time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		HeadRevision: 41,
	}
}

func snapshot() work.Snapshot {
	return work.Snapshot{
		Key: "ENG-42", Project: "ENG", Title: "ship the thing",
		Status: work.StatusTodo, Assignee: "eng", Reporter: "jane",
		Watchers: []string{"jane", "eng", "ops"},
	}
}

func parse(t *testing.T, p *work.Parser, c work.Change) []notify.Routed {
	t.Helper()
	got, err := p.Parse(t.Context(), delivery(t, c), registry(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return got
}

func routedTo(routed []notify.Routed) map[string]string {
	out := map[string]string{}
	for _, r := range routed {
		out[r.To.Handle] = r.Metadata[work.RoutedViaField]
	}
	return out
}

// THE STRONGEST CLAIM WINS. A mentioned assignee is woken as a MENTION, which
// the prompt renders as a directed ask — waking them as a watcher would tell
// somebody who was asked a question that they are merely following along.
func TestTheStrongestRoutingReasonWins(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	c := change(work.ChangeComment, snapshot())
	c.Mentions = []string{"eng"}

	got := routedTo(parse(t, p, c))
	if got["eng"] != work.ViaMention {
		t.Errorf("the mentioned assignee was routed via %q, want a mention", got["eng"])
	}
	if got["ops"] != work.ViaWatcher {
		t.Errorf("a watcher was routed via %q", got["ops"])
	}
	// THE ACTOR IS NEVER WOKEN about their own change.
	if via, woken := got["jane"]; woken {
		t.Errorf("the actor was woken via %q about their own comment", via)
	}
}

// A MUTED WATCHER IS ALREADY OUT OF THE SNAPSHOT, so the parser cannot
// re-add them: subtracting here as well would be a second implementation of
// the mute rule, and the one that forgot would wake somebody who opted out.
func TestAMutedWatcherIsNeverRouted(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	snap := snapshot()
	snap.Watchers = []string{"jane", "eng"} // ops muted, so already absent
	got := routedTo(parse(t, p, change(work.ChangeStatus, snap)))
	if _, woken := got["ops"]; woken {
		t.Errorf("a muted watcher was woken: %v", got)
	}
}

// AN ITEM MUST NEVER LAND NOWHERE. An unassigned item filed into a project
// produces no error anywhere and is discovered weeks later, so the lead of
// the unit that owns it is woken.
func TestAnItemNamingNobodyReachesTheLead(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	snap := snapshot()
	snap.Assignee, snap.Watchers = "", nil
	got := routedTo(parse(t, p, change(work.ChangeCreated, snap)))
	if got["lead"] != work.ViaLeadFallback {
		t.Fatalf("routed %v, want the lead by fallback", got)
	}
	if len(got) != 1 {
		t.Errorf("the fallback woke %d seats", len(got))
	}
}

// THE ACTOR OWNING THE WORK IS THE ONE CASE WHERE SILENCE IS RIGHT: somebody
// took the item and is working on it in the open.
//
// Keyed on the ASSIGNEE rather than "was every target the actor", because the
// participants rule adds the reporter automatically — the every-target reading
// would call an unassigned item filed by the founder self-service and stay
// silent on the most important case in the tracker.
func TestTheLeadIsNotWokenWhenTheActorOwnsTheItem(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	snap := snapshot()
	snap.Assignee, snap.Watchers = "eng", []string{"eng"}
	c := change(work.ChangeStatus, snap)
	c.Actor = "eng"
	if got := parse(t, p, c); len(got) != 0 {
		t.Errorf("routed %v — the assignee working in the open woke somebody", routedTo(got))
	}

	// BUT AN UNASSIGNED ITEM WHOSE ONLY WATCHER IS ITS REPORTER STILL
	// REACHES THE LEAD. This is the case the other reading gets wrong.
	snap.Assignee, snap.Watchers = "", []string{"jane"}
	c = change(work.ChangeCreated, snap)
	c.Actor = "jane"
	if got := routedTo(parse(t, p, c)); got["lead"] != work.ViaLeadFallback {
		t.Errorf("routed %v, want the lead: an unassigned item its reporter "+
			"filed has landed nowhere", got)
	}
}

// A LEAD FILING IN THEIR OWN PROJECT MUST NOT WAKE THEMSELVES: they answer,
// which wakes them again, for as long as nobody is watching.
func TestALeadDoesNotWakeThemselves(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	snap := snapshot()
	snap.Assignee, snap.Watchers = "", nil
	c := change(work.ChangeCreated, snap)
	c.Actor = "lead"
	if got := parse(t, p, c); len(got) != 0 {
		t.Errorf("the lead woke themselves: %v", routedTo(got))
	}
}

// AN UNMAPPED PROJECT PRODUCES NOTHING rather than a guess. A wake sent to
// whoever happens to be first in a map teaches that seat to ignore the
// tracker.
func TestAnUnmappedProjectRoutesToNobody(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{})
	snap := snapshot()
	snap.Assignee, snap.Watchers = "", nil
	if got := parse(t, p, change(work.ChangeCreated, snap)); len(got) != 0 {
		t.Errorf("an unmapped project routed to %v", routedTo(got))
	}
}

// A HANDLE THAT IS NO LONGER A SEAT IS DROPPED. Somebody who left, or a
// watcher recorded before a rename: a notification addressed to nobody is one
// nothing reports.
func TestAWatcherWhoIsNoLongerASeatIsDropped(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	snap := snapshot()
	snap.Watchers = append(snap.Watchers, "departed")
	got := routedTo(parse(t, p, change(work.ChangeStatus, snap)))
	if _, woken := got["departed"]; woken {
		t.Errorf("a handle that is not a seat was routed to: %v", got)
	}
	if got["ops"] != work.ViaWatcher {
		t.Errorf("dropping one recipient lost the others: %v", got)
	}
}

// THE RECORD TRAVELS, so the routing node reads nothing. It is rarely the
// node running the recipient and is often behind on its projection — routing
// from a local read would use a stale head or block the feed.
func TestRoutingReadsNothingButThePayload(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{
		Leads: work.Leads{"ENG": "lead"}, BaseURL: "https://crewlet.example.com",
	})
	got := parse(t, p, change(work.ChangeAssignee, snapshot()))
	if len(got) == 0 {
		t.Fatal("nothing routed")
	}
	meta := got[0].Metadata
	for key, want := range map[string]string{
		work.MetaItemKey:  "ENG-42",
		work.MetaProject:  "ENG",
		work.MetaStatus:   string(work.StatusTodo),
		work.MetaAssignee: "eng",
		work.MetaTitle:    "ship the thing",
		work.MetaRevision: "41",
		"url":             "https://crewlet.example.com/work/ENG-42",
	} {
		if meta[key] != want {
			t.Errorf("metadata[%q] = %q, want %q", key, meta[key], want)
		}
	}

	// NO BASE URL, NO LINK. A link that opens nothing costs a reader a
	// click to discover, where an absent one costs nothing.
	bare := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	if got := parse(t, bare, change(work.ChangeAssignee, snapshot())); got[0].Metadata["url"] != "" {
		t.Errorf("a deployment with no public URL composed %q", got[0].Metadata["url"])
	}
}

// ONE COPY PER RECIPIENT CARRIES ITS OWN REASON. A shared metadata map would
// make every copy claim whichever reason was written last, so every recipient
// would read the prompt written for one of them.
func TestEachCopyCarriesItsOwnReason(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	c := change(work.ChangeComment, snapshot())
	c.Mentions = []string{"ops"}
	got := parse(t, p, c)

	reasons := map[string]int{}
	for _, r := range got {
		reasons[r.Metadata[work.RoutedViaField]]++
	}
	if reasons[work.ViaMention] != 1 || reasons[work.ViaAssignee] != 1 {
		t.Errorf("reasons = %v, want one mention and one assignee", reasons)
	}
}

// A CHANGE NAMING NO ITEM ROUTES TO NOBODY. Every rule rests on the item —
// the conversation key, the link, the lead lookup — so a record without one
// is a wake with nowhere to look.
func TestAChangeWithNoItemRoutesToNobody(t *testing.T) {
	t.Parallel()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	if got := parse(t, p, change(work.ChangeStatus, work.Snapshot{})); len(got) != 0 {
		t.Errorf("routed %v", routedTo(got))
	}
	// And a delivery with no record at all is an ERROR rather than a
	// silent nothing: it means the feed relayed something it should not
	// have, which nothing else would report.
	if _, err := p.Parse(t.Context(), types.RawWebhook{}, registry(t)); err == nil {
		t.Error("an empty delivery parsed successfully")
	}
}

// ADDRESSED IS THE HALF OF "DID THIS TURN DELIVER" A MODEL CANNOT GET WRONG.
// An addressed turn may not end in silence — to the person who asked, silence
// is indistinguishable from a message that was lost. Marking a watcher copy
// addressed would make every seat post on every change it merely observes.
func TestOnlyAnAskIsAddressed(t *testing.T) {
	t.Parallel()
	for via, want := range map[string]bool{
		work.ViaAssignee:     true,
		work.ViaMention:      true,
		work.ViaWatcher:      false,
		work.ViaLeadFallback: false,
		"":                   false,
		"something-new":      false,
	} {
		if got := work.Addressed(map[string]string{work.RoutedViaField: via}); got != want {
			t.Errorf("Addressed(%q) = %v, want %v", via, got, want)
		}
	}
	if !slices.Equal(work.AddressedKinds(), []string{work.ViaAssignee, work.ViaMention}) {
		t.Errorf("AddressedKinds = %v", work.AddressedKinds())
	}
}

// ---- the prompt ------------------------------------------------------- //

func built(t *testing.T, via string, kind work.ChangeKind, body string) string {
	t.Helper()
	p := work.NewParser(work.ParserOptions{Leads: work.Leads{"ENG": "lead"}})
	c := change(kind, snapshot())
	c.Excerpt = body
	if via == work.ViaMention {
		c.Mentions = []string{"ops"}
	}
	for _, r := range parse(t, p, c) {
		if r.Metadata[work.RoutedViaField] == via {
			return (work.Prompt{}).Build(r.Inbound, registry(t))
		}
	}
	// The lead fallback needs an item naming nobody.
	if via == work.ViaLeadFallback {
		snap := snapshot()
		snap.Assignee, snap.Watchers = "", nil
		c := change(kind, snap)
		c.Excerpt = body
		for _, r := range parse(t, p, c) {
			return (work.Prompt{}).Build(r.Inbound, registry(t))
		}
	}
	t.Fatalf("no copy was routed via %q", via)
	return ""
}

// A LEAD FALLBACK GETS THE DECISION BLOCK AND NO SECOND LIST. The delegate /
// take it / escalate block IS that seat's instruction, and a second set of
// steps under a second heading makes a lead read both and follow neither.
func TestTheLeadFallbackExplainsItselfAndGivesOneList(t *testing.T) {
	t.Parallel()
	got := built(t, work.ViaLeadFallback, work.ChangeCreated, "")
	for _, want := range []string{
		"## Why you received this", "by\n FALLBACK", "**Delegate**",
		"**Take it yourself**", "**Escalate**",
	} {
		if !strings.Contains(got, strings.ReplaceAll(want, "\n ", " ")) {
			t.Errorf("the lead prompt omits %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "## How to handle this") {
		t.Error("the lead prompt carries a second instruction list")
	}

	// AND A DIRECTED ROUTING DOES NOT EXPLAIN ITSELF: being assigned says
	// what is wanted, and the explanation would be noise on every wake.
	assigned := built(t, work.ViaAssignee, work.ChangeAssignee, "")
	if strings.Contains(assigned, "## Why you received this") {
		t.Error("an assignment carries the fallback's explanation")
	}
	if !strings.Contains(assigned, "## How to handle this") {
		t.Error("an assignment carries no instructions")
	}
}

// A WATCHER IS NOT BEING ASKED. Telling one they owe an answer is precisely
// how a tracker fills up with "noted, thanks" — the running-commentary rule
// the same prompt states two lines above.
func TestOnlyADirectAskIsToldItOwesAnAnswer(t *testing.T) {
	t.Parallel()
	owed := "have decided not to act"
	if got := built(t, work.ViaAssignee, work.ChangeAssignee, ""); !strings.Contains(got, owed) {
		t.Errorf("an assignee is not told they owe an answer:\n%s", got)
	}
	if got := built(t, work.ViaMention, work.ChangeComment, "please look"); !strings.Contains(got, owed) {
		t.Errorf("a mentioned seat is not told they owe an answer:\n%s", got)
	}
	if got := built(t, work.ViaWatcher, work.ChangeStatus, "status todo → in_progress"); strings.Contains(got, owed) {
		t.Errorf("a watcher is told they owe an answer:\n%s", got)
	}
}

// A REMOVAL SAYS STOP, and sends nobody to fetch an item that is gone.
func TestARemovalTellsTheSeatToStop(t *testing.T) {
	t.Parallel()
	got := built(t, work.ViaAssignee, work.ChangeRemoved, "")
	if !strings.Contains(got, "no longer exists") || !strings.Contains(got, "stop any in-flight work") {
		t.Errorf("a removal does not say stop:\n%s", got)
	}
	if strings.Contains(got, "## Get full context") {
		t.Error("a removal sends the seat to read an item that is gone")
	}
}

// RECON IS NOT FREE: the flag also suppresses personal-memory filtering and
// episode recall, so setting it on a wake that already carries what it means
// costs the seat its own context for nothing.
func TestReconIsAskedOnlyForAPointer(t *testing.T) {
	t.Parallel()
	prompt := work.Prompt{}
	pointer := notify.Inbound{
		EventType: string(work.ChangeStatus),
		Metadata:  map[string]string{work.MetaItemKey: "ENG-42"},
	}
	if !prompt.RequiresRecon(pointer) {
		t.Error("a bare transition is not treated as a pointer")
	}
	prose := notify.Inbound{
		EventType: string(work.ChangeComment),
		Body:      "here is what I found in the logs",
		Metadata:  map[string]string{work.MetaItemKey: "ENG-42"},
	}
	if prompt.RequiresRecon(prose) {
		t.Error("a comment carrying its own text is treated as a pointer, which " +
			"costs the seat its memory and episode recall for nothing")
	}
	// A comment with no text IS a pointer.
	prose.Body = "   "
	if !prompt.RequiresRecon(prose) {
		t.Error("an empty comment is not treated as a pointer")
	}
}

// A DIGEST KEEPS WHAT PEOPLE SAID AND COLLAPSES WHAT THE LEAD LINE ALREADY
// STATES. Five field-change bodies in one trigger is the same sentence five
// times, burying the comment underneath.
func TestTheDigestKeepsCommentsAndCollapsesDeltas(t *testing.T) {
	t.Parallel()
	prompt := work.Prompt{}
	if got := prompt.DigestBody(string(work.ChangeComment), "something said"); got != "something said" {
		t.Errorf("a comment's body was collapsed: %q", got)
	}
	for _, kind := range []work.ChangeKind{work.ChangeStatus, work.ChangeFields, work.ChangeAssignee} {
		if got := prompt.DigestBody(string(kind), "status todo → in_progress"); got != "" {
			t.Errorf("%s kept its body in a digest: %q", kind, got)
		}
	}
}

// THE CONVERSATION IS THE ITEM, keyed on the human key so a chat thread about
// ENG-42 and the tracker activity on it land in one ledger.
func TestTheConversationIsTheItemKey(t *testing.T) {
	t.Parallel()
	got := (work.Prompt{}).ConversationKey(map[string]string{
		work.MetaItemKey: "ENG-42", work.MetaItemID: "01J8-uuid",
	}, "")
	if got != "ENG-42" {
		t.Errorf("ConversationKey = %q, want the human key", got)
	}
	var prompt work.Prompt
	if prompt.WakesActor(string(work.ChangeAssignee)) {
		t.Error("a tracker change wakes its own actor")
	}
}
