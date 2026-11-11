package notify_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
)

func evOf(t *testing.T, payload map[string]any) *events.Event {
	t.Helper()
	ev := events.New(types.ExternalNotification{NotificationSource: "chat"}, events.TraceContext{})
	for k, v := range payload {
		if ev.Payload == nil {
			ev.Payload = map[string]any{}
		}
		ev.Payload[k] = v
	}
	return ev
}

// An event that cannot name its conversation must never merge with another,
// and a SHARED fallback would merge every one of them into a single turn.
func TestTheFallbackIsUniquePerEvent(t *testing.T) {
	a, b := evOf(t, nil), evOf(t, nil)
	ka, kb := notify.KeyOf(a), notify.KeyOf(b)
	if ka == kb {
		t.Fatalf("two unrelated events share the key %q — they would coalesce into one turn", ka)
	}
	for _, k := range []string{ka, kb} {
		if !strings.HasPrefix(k, notify.EventPrefix) {
			t.Fatalf("fallback %q is not namespaced", k)
		}
	}
}

// A stamped key is what the producer meant; the broker must not second-guess.
func TestAStampedKeyWins(t *testing.T) {
	ev := evOf(t, nil)
	notify.Stamp(ev, "chat:C1:1718.003", "chat:C1:1718.003")
	if got := notify.KeyOf(ev); got != "chat:C1:1718.003" {
		t.Fatalf("KeyOf = %q, want the stamped key", got)
	}
}

// An absent field and an empty one would read the same to every reader, so an
// empty key is not stamped at all and the fallback stays in one place.
func TestAnEmptyKeyIsNotStamped(t *testing.T) {
	ev := evOf(t, nil)
	notify.Stamp(ev, "", "")
	if _, present := ev.Payload[notify.PartitionField]; present {
		t.Fatal("an empty key was stamped, so the fallback is now bypassed by an empty string")
	}
	if _, present := ev.Payload[notify.ConversationField]; present {
		t.Fatal("an empty identity was stamped, so the fallback is now bypassed by an empty string")
	}
	if !strings.HasPrefix(notify.KeyOf(ev), notify.EventPrefix) {
		t.Fatal("the fallback did not apply")
	}
	// BOTH READS FALL BACK TO THE SAME VALUE, which is what keeps
	// [notify.Derived] answering the answer route and the answerable-in-chat
	// column identically: an event that can be "derived" under one key and
	// not the other would be offered a reply on one route that the other
	// says nobody can give.
	if notify.ConversationIdentityOf(ev) != notify.KeyOf(ev) {
		t.Fatalf("an event naming neither key fell back to %q and %q",
			notify.KeyOf(ev), notify.ConversationIdentityOf(ev))
	}
}

func TestStampingAnEventWithNoPayloadWorks(t *testing.T) {
	ev := events.New(types.ExternalNotification{}, events.TraceContext{})
	ev.Payload = nil
	notify.Stamp(ev, "chat:C1", "chat:C1")
	if got := notify.KeyOf(ev); got != "chat:C1" {
		t.Fatalf("KeyOf = %q", got)
	}
}

// Two third-party apps mint the same local key routinely (a work item and an
// issue are both plausibly "42"), and an un-namespaced key merges their
// events.
func TestALocalKeyIsNamespacedByItsSource(t *testing.T) {
	if a, b := notify.Namespaced("jira", "42"), notify.Namespaced("gitlab", "42"); a == b {
		t.Fatalf("two sources produced the same key %q", a)
	}
	if got := notify.Namespaced("jira", "42"); got != "jira:42" {
		t.Fatalf("Namespaced = %q", got)
	}
	// Half a key is no key: namespacing an empty local half would produce
	// "jira:", which every event from that source would share.
	if got := notify.Namespaced("jira", ""); got != "" {
		t.Fatalf("Namespaced with no local key = %q, want empty", got)
	}
	if got := notify.Namespaced("", "42"); got != "" {
		t.Fatalf("Namespaced with no source = %q, want empty", got)
	}
}

// The question a parked executor asks before telling somebody to reply in
// a thread that may not exist — and before taking a delivery for the reply to
// its question. A schedule's conversation is nobody's to reply into: every
// fire of the schedule carries it.
//
// Mutation: answer true for a schedule's key, and its row goes red.
func TestDerivedTellsAReplyableConversationFromAnEventFallback(t *testing.T) {
	cases := map[string]bool{
		"chat:C1:1718.003": true,
		"jira:ENG-42":      true,
		"event:018f-abc":   false,
		"":                 false,
		notify.ScheduleConversation(types.ScheduleScopeUnit, "eng", "standup"): false,
	}
	for key, want := range cases {
		if got := notify.Derived(key); got != want {
			t.Fatalf("Derived(%q) = %v, want %v", key, got, want)
		}
	}
}

// A SCHEDULE'S CONVERSATION IS KNOWN BY ITS PREFIX, AND NOTHING ELSE CARRIES
// IT. Every fire of a schedule is stamped with one key, its scope, its scope's
// id and its name escaped part by part, so a separator inside one part merges
// no two schedules; and no key a source derives, nor the per-event fallback,
// reads as a schedule's — a source's key is its source and a colon, which a
// schedule's prefix is not.
//
// Mutation: join the parts unescaped, leave the name alone unescaped, escape it
// as a query does, spell the prefix with a colon, or let [notify.Scheduled]
// match a source's key, and a check goes red.
func TestAScheduleConversationIsKnownByItsPrefixAlone(t *testing.T) {
	standup := notify.ScheduleConversation(types.ScheduleScopeUnit, "eng", "standup")
	if want := "schedule/unit/eng/standup"; standup != want {
		t.Fatalf("a unit's standup keys %q, want %q", standup, want)
	}
	split := notify.ScheduleConversation(types.ScheduleScopeUnit, "q3/launch", "review")
	joined := notify.ScheduleConversation(types.ScheduleScopeUnit, "q3", "launch/review")
	if split == joined {
		t.Fatalf("two schedules share the conversation %q", split)
	}
	// THE NAME IS ESCAPED ON ITS OWN: a slash and a space in it alone are
	// spelled as a path segment spells them, so no name reads as another
	// schedule's scope and the key is as long as the name's bound measures.
	if got, want := notify.ScheduleConversation(types.ScheduleScopeUnit, "eng", "weekly report/Q3"),
		"schedule/unit/eng/weekly%20report%2FQ3"; got != want {
		t.Fatalf("a name holding a space and a slash keys %q, want %q", got, want)
	}
	for _, key := range []string{standup, split, joined,
		notify.ScheduleConversation(types.ScheduleScopeRole, "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c11", "a b")} {
		if !notify.Scheduled(key) {
			t.Errorf("Scheduled(%q) = false, want a schedule's", key)
		}
	}
	for _, key := range []string{
		notify.Namespaced("schedule", "unit/eng/standup"), "chat:C1", "jira:ENG-42",
		notify.Fallback("018f-abc"), "",
	} {
		if notify.Scheduled(key) {
			t.Errorf("Scheduled(%q) = true, want no schedule's", key)
		}
	}
}

// Every event in a partition shares a key by construction, so a later one
// naming a different key is a routing bug — and taking the first keeps the
// answer stable rather than depending on which event sorted last.
func TestAPartitionsKeyIsItsFirstStampedOne(t *testing.T) {
	first, second := evOf(t, nil), evOf(t, nil)
	notify.Stamp(first, "chat:C1", "chat:C1")
	notify.Stamp(second, "chat:C2", "chat:C2")
	if got := notify.KeyOfAll([]*events.Event{first, second}); got != "chat:C1" {
		t.Fatalf("KeyOfAll = %q, want the first", got)
	}
	// A partition that stamped nothing has no key — the caller decides what
	// that means, rather than being handed one event's fallback as if it
	// described the whole partition.
	if got := notify.KeyOfAll([]*events.Event{evOf(t, nil)}); got != "" {
		t.Fatalf("KeyOfAll with nothing stamped = %q, want empty", got)
	}
}

func TestNilEventsAreSurvivable(t *testing.T) {
	if got := notify.KeyOf(nil); got != "" {
		t.Fatalf("KeyOf(nil) = %q", got)
	}
	if got := notify.KeyOfAll([]*events.Event{nil, nil}); got != "" {
		t.Fatalf("KeyOfAll(nils) = %q", got)
	}
	if got := notify.ConversationIdentityOf(nil); got != "" {
		t.Fatalf("ConversationIdentityOf(nil) = %q", got)
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{nil, nil}); got != "" {
		t.Fatalf("ConversationIdentityOfAll(nils) = %q", got)
	}
	notify.Stamp(nil, "chat:C1", "chat:C1") // must not panic
}

// THE TWO FIELDS ARE INDEPENDENT ON THE WIRE, because the one case they exist
// for is the one where they differ: a direct message's thread reply partitions
// on the thread and belongs to the whole channel.
func TestBothKeysRoundTripIndependently(t *testing.T) {
	ev := evOf(t, nil)
	notify.Stamp(ev, "chat:D1:root-1", "chat:D1")
	if got := notify.KeyOf(ev); got != "chat:D1:root-1" {
		t.Errorf("the broker would partition this on %q", got)
	}
	if got := notify.ConversationIdentityOf(ev); got != "chat:D1" {
		t.Errorf("the turn's rows on the executor log would be keyed on %q", got)
	}
	if got := notify.KeyOfAll([]*events.Event{ev}); got != "chat:D1:root-1" {
		t.Errorf("KeyOfAll = %q", got)
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{ev}); got != "chat:D1" {
		t.Errorf("ConversationIdentityOfAll = %q", got)
	}
}

// THE PARTITION IS NEVER READ AS THE IDENTITY. The partition function reads
// its own field and nothing else, and the identity read reads ITS own field
// and nothing else: an event that states a partition and no identity names no
// conversation, and falls back per event rather than being filed under a batch
// key its next turn would never look up.
func TestThePartitionIsNeverReadAsTheIdentity(t *testing.T) {
	ev := evOf(t, map[string]any{notify.PartitionField: "chat:D1:root-1"})
	if got := notify.KeyOf(ev); got != "chat:D1:root-1" {
		t.Errorf("the partition function reads %q", got)
	}
	if got, want := notify.ConversationIdentityOf(ev), notify.Fallback(ev.ID.String()); got != want {
		t.Errorf("identity = %q, want the event's own fallback %q", got, want)
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{ev}); got != "" {
		t.Errorf("the partition resolved to %q, want no identity", got)
	}
}

// THE QUEUE MINTS THE SAME FALLBACK NAMESPACE THIS PACKAGE DOES, and nothing
// else pins the two spellings together: internal/queue holds its own hardcoded
// copy of "event:" for a key function that answered empty, and it cannot
// import this package (notify imports queue). Rename [notify.EventPrefix]
// alone and the broker goes on minting the old namespace — which [Derived]
// then reads as a real conversation, so a parked run is offered as answerable
// in a thread that does not exist.
func TestTheQueuesOwnFallbackSharesThisNamespace(t *testing.T) {
	ev := evOf(t, nil)
	parts := queue.PartitionByKey([]*events.Event{ev},
		func(*events.Event) string { return "" },
		func(e *events.Event) *events.Event { return e })
	if len(parts) != 1 {
		t.Fatalf("one event made %d partitions", len(parts))
	}
	if got := parts[0].Key; got != notify.Fallback(ev.ID.String()) {
		t.Fatalf("the queue minted %q and this package mints %q", got,
			notify.Fallback(ev.ID.String()))
	}
	if notify.Derived(parts[0].Key) {
		t.Fatal("the queue's fallback reads as a real conversation, so a parked run " +
			"would be offered as answerable in a thread nobody can reach")
	}
}

// threadNotice is a notification on the thread chat:C1:1718.003 written as n
// names its writer.
func threadNotice(t *testing.T, n types.ExternalNotification) *events.Event {
	t.Helper()
	n.NotificationSource = "chat"
	ev := events.New(n, events.TraceContext{})
	notify.Stamp(ev, "chat:C1:1718.003", "chat:C1")
	return ev
}

// ONE TURN WORKS FOR ONE WRITER (ADR-0109): the partition function splits a
// thread's stamped key by who wrote each message, so one person's burst is
// one turn and a second person's message in the same thread is a turn of its
// own, each held to its own writer. The stamped key stays the thread's: a
// merged event carries it, and a parked run's batch is matched against it.
//
// Mutation: drop the writer from KeyOf, and the second person's message joins
// the first person's turn.
func TestOneTurnWorksForOneWriter(t *testing.T) {
	jane := func() *events.Event {
		return threadNotice(t, types.ExternalNotification{Sender: "Jane",
			Metadata: map[string]string{notify.ActorField: "U0JANE"}})
	}
	sam := threadNotice(t, types.ExternalNotification{Sender: "Sam",
		Metadata: map[string]string{notify.ActorField: "U0SAM"}})
	drain := []*events.Event{jane(), jane(), sam, jane(), jane()}
	parts := queue.PartitionByKey(drain, notify.KeyOf, func(e *events.Event) *events.Event { return e })
	if len(parts) != 2 || len(parts[0].Items) != 4 || len(parts[1].Items) != 1 || parts[1].Items[0] != sam {
		t.Fatalf("Jane's four messages and Sam's one made %d partitions, want Jane's four as one turn "+
			"and Sam's as another", len(parts))
	}
	if parts[0].Key == parts[1].Key {
		t.Fatalf("both writers partition on %q", parts[0].Key)
	}
	for _, part := range parts {
		if got := notify.KeyOfAll(part.Items); got != "chat:C1:1718.003" {
			t.Errorf("the stamped key of %s's turn is %q, want the thread's", part.Key, got)
		}
	}
}

// EVERY WRITER IS NAMED BY THE IDENTITY ITS TURN IS WORKED FOR: a first-party
// notice's by the kind its record names (a seat by its handle, a credential
// acting as no seat by its login, the engine as itself), and a vendor's by the
// actor its parser stamped, else its sender. Each is a partition of its own,
// and no writer can spell another's key: a writer's parts are escaped, so it
// never holds the separator. An event that is no external notification, and
// an event with no stamped key, are not split at all.
func TestEveryWriterIsAPartitionOfItsOwn(t *testing.T) {
	writers := map[string]types.ExternalNotification{
		"a seat":                 {Writer: &types.NotificationWriter{Kind: iam.ActorAgent, Seat: "cto"}},
		"a person as their seat": {Writer: &types.NotificationWriter{Kind: iam.ActorHuman, Seat: "jane"}},
		"a credential": {Writer: &types.NotificationWriter{Kind: iam.ActorOperator, Login: "token:ci",
			Apps: []iam.Grant{iam.GrantWorklet}}},
		"the engine":       {Writer: &types.NotificationWriter{Kind: iam.ActorSystem}},
		"a vendor's actor": {Sender: "Jane", Metadata: map[string]string{notify.ActorField: "U0JANE"}},
		"a sender alone":   {Sender: "jane by token:ci"},
	}
	seen := map[string]string{}
	for name, n := range writers {
		key := notify.KeyOf(threadNotice(t, n))
		writer, split := strings.CutPrefix(key, "chat:C1:1718.003"+notify.WriterSeparator)
		if !split || writer == "" || strings.Contains(writer, " ") {
			t.Errorf("%s: KeyOf = %q, want the thread's key split by one escaped writer", name, key)
		}
		if other, taken := seen[key]; taken {
			t.Errorf("%s and %s share the partition %q", name, other, key)
		}
		seen[key] = name
	}

	ask := events.New(types.A2ARequest{Requester: "cto"}, events.TraceContext{})
	notify.Stamp(ask, "a2a:cto", "a2a:cto")
	if got := notify.KeyOf(ask); got != "a2a:cto" {
		t.Errorf("a colleague's ask partitions on %q, want its stamped key unsplit", got)
	}
	unstamped := events.New(types.ExternalNotification{NotificationSource: "chat", Sender: "Jane"},
		events.TraceContext{})
	if got, want := notify.KeyOf(unstamped), notify.Fallback(unstamped.ID.String()); got != want {
		t.Errorf("an event with no stamped key partitions on %q, want its own fallback %q unsplit", got, want)
	}
}

// ONE TURN WORKS FOR ONE WHOM (ADR-0109): an agent's writes carry whom its
// turn worked for, and the turn each wakes works for them, so one seat's
// writes for two requesters are two partitions and two turns: coalesced, the
// second requester's write would be worked inside the first's turn, held to
// the first's apps. The WHOLE carrier keys the partition (its kind, the seat
// or the login, a credential's apps), so every constituent of one turn carries
// the same; the same requester's writes are one turn, as one writer's are. A
// carrier on a writer that is no agent's is never read, so it splits nothing.
//
// Mutation: leave the carrier out of writerOf, and the agent's writes for ada
// and for omar coalesce into one turn worked for ada.
func TestEveryWhomAnAgentWroteForIsAPartitionOfItsOwn(t *testing.T) {
	t.Parallel()
	by := func(kind iam.ActorKind, w *types.WorkingFor) *events.Event {
		return threadNotice(t, types.ExternalNotification{Sender: "cto",
			Writer: &types.NotificationWriter{Kind: kind, Seat: "cto", WorkingFor: w}})
	}
	token := func(apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps}
	}
	whoms := map[string]*events.Event{
		"for nobody else":           by(iam.ActorAgent, nil),
		"for ada":                   by(iam.ActorAgent, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}),
		"for omar":                  by(iam.ActorAgent, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}),
		"for a token using Worklet": by(iam.ActorAgent, token(iam.GrantWorklet)),
		"for the token using Worklet and Doclet": by(iam.ActorAgent,
			token(iam.GrantWorklet, iam.GrantDoclet)),
		"for a stranger": by(iam.ActorAgent, &types.WorkingFor{Kind: types.WorkingForStranger}),
	}
	seen := map[string]string{}
	for name, ev := range whoms {
		key := notify.KeyOf(ev)
		writer, split := strings.CutPrefix(key, "chat:C1:1718.003"+notify.WriterSeparator)
		if !split || !strings.HasPrefix(writer, "seat:cto") || strings.Contains(writer, notify.WriterSeparator) {
			t.Errorf("%s: KeyOf = %q, want the thread's key split by the agent's seat", name, key)
		}
		if other, taken := seen[key]; taken {
			t.Errorf("the writes %s and %s share the partition %q", name, other, key)
		}
		seen[key] = name
	}

	drain := []*events.Event{whoms["for ada"],
		by(iam.ActorAgent, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}), whoms["for omar"]}
	parts := queue.PartitionByKey(drain, notify.KeyOf, func(e *events.Event) *events.Event { return e })
	if len(parts) != 2 || len(parts[0].Items) != 2 || len(parts[1].Items) != 1 {
		t.Fatalf("two writes for ada and one for omar made %d partitions, want ada's two as one "+
			"turn and omar's as another", len(parts))
	}

	person := by(iam.ActorHuman, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"})
	if got, want := notify.KeyOf(person), notify.KeyOf(by(iam.ActorHuman, nil)); got != want {
		t.Errorf("a person's writer handed a carrier partitions on %q, want %q: a person writes "+
			"for nobody but themselves", got, want)
	}
}

// WHOM A WRITE WAS MADE FOR HAS ONE KEY, THE PARTITION'S (WorkingForKey).
// The worked-through mark of a chat message's edit compares the marking turn's
// carrier with the one the edit's wake stated, and a turn's carrier does not
// always come through FirstPartyWriter: a pickup's is read off a history row,
// in the order its apps were granted. So the key holds a credential's apps to
// the order a writer carries them in, drops an app this build does not know as
// the writer does, and keys nobody, and only nobody, as "". And the partition
// keys on exactly this key.
//
// Mutation: key a credential's apps in the order they arrive, keep an app this
// build does not know, or key a carrier of an empty kind as "", and a row goes
// red.
func TestWorkingForKeyKeysACarrierAsThePartitionSplitsIt(t *testing.T) {
	t.Parallel()
	seat := func(handle string) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
	}
	token := func(apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps}
	}
	same := []struct {
		name string
		a, b *types.WorkingFor
	}{
		{"a seat, and its handle with spaces around it", seat("ada"), seat(" ada ")},
		{"a credential's apps in the order they were granted, and in the order a writer carries them",
			token(iam.GrantChatlet, iam.GrantWorklet), token(iam.GrantWorklet, iam.GrantChatlet)},
		{"a credential holding an app this build does not know, and holding none of it",
			token(iam.GrantWorklet, iam.Grant("telepathy")), token(iam.GrantWorklet)},
		{"a credential naming an app twice, and once",
			token(iam.GrantWorklet, iam.GrantWorklet), token(iam.GrantWorklet)},
		{"two carriers of one kind this build does not know, naming different seats",
			&types.WorkingFor{Kind: "robot", Seat: "ada"}, &types.WorkingFor{Kind: "robot", Seat: "omar"}},
	}
	differ := []struct {
		name string
		a, b *types.WorkingFor
	}{
		{"nobody, and a carrier of an empty kind", nil, &types.WorkingFor{}},
		{"nobody, and a stranger", nil, &types.WorkingFor{Kind: types.WorkingForStranger}},
		{"two seats", seat("ada"), seat("omar")},
		{"one login with two sets of apps", token(iam.GrantWorklet),
			token(iam.GrantWorklet, iam.GrantDoclet)},
		{"a seat, and a credential whose login is its handle", seat("ada"),
			&types.WorkingFor{Kind: types.WorkingForCredential, Login: "ada"}},
	}
	if got := notify.WorkingForKey(nil); got != "" {
		t.Errorf("nobody keys as %q, want \"\": a write made for nobody else keys the partition by "+
			"its writer alone", got)
	}
	var carriers []*types.WorkingFor
	for _, tc := range same {
		if a, b := notify.WorkingForKey(tc.a), notify.WorkingForKey(tc.b); a != b {
			t.Errorf("%s key as %q and %q, want one key: a mark made under one would never match "+
				"a wake stated under the other", tc.name, a, b)
		}
		carriers = append(carriers, tc.a, tc.b)
	}
	for _, tc := range differ {
		if a := notify.WorkingForKey(tc.a); a == notify.WorkingForKey(tc.b) {
			t.Errorf("%s both key as %q, want two keys: a mark made for one would drop a wake "+
				"made for the other", tc.name, a)
		}
		carriers = append(carriers, tc.a, tc.b)
	}

	for _, c := range carriers {
		ev := threadNotice(t, types.ExternalNotification{Sender: "cto",
			Writer: &types.NotificationWriter{Kind: iam.ActorAgent, Seat: "cto", WorkingFor: c}})
		want := "chat:C1:1718.003" + notify.WriterSeparator + "seat:cto" + notify.WorkingForKey(c)
		if got := notify.KeyOf(ev); got != want {
			t.Errorf("an agent's write for %+v partitions on %q, want %q: the partition and every "+
				"reader comparing carriers key them one way", c, got, want)
		}
	}
}
