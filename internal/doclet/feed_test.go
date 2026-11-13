package doclet_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/recordseal"
)

// THE WAKE PATH, END TO END: a committed record becomes a notification for the
// people who asked to hear about it.
//
// The feed and the parser are two halves of one contract and neither can be
// checked alone: the feed decides WHAT to relay and puts a record on the
// inbound topic, and the parser decides WHO it is for by reading that same
// record. A test that built the parser's input by hand would have agreed with
// itself while the two halves spoke different shapes — which is precisely what
// they did. The feed relayed the [doclet.MutationRecord] it encodes, the parser
// decoded that body as a change document (the shape the coordination family
// this domain replaced put on the wire), and the two share a version field and
// almost no key: every delivery decoded cleanly into a change with no page id,
// and every page notification in the company was dropped one line later, with
// no error anywhere.
func TestACommittedRecordWakesThePeopleWatchingThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}

	routed := r.route(r.lastRecord(), doclet.Leads{"ENG": "lead"})
	if len(routed) != 1 {
		t.Fatalf("a save on a page carla watches woke %d seats: %+v",
			len(routed), routed)
	}
	got := routed[0]
	if got.To.Handle != "carla" {
		t.Errorf("the wake went to %q, want carla", got.To.Handle)
	}
	if got.Inbound.Metadata[doclet.MetaPageID] != page.Page.ID {
		t.Errorf("the wake names page %q, want %s — a recipient with no page "+
			"id has nothing to open", got.Inbound.Metadata[doclet.MetaPageID],
			page.Page.ID)
	}
	if got.Inbound.Metadata[doclet.MetaChangeKind] != string(doclet.ChangeSaved) {
		t.Errorf("the wake says %q happened",
			got.Inbound.Metadata[doclet.MetaChangeKind])
	}
	if got.Inbound.Metadata[doclet.MetaVersion] != "2" {
		t.Errorf("the wake reports version %q, want 2 — its reader passes that "+
			"number back as `base_version`",
			got.Inbound.Metadata[doclet.MetaVersion])
	}
	if got.Inbound.Metadata[doclet.MetaTitle] != "Runbook" ||
		got.Inbound.Metadata[doclet.MetaContainer] != "ENG" {
		t.Errorf("the wake's address is %q/%q",
			got.Inbound.Metadata[doclet.MetaContainer],
			got.Inbound.Metadata[doclet.MetaTitle])
	}
	if got.WakeID == uuid.Nil {
		t.Error("the wake carries no id, so a redelivery is not recognisable " +
			"as one")
	}
	// AND THE AUTHOR IS NOT WOKEN BY HER OWN WRITE.
	for _, c := range routed {
		if c.To.Handle == "jane" {
			t.Error("jane was woken about her own save")
		}
	}
}

// A MENTION REACHES SOMEBODY WHO DOES NOT WATCH THE PAGE, and a CREATE with
// nobody named falls back to the container's lead.
//
// The two are the parser's other arms, and they read the same relayed record:
// one a set the writer computed (the mentions), one a decision about which
// kinds are worth a lead's attention at all.
func TestAMentionAndTheLeadFallbackBothRouteOffTheRelayedRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})

	// A CREATE nobody was named in reaches the container's lead — a page
	// appearing in a team's space is a fact a lead has a reason to know.
	created := r.route(r.recordAt(1), doclet.Leads{"ENG": "lead"})
	if len(created) != 1 || created[0].To.Handle != "lead" {
		t.Fatalf("a create nobody watches woke %+v, want the container's lead",
			created)
	}
	if created[0].Inbound.Metadata[doclet.RoutedViaField] != doclet.ViaLeadFallback {
		t.Errorf("the lead's copy is routed via %q",
			created[0].Inbound.Metadata[doclet.RoutedViaField])
	}

	if _, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		doclet.NewComment{Body: "@bob is this current?", Mentions: []string{"bob"}}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	routed := r.route(r.lastRecord(), doclet.Leads{"ENG": "lead"})
	handles := make([]string, 0, len(routed))
	for _, c := range routed {
		handles = append(handles, c.To.Handle)
	}
	if !slices.Contains(handles, "bob") {
		t.Fatalf("a mention woke %v, and bob is not among them — a directed "+
			"ask that reaches nobody is the one wake that cannot be missed",
			handles)
	}
	for _, c := range routed {
		if c.To.Handle != "bob" {
			continue
		}
		if c.Inbound.Metadata[doclet.RoutedViaField] != doclet.ViaMention {
			t.Errorf("bob's copy is routed via %q, want a mention",
				c.Inbound.Metadata[doclet.RoutedViaField])
		}
		if !doclet.Addressed(c.Inbound.Metadata) {
			t.Error("a mention is not marked as an ask, so nothing is waiting " +
				"for bob to answer")
		}
	}
}

// A PAGE'S NOTICE SAYS WHO WROTE THE CHANGE, BY THE RECORD'S OWN KIND. A
// person writes as the seat they hold, an agent as its seat, and a credential
// acting as no seat under its own login — three actors spelled alike, of
// which only the kind tells a turn the notice wakes whether a colleague is
// asking. A wake that named only the actor read every one of them as a vendor
// id nobody holds, which is a stranger.
func TestAPageNoticeSaysWhoWroteTheChange(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	for _, c := range []struct {
		actor doclet.Actor
		want  types.NotificationWriter
	}{
		{author("jane"), types.NotificationWriter{Kind: iam.ActorHuman, Seat: "jane"}},
		{agent("swe"), types.NotificationWriter{Kind: iam.ActorAgent, Seat: "swe"}},
		// A CREDENTIAL ACTING AS NO SEAT, by its login and with the apps the
		// node that took its write recorded for it (ADR-0109).
		{doclet.Actor{Handle: "token:ci", Kind: doclet.AuthorOperator,
			Apps: []iam.Grant{iam.GrantDoclet}},
			types.NotificationWriter{Kind: iam.ActorOperator, Login: "token:ci",
				Apps: []iam.Grant{iam.GrantDoclet}}},
	} {
		if _, _, err := r.store.Comment(t.Context(), c.actor, page.Page.ID,
			doclet.NewComment{Body: "@bob is this current?", Mentions: []string{"bob"},
				Asks: ptr(true)}); err != nil {
			t.Fatalf("comment as %s: %v", c.actor.Handle, err)
		}
		var wake *notify.Routed
		for _, routed := range r.route(r.lastRecord(), nil) {
			if routed.To.Handle == "bob" {
				wake = &routed
			}
		}
		if wake == nil {
			t.Fatalf("%s's mention of bob woke nobody", c.actor.Handle)
		}
		if got := wake.Writer; got == nil || !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%s's comment names its writer %+v, want %+v", c.actor.Handle, got, c.want)
		}
	}
}

// lastRecord is the newest record on the log, as the feed would receive it.
func (r *roundTrip) lastRecord() []byte {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return r.recordAt(last)
}

// recordAt is one record's BODY, frame opened.
//
// The wake feed is handed the body rather than the bytes off the stream: the
// framework verifies before any domain decodes, so what travels on a change
// record is what the domain published. A harness handing on the framed bytes
// would be testing a translator against something no translator is ever given.
func (r *roundTrip) recordAt(seq uint64) []byte {
	r.t.Helper()
	_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present %v)", seq, err, ok)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != recordseal.Verified {
		r.t.Fatalf("record %d did not verify: %s", seq, verdict)
	}
	return body
}

// A LEAD WHO IS NO LONGER A SEAT IS NOT THE ANSWER TO "nobody was named".
//
// The lead map is built from one epoch's org and a parser outlives that epoch,
// so a lead whose seat was deleted or whose handle was renamed is a handle the
// company no longer employs. Routing to one produces a wake that resolves to
// nobody three layers away, in the notification service, where the log line
// names a recipient rather than the container whose page activity reached no
// one, and [doclet.Parser] already drops exactly these handles when they are
// mentions or watchers.
func TestTheLeadFallbackDropsAHandleTheCompanyNoLongerEmploys(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	create := r.recordAt(1)

	// THE CONTROL FIRST: the same record, the same lead map, and a roster
	// that still has the lead on it. Without this the case below passes
	// identically if the record simply stopped waking anybody.
	employed := r.routeVia(create, doclet.Leads{"ENG": "lead"}, registry(t, "jane", "lead"))
	if len(employed) != 1 || employed[0].To.Handle != "lead" {
		t.Fatalf("control: a create in a container whose lead is on the roster "+
			"woke %+v, want the lead", employed)
	}

	gone := r.routeVia(create, doclet.Leads{"ENG": "lead"}, registry(t, "jane"))
	if len(gone) != 0 {
		t.Errorf("a create fell back to %+v, and that handle is not a seat — "+
			"the wake can only be reported as undeliverable somewhere that "+
			"cannot name the container it came from", gone)
	}
}

// A RECORD A NEWER BUILD WROTE IS DEFERRED TO A PEER ON THAT BUILD, never
// answered "no wake".
//
// The page's record is real — written through the store, its frame opened —
// and only its version is moved past what this build decodes, so the envelope
// still reads and the rules are a newer build's. Answered "no wake", the feed
// acknowledges it for the whole fleet and the watcher's wake is gone (#838);
// answered with the feed's deferral, a peer on the newer build derives it.
//
// Mutation: answer the future-version arm with no wake, and the deferral is
// gone.
func TestAPageRecordANewerBuildWroteIsDeferredToAPeer(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose", Watchers: []string{"carla"}})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.lastRecord(), &raw); err != nil {
		t.Fatalf("read the record: %v", err)
	}
	raw["v"] = json.RawMessage(strconv.Itoa(doclet.RecordVersion + 1))
	future, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	_, wake, err := doclet.NewTranslator(nil).Translate(t.Context(),
		changefeed.Record{Payload: future, Key: "k"})
	var deferral *changefeed.Deferral
	if wake || !errors.As(err, &deferral) || deferral.Reason != changefeed.DeferNewerRecord {
		t.Fatalf("a newer build's page record answered wake=%v, %v; want it deferred to a "+
			"peer on that build (%s)", wake, err, changefeed.DeferNewerRecord)
	}
}

// A RELAYED PAGE RECORD A NEWER BUILD WROTE IS DEFERRED TO A PEER THAT READS
// IT, on the parser's side of the relay as on the feed's.
//
// The feed on a newer node relays the record's wake, and whichever node wins
// the delivery parses it, so an older node meets the record here too. Refused
// as a payload nobody can read, the notify service acked it on the one group
// every node takes inbound mail from, and the watcher's wake was gone for the
// fleet (crewlet#993); answered with the spine's deferral, the service passes
// it to a peer on the newer build.
//
// Mutation: return the decode's error as it is, and no deferral reaches the
// service.
func TestARelayedPageRecordANewerBuildWroteIsDeferredToAPeer(t *testing.T) {
	t.Parallel()
	future, err := encodeSuiteRecord(string(doclet.KindPage), "page-1", "op-1",
		doclet.RecordVersion+1)
	if err != nil {
		t.Fatalf("encode a newer build's record: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(future, &body); err != nil {
		t.Fatalf("read the record back: %v", err)
	}

	_, err = doclet.NewParser(doclet.ParserOptions{}).Parse(t.Context(),
		types.RawWebhook{Body: body}, nil)
	var deferral *notify.Deferral
	if !errors.As(err, &deferral) || deferral.Reason != notify.DeferNewerRecord {
		t.Fatalf("a relayed page record a newer build wrote answered %v, want it "+
			"deferred to a peer on that build (%s)", err, notify.DeferNewerRecord)
	}
}

// registry is the company roster, holding exactly these handles.
func registry(t *testing.T, handles ...string) *notify.Registry {
	t.Helper()
	o := &org.Organization{Name: "nimbus"}
	for _, handle := range handles {
		o.Roles = append(o.Roles, &org.Role{
			Name: strings.ToUpper(handle), DeclaredHandle: handle,
		})
	}
	o.Normalize()
	return notify.NewRegistry(o, nil)
}

// route runs one committed record through BOTH halves — the feed's translator
// and the parser — because the contract under test is that they agree.
//
// A NIL REGISTRY admits every handle, which is the honest answer for a
// deployment with no organization loaded — and keeps these cases about the two
// halves agreeing rather than about who is on the roster.
func (r *roundTrip) route(payload []byte, leads doclet.Leads) []notify.Routed {
	r.t.Helper()
	return r.routeVia(payload, leads, nil)
}

// routeVia is the same path against a named roster.
func (r *roundTrip) routeVia(payload []byte, leads doclet.Leads,
	reg *notify.Registry) []notify.Routed {

	r.t.Helper()
	return r.routeWith(payload, doclet.ParserOptions{Leads: leads}, reg)
}

// routeWith is the same path through a parser built from opts.
func (r *roundTrip) routeWith(payload []byte, opts doclet.ParserOptions,
	reg *notify.Registry) []notify.Routed {

	r.t.Helper()
	delivery, wake, err := doclet.NewTranslator(nil).Translate(r.t.Context(),
		changefeed.Record{Payload: payload, Key: "k"})
	if err != nil {
		r.t.Fatalf("translate: %v", err)
	}
	if !wake {
		r.t.Fatal("the feed declined to relay a record that carries a " +
			"notification")
	}
	parser := doclet.NewParser(opts)
	routed, err := parser.Parse(r.t.Context(), types.RawWebhook{Body: delivery.Body}, reg)
	if err != nil {
		r.t.Fatalf("parse: %v", err)
	}
	return routed
}

// EVERY WAKE CARRIES ITS RECORD'S OWN INSTANT, a watcher's and the lead's
// fallback alike — see notify.Routed.WakeAt for what the delivery's own clock
// cost a re-run turn.
func TestAPageWakeIsStampedWithItsRecordsInstant(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	watched := r.write(author("jane"), doclet.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), author("jane"), watched.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	directed := r.lastRecord()
	r.write(author("jane"), doclet.NewPage{Container: "ENG", Title: "Nobody watches this"})
	fallback := r.lastRecord()

	for name, payload := range map[string][]byte{
		"a watcher's wake": directed, "the lead's fallback": fallback,
	} {
		record, err := doclet.Decode(payload)
		if err != nil {
			t.Fatalf("decode %s's record: %v", name, err)
		}
		if record.CreatedAt.IsZero() {
			t.Fatalf("the premise: %s's record carries no instant", name)
		}
		routed := r.route(payload, doclet.Leads{"ENG": "lead"})
		if len(routed) == 0 {
			t.Fatalf("%s reached nobody", name)
		}
		for _, got := range routed {
			if !got.WakeAt.Equal(record.CreatedAt) {
				t.Errorf("%s to %s is stamped %s, want the record's %s", name,
					got.To.Handle, got.WakeAt, record.CreatedAt)
			}
		}
	}
}

// A WOKEN SEAT READS A COMMENT WHOLE, newlines and all — never the card's
// excerpt of it. The card is a preview cut at MaxExcerpt for a list of
// notices; the wake is what a seat acts on.
func TestAPageCommentWakesItsWatchersWithTheWholeComment(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	long := strings.Repeat("Step one looks right to me.\n", 40) + "BUT step four deletes prod."
	if _, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		doclet.NewComment{Body: long}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	routed := r.route(r.lastRecord(), doclet.Leads{"ENG": "lead"})
	if len(routed) == 0 {
		t.Fatal("a comment on a watched page woke nobody")
	}
	if got := routed[0].Inbound.Body; got != long {
		t.Fatalf("the woken seat read %d bytes of a %d-byte comment", len(got), len(long))
	}
}

// A WAKE LINKS THE PERSON TO THE PAGE, not to its JSON. The `url` a page change
// carries is the dashboard's address of the page on the dashboard's base — the
// address the dashboard's router resolves and the backlinks read back — and a
// parser with no base composes none rather than a rooted path nobody can open.
func TestAPageWakeLinksToTheDashboardsAddressOfThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	record := r.lastRecord()

	routed := r.routeWith(record, doclet.ParserOptions{BaseURL: "https://crewlet.example.com/"}, nil)
	if len(routed) != 1 {
		t.Fatalf("woke %d seats, want carla alone: %+v", len(routed), routed)
	}
	url := routed[0].Inbound.Metadata["url"]
	if want := "https://crewlet.example.com/" + doclet.AddressPrefix + page.Page.ID; url != want {
		t.Errorf("the wake links to %q, want the dashboard's address %q", url, want)
	}
	if got := doclet.Links(url); !slices.Equal(got, []string{page.Page.ID}) {
		t.Errorf("the backlinks read %v out of the wake's own link, want [%s]", got, page.Page.ID)
	}

	for _, c := range r.routeWith(record, doclet.ParserOptions{}, nil) {
		if url, ok := c.Inbound.Metadata["url"]; ok {
			t.Errorf("a parser with no base linked the wake to %q, want no link", url)
		}
	}
}

// AN AGENT'S PAGE COMMENT THAT ASKS NOTHING WAKES NO AGENT (ADR-0113): its
// mention is a reference and its watchers are not woken by it, while the
// same comment saying it asks, a person's comment, and an older build's
// comment with no statement reach them.
func TestAnAgentsPageCommentThatAsksNothingWakesNoAgent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	roster := chart(t, "jane", "swe", "bob")
	for i, c := range []struct {
		actor doclet.Actor
		asks  *bool
		want  []string
	}{
		{agent("swe"), ptr(false), []string{"jane"}},
		{agent("swe"), ptr(true), []string{"bob", "jane"}},
		{author("jane"), nil, []string{"bob"}},
	} {
		if _, _, err := r.store.Comment(t.Context(), c.actor, page.Page.ID,
			doclet.NewComment{Body: "Nothing for @bob, it still reads right.",
				Mentions: []string{"bob"}, Asks: c.asks}); err != nil {
			t.Fatalf("comment as %s: %v", c.actor.Handle, err)
		}
		routed := r.routeVia(r.lastRecord(), nil, roster)
		if got := routedTo(routed); !slices.Equal(got, c.want) {
			t.Errorf("case %d: %s's comment reached %v, want %v", i, c.actor.Handle, got, c.want)
		}
		// THE AGENT IT NAMED IS TOLD AND HELD, a held wake on its record.
		if wantHeld := c.asks != nil && !*c.asks; wantHeld != slices.Equal(heldBy(routed), []string{"bob"}) {
			t.Errorf("case %d: %s's comment held %v, want bob held %v", i, c.actor.Handle,
				heldBy(routed), wantHeld)
		}
	}
	// AN OLDER BUILD'S AGENT COMMENT, which states nothing, routes as before.
	unstated := r.lastRecord()
	record, err := doclet.Decode(unstated)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	record.ActorKind, record.Notify.Asks = doclet.AuthorAgent, nil
	if got := routedTo(r.routeVia(reencode(t, record), nil, roster)); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("an unstated agent comment reached %v, want bob as before", got)
	}
}

// AN ANSWER ON A PAGE REACHES THE AGENT THAT ASKED (ADR-0113): a page has no
// answer record, so while an agent's ask is open any reply in its thread by
// somebody else wakes it, whatever the reply names and whether it asks; the
// answer of a seat it named closes the ask, and the replies after wake nobody.
func TestAPageReplyWakesTheAgentThatAskedInItsThread(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	roster := chart(t, "jane", "a", "b", "c")
	asked, _, err := r.store.Comment(t.Context(), agent("a"), page.Page.ID,
		doclet.NewComment{Body: "@b is section 3 still accurate?", Mentions: []string{"b"},
			Asks: ptr(true)})
	if err != nil {
		t.Fatalf("a's question: %v", err)
	}
	reply := func(actor doclet.Actor, body string, asks *bool, mentions ...string) []string {
		t.Helper()
		if _, _, err := r.store.Comment(t.Context(), actor, page.Page.ID,
			doclet.NewComment{Body: body, ReplyTo: asked.ID, Mentions: mentions, Asks: asks}); err != nil {
			t.Fatalf("%s's reply: %v", actor.Handle, err)
		}
		return routedTo(r.routeVia(r.lastRecord(), nil, roster))
	}
	if got := reply(agent("c"), "section 4 changed last week", ptr(false)); !slices.Contains(got, "a") {
		t.Errorf("c's reply in a's open thread reached %v, want a", got)
	}
	if got := reply(agent("b"), "@a yes, still accurate", ptr(false), "a"); !slices.Contains(got, "a") {
		t.Fatalf("b's answer reached %v, want a, who asked", got)
	}
	if got := reply(agent("c"), "and section 4 too", ptr(false)); slices.Contains(got, "a") {
		t.Errorf("c's reply after b answered reached %v, want not a", got)
	}
	if got := reply(agent("a"), "thanks both", ptr(false)); slices.Contains(got, "a") ||
		slices.Contains(got, "b") || slices.Contains(got, "c") {
		t.Errorf("a's own thanks reached %v, want no agent", got)
	}
}

// reencode is a record encoded again after a test changed it.
func reencode(t *testing.T, rec doclet.MutationRecord) []byte {
	t.Helper()
	out, err := doclet.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

// routedTo is the handles a routing woke, sorted.
func routedTo(routed []notify.Routed) []string {
	var out []string
	for _, c := range routed {
		if !c.Held {
			out = append(out, c.To.Handle)
		}
	}
	slices.Sort(out)
	return out
}

// heldBy is the handles a routing told and did not wake, sorted.
func heldBy(routed []notify.Routed) []string {
	var out []string
	for _, c := range routed {
		if c.Held {
			out = append(out, c.To.Handle)
		}
	}
	slices.Sort(out)
	return out
}

// AN EDIT KEEPS ITS COMMENT'S STATEMENT (ADR-0113): an agent's comment that
// asked nothing stays a reference when edited, and one that asked still asks.
func TestAPageCommentEditKeepsItsAsks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	roster := chart(t, "jane", "swe", "bob")
	for _, asks := range []bool{false, true} {
		posted, _, err := r.store.Comment(t.Context(), agent("swe"), page.Page.ID,
			doclet.NewComment{Body: "Nothing for @bob.", Mentions: []string{"bob"}, Asks: &asks})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		if _, _, err := r.store.EditComment(t.Context(), agent("swe"), page.Page.ID, posted.ID,
			"Nothing for @bob, still."); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if got := routedTo(r.routeVia(r.lastRecord(), nil, roster)); slices.Contains(got, "bob") != asks {
			t.Errorf("the edit of a comment with asks=%v reached %v", asks, got)
		}
	}
}

// pageThread is a page whose comments a test posts and routes, against a
// roster of jane, a person, and agents a, b and c.
type pageThread struct {
	t      *testing.T
	r      *roundTrip
	page   string
	roster *notify.Registry
}

func newPageThread(t *testing.T) *pageThread {
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	return &pageThread{t: t, r: r, page: page.Page.ID, roster: chart(t, "jane", "a", "b", "c")}
}

// say posts one comment and answers the agents it woke.
func (p *pageThread) say(who, replyTo string, asks bool, body string, mentions ...string) (string, []string) {
	p.t.Helper()
	posted, _, err := p.r.store.Comment(p.t.Context(), agent(who), p.page,
		doclet.NewComment{Body: body, ReplyTo: replyTo, Mentions: mentions, Asks: &asks})
	if err != nil {
		p.t.Fatalf("%s's %q: %v", who, body, err)
	}
	var agents []string
	for _, handle := range routedTo(p.r.routeVia(p.r.lastRecord(), nil, p.roster)) {
		if handle != "jane" {
			agents = append(agents, handle)
		}
	}
	return posted.ID, agents
}

// A PAGE ASK STAYS OPEN UNTIL SOMEBODY ELSE ANSWERS IT (ADR-0113): a
// clarifying question back and its answer do not close the first ask, so its
// answer still reaches the agent that asked, and the acknowledgements after
// the two answers wake nobody.
func TestTwoAskersOnAPageStopAfterOneRound(t *testing.T) {
	t.Parallel()
	p := newPageThread(t)
	root, woke := p.say("a", "", true, "@b is section 3 still accurate?", "b")
	if !slices.Equal(woke, []string{"b"}) {
		t.Fatalf("a's question woke %v, want b", woke)
	}
	for i, step := range []struct {
		who, body string
		asks      bool
		want      []string
	}{
		{"b", "@a which version do you mean?", true, []string{"a"}},
		{"a", "v2", false, []string{"b"}},
		{"b", "then yes, section 3 is accurate", false, []string{"a"}},
		{"a", "thanks", false, nil},
		{"b", "you're welcome", false, nil},
		{"a", "cheers", false, nil},
	} {
		var mentions []string
		if step.asks {
			mentions = []string{"a"}
		}
		if _, woke = p.say(step.who, root, step.asks, step.body, mentions...); !slices.Equal(woke, step.want) {
			t.Fatalf("step %d (%s: %q) woke %v, want %v", i, step.who, step.body, woke, step.want)
		}
	}
}

// A CO-ASKER'S NUDGE ON A PAGE WAKES NOBODY WHO AWAITS THE SAME SEAT, while
// the addressee's answer wakes both agents that asked.
func TestAPageCoAskersNudgeLeavesTheOtherAskerAsleep(t *testing.T) {
	t.Parallel()
	p := newPageThread(t)
	root, _ := p.say("a", "", true, "@b is section 3 still accurate?", "b")
	p.say("c", root, true, "@b and section 4?", "b")
	if _, woke := p.say("c", root, false, "any word?"); len(woke) != 0 {
		t.Fatalf("c's nudge woke %v, want nobody: c only awaits the seat a awaits", woke)
	}
	if _, woke := p.say("b", root, false, "yes, both still hold"); !slices.Equal(woke, []string{"a", "c"}) {
		t.Errorf("b's answer woke %v, want a and c, both still awaiting", woke)
	}
	if _, woke := p.say("a", root, false, "thanks"); len(woke) != 0 {
		t.Errorf("a's thanks woke %v, want nobody: b answered both asks", woke)
	}
}

// AN ANSWER POSTED AT THE TOP LEVEL REACHES THE AGENT THAT ASKED, when it names
// the asker whose open question asked its author; it wakes no other agent.
func TestATopLevelPageAnswerNamingTheAskerWakesIt(t *testing.T) {
	t.Parallel()
	p := newPageThread(t)
	p.say("a", "", true, "@b is section 3 still accurate?", "b")
	if _, woke := p.say("b", "", false, "@a section 3 is still accurate.", "a"); !slices.Equal(woke, []string{"a"}) {
		t.Fatalf("b's top-level answer woke %v, want a", woke)
	}
	if _, woke := p.say("c", "", false, "@a noted.", "a"); len(woke) != 0 {
		t.Errorf("c's top-level remark woke %v, want nobody: a asked b, not c", woke)
	}
	// THE TOP-LEVEL ANSWER CLOSED THE ASK: neither b naming a again nor a
	// reply in a's thread wakes a.
	if _, woke := p.say("b", "", false, "@a section 4 too.", "a"); len(woke) != 0 {
		t.Errorf("b's second top-level remark woke %v, want nobody: the ask was answered", woke)
	}
}

// A REMOVED ASKING COMMENT ASKS NOTHING: its row is gone, so its author awaits
// no reply in its thread.
func TestARemovedPageAskStopsWaking(t *testing.T) {
	t.Parallel()
	p := newPageThread(t)
	root, _ := p.say("b", "", false, "Section 3 was reviewed.")
	ask, _ := p.say("a", root, true, "@b which reviewer?", "b")
	if _, err := p.r.store.RemoveComment(t.Context(), agent("a"), p.page, ask,
		doclet.CommentAuthority{}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, woke := p.say("b", root, false, "the usual one"); len(woke) != 0 {
		t.Errorf("a reply after the ask was removed woke %v, want nobody", woke)
	}
}
