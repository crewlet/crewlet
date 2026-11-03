package schedule

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// EVERY FIRE OF ONE SCHEDULE SHARES ONE INBOX PARTITION, AND SAYS WHEN IT WAS
// DUE.
//
// A seat that cannot work holds its inbox, and a schedule keeps firing into
// it. With a partition per fire, every fire that waited was its own turn when
// the seat came back: three health checks due five hours apart ran within
// four minutes of a limit clearing. Sharing one partition is what hands the
// waiting fires to one handler call, where the seat runs only the latest, and
// the instant each was due is what decides which one that is.
func TestEveryFireOfAScheduleSharesOnePartition(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(
		org.Schedule{Name: "health-check", Cron: "0 * * * *", Task: "check the services"},
		org.Schedule{Name: "standup", Cron: "0 * * * *", Task: "write the standup"},
	))
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))
	h.tick(tickAt(10, 0, 30))

	byName := map[string][]*events.Event{}
	for _, p := range h.q.inboxTasks() {
		fire, _ := events.DataAs[*types.TaskAssigned](p.ev)
		byName[fire.Schedule] = append(byName[fire.Schedule], p.ev)
	}
	checks := byName["health-check"]
	if len(checks) != 2 || len(byName["standup"]) != 2 {
		t.Fatalf("fired %d health checks and %d standups, want two of each",
			len(checks), len(byName["standup"]))
	}
	first, second := notify.KeyOf(checks[0]), notify.KeyOf(checks[1])
	if first != second {
		t.Fatalf("two fires of one schedule key partitions %q and %q; fires that "+
			"wait together would each be their own turn", first, second)
	}
	if notify.KeyOf(byName["standup"][0]) == first {
		t.Fatal("two schedules of one seat share a partition, so one would run in " +
			"place of the other")
	}
	for i, want := range []int{9, 10} {
		fire, _ := events.DataAs[*types.TaskAssigned](checks[i])
		if !fire.ScheduledAt.Equal(tickAt(want, 0, 0)) {
			t.Errorf("fire %d says it was due at %s, want %s", i, fire.ScheduledAt,
				tickAt(want, 0, 0))
		}
		// AND WHOSE SCHEDULE IT IS, whole: the scope a fire the seat does
		// not run is filed under, beside the name two units can share. A
		// role schedule's scope is its own seat, by the seat's agent id.
		if fire.ScopeType != types.ScheduleScopeRole || fire.ScopeID == "" ||
			fire.ScopeID != fire.Agent {
			t.Errorf("fire %d names its schedule's scope as %q/%q, want role/%s",
				i, fire.ScopeType, fire.ScopeID, fire.Agent)
		}
	}
}

// A SEPARATOR IN A NAME DOES NOT MERGE TWO SCHEDULES, for the reason the
// ledger's identity is a struct rather than a joined string.
func TestAFirePartitionCannotCollideAcrossScopes(t *testing.T) {
	t.Parallel()
	a := FirePartition(Entry{Scope: types.ScheduleScopeUnit, ScopeID: "Q3/Launch",
		Schedule: org.Schedule{Name: "review"}})
	b := FirePartition(Entry{Scope: types.ScheduleScopeUnit, ScopeID: "Q3",
		Schedule: org.Schedule{Name: "Launch/review"}})
	if a == b {
		t.Fatalf("two different schedules share the partition %q", a)
	}
}

// EACH FIRE OF A SCHEDULE IS ONE CONVERSATION OF ITS RUNNER SEAT.
//
// A fire is stamped with its schedule's conversation as BOTH keys: the
// partition, so the fires that wait in a seat's inbox together reach it
// together and run once, on that one key (the dispatcher's half,
// internal/engine's TestFiresOfOneScheduleThatWaitedTogetherRunOnce); and the
// identity, so the fire's turn reads the head the schedule's last fire left on
// its seat's executor log and continues that session. The log keys a session
// on its seat and its conversation, so a unit schedule fanned out to two seats
// is two conversations, one per seat: each seat's fire reaches its own inbox
// under the one key. The schedule's identity keys it and nothing else: an edit
// to its task, its cron and its timeout keeps the conversation, and a renamed
// schedule's next fire starts one of its own. And it is no conversation
// anybody can reply into, so no fire is taken for the reply to a question the
// last fire's run parked on.
//
// Mutation: stamp the partition alone, put the runner, the fire, the task or
// the cron in the key, or leave the name out of it, and a check goes red.
func TestEachFireOfAScheduleIsOneConversationOfItsRunner(t *testing.T) {
	t.Parallel()
	check := org.Schedule{Name: "health check", Cron: "0 * * * *", Task: "check the services"}
	h := build(t, roleOrg(check))
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))
	edited := check
	edited.Task, edited.Cron, edited.TimeoutSeconds = "check the services and the queues", "0 */2 * * *", 600
	h.reload(roleOrg(edited))
	h.tick(tickAt(10, 0, 30))

	want := notify.ScheduleConversation(types.ScheduleScopeRole, seatScopeID("qa"), "health check")
	fires := h.q.inboxTasks()
	if len(fires) != 2 {
		t.Fatalf("the schedule fired %d times, want twice: before and after its edit", len(fires))
	}
	for i, p := range fires {
		identity := notify.ConversationIdentityOfAll([]*events.Event{p.ev})
		if identity != want || notify.KeyOf(p.ev) != want || p.topic != seatInbox("qa") {
			t.Errorf("fire %d reached %s as conversation %q in partition %q, want both %q, on the "+
				"runner's inbox", i, p.topic, identity, notify.KeyOf(p.ev), want)
		}
	}
	if notify.Derived(want) {
		t.Errorf("the schedule's conversation %q reads as one somebody could reply into", want)
	}

	renamed := edited
	renamed.Name = "service check"
	h.reload(roleOrg(renamed))
	h.tick(tickAt(12, 0, 30))
	fires = h.q.inboxTasks()
	fresh := notify.ScheduleConversation(types.ScheduleScopeRole, seatScopeID("qa"), "service check")
	if len(fires) != 3 {
		t.Fatalf("the schedule fired %d times in all, want a third fire once renamed", len(fires))
	}
	if got := notify.ConversationIdentityOfAll([]*events.Event{fires[2].ev}); got != fresh || got == want {
		t.Fatalf("the renamed schedule's fire is on conversation %q, want one of its own %q", got, fresh)
	}

	unit := build(t, unitOrg(org.TargetEach))
	unit.seed(tickAt(8, 59, 30))
	unit.tick(tickAt(9, 0, 30))
	standup := notify.ScheduleConversation(types.ScheduleScopeUnit, "Quality", "standup")
	bySeat := map[string]string{}
	for _, p := range unit.q.inboxTasks() {
		bySeat[p.topic] = notify.ConversationIdentityOfAll([]*events.Event{p.ev})
	}
	if len(bySeat) != 2 || bySeat[seatInbox("qa-lead")] != standup || bySeat[seatInbox("qa-dev")] != standup {
		t.Fatalf("the unit's standup reached %v, want each member's own inbox under %q", bySeat, standup)
	}
}

// THE LONGEST SCHEDULE NAME A COMPANY ADMITS KEYS A CONVERSATION THE EXECUTOR
// LOG TAKES.
//
// Every fire is filed on its runner seat's executor log under its schedule's
// conversation, a key the log refuses past its bound rather than cut — so a
// schedule a company admits must key a conversation within it, or none of its
// fires would keep a session or leave an ending. The longest such key is a
// unit schedule's: the unit's key at the longest the company document's id
// grammar admits, and a name at org.ScheduleNameMaxBytes once escaped as the key
// spells it — a path segment — whether nothing in it escapes, every byte does,
// its letters lie outside ASCII, its spaces take three bytes each (a query
// would spell them in one), its colons one (a query would take three), or
// spaces pad it that the key keeps and a trim would drop; a role schedule's
// scope is its seat's agent id, shorter. A name one byte longer is refused, at
// its field, on either.
//
// Mutation: raise org.ScheduleNameMaxBytes past the room the log's bound
// leaves the name, measure the name unescaped, as url.QueryEscape spells it or
// trimmed, or widen the unit id grammar, and a check goes red.
func TestTheLongestScheduleNameFitsTheLogsKey(t *testing.T) {
	t.Parallel()
	document := func(unitID, name string) []byte {
		schedule := func(task string) string {
			return "      - {name: " + strconv.Quote(name) + ", cron: \"0 9 * * *\", task: " + task + "}\n"
		}
		return []byte("name: Acme\nroles:\n  - name: Ops\n    schedules:\n" + schedule("check the services") +
			"units:\n  - name: Eng\n    id: " + unitID + "\n    roles:\n      - name: Dev\n    schedules:\n" +
			schedule("post the standup"))
	}
	// THE LONGEST UNIT KEY a company document admits, and one byte more the
	// premise refuses: the key no escape touches spends its whole length.
	unitID := "u" + strings.Repeat("0", 63)
	if _, err := config.ParseCompany(document(unitID+"0", "standup")); err == nil {
		t.Fatal("the premise: a unit id one byte past sixty-four was admitted, so this case does not " +
			"build the longest key a company admits")
	}
	at := org.ScheduleNameMaxBytes
	for shape, name := range map[string]string{
		"nothing escaped":       strings.Repeat("s", at),
		"every byte escaped":    strings.Repeat("/", at/3) + strings.Repeat("s", at%3),
		"letters outside ASCII": strings.Repeat("é", at/6) + strings.Repeat("s", at%6),
		"spaces inside":         "a" + strings.Repeat(" ", (at-2)/3) + "b" + strings.Repeat("s", (at-2)%3),
		"kept in a path":        strings.Repeat(":", at),
		"padded":                strings.Repeat(" ", 40) + strings.Repeat("s", at-120),
	} {
		cfg, err := config.ParseCompany(document(unitID, name))
		if err != nil {
			t.Fatalf("%s: a name at the bound was refused: %v", shape, err)
		}
		o, err := cfg.Organization()
		if err != nil {
			t.Fatalf("%s: Organization: %v", shape, err)
		}
		entries := Entries(o)
		if len(entries) != 2 {
			t.Fatalf("%s: the company declares %d schedules, want the role's and the unit's", shape, len(entries))
		}
		for _, e := range entries {
			if key := FirePartition(e); len(key) > executordomain.ConversationKeyMaxBytes {
				t.Errorf("%s: the %s schedule's conversation is %d bytes, past the executor log's %d",
					shape, e.Scope, len(key), executordomain.ConversationKeyMaxBytes)
			}
		}
		_, err = config.ParseCompany(document(unitID, name+"s"))
		var refused []string
		for _, p := range config.Problems(err) {
			refused = append(refused, p.Path)
		}
		for _, want := range []string{"roles[0].schedules[0].name", "units[0].schedules[0].name"} {
			if !slices.Contains(refused, want) {
				t.Errorf("%s: a name a byte past the bound was refused at %v, want at %s", shape, refused, want)
			}
		}
	}
}
