package fleet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

var t0 = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

// plan is an available plan member at share points, last served minutesAgo.
func plan(key string, share, minutesAgo int) fleet.Member {
	return fleet.Member{Key: key, Plan: true, State: fleet.Available, Share: share, Known: true,
		LastServed: t0.Add(-time.Duration(minutesAgo) * time.Minute)}
}

// key is an available non-plan member: share 0, as the engine knows no cap.
func key(name string, minutesAgo int) fleet.Member {
	m := plan(name, 0, minutesAgo)
	m.Plan = false
	return m
}

func with(m fleet.Member, state fleet.State, until time.Time) fleet.Member {
	m.State, m.Until = state, until
	return m
}

func full(m fleet.Member) fleet.Member {
	m.Full = true
	return m
}

func unknown(m fleet.Member) fleet.Member {
	m.Share, m.Known = 0, false
	return m
}

// TestPickHandsNewWorkToTheRuleMember holds Pick to the strategies' rule: who
// takes new work, and why, for every case a seat job meets.
func TestPickHandsNewWorkToTheRuleMember(t *testing.T) {
	soon, later := t0.Add(20*time.Minute), t0.Add(3*time.Hour)
	cases := []struct {
		name     string
		strategy fleet.Strategy
		members  []fleet.Member
		home     string
		excluded []string
		want     string
		reason   fleet.Reason
	}{
		{
			name: "sequential fills the first member in list order", strategy: fleet.Sequential,
			members: []fleet.Member{plan("b", 70, 1), plan("a", 10, 90)},
			want:    "b", reason: fleet.ReasonNextInOrder,
		},
		{
			name: "sequential moves on once the first nears its limit", strategy: fleet.Sequential,
			members: []fleet.Member{with(plan("b", 85, 1), fleet.Near, time.Time{}), plan("a", 10, 90)},
			home:    "b", want: "a", reason: fleet.ReasonNextInOrder,
		},
		{
			name: "sequential skips a member at its limit", strategy: fleet.Sequential,
			members: []fleet.Member{with(plan("b", 100, 1), fleet.Limit, later), plan("a", 10, 90)},
			want:    "a", reason: fleet.ReasonNextInOrder,
		},
		{
			name: "balanced picks the lowest share", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 40, 1), plan("b", 12, 1), plan("c", 30, 1)},
			want:    "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "balanced ties go to the member that served least recently", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 20, 5), plan("b", 20, 50), plan("c", 20, 10)},
			want:    "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "balanced full ties go to the first listed", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 20, 5), plan("b", 20, 5)},
			want:    "a", reason: fleet.ReasonLowestShare,
		},
		{
			name: "the home keeps its work within the threshold", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 25, 1), plan("b", 20, 60)},
			home:    "a", want: "a", reason: fleet.ReasonWithinThreshold,
		},
		{
			name: "the home loses its work past the threshold", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 26, 1), plan("b", 20, 60)},
			home:    "a", want: "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "the home that is the least used is the lowest share", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 10, 1), plan("b", 20, 60)},
			home:    "a", want: "a", reason: fleet.ReasonLowestShare,
		},
		{
			name: "a near home takes no new work while another member is available", strategy: fleet.Balanced,
			members: []fleet.Member{with(plan("a", 82, 1), fleet.Near, time.Time{}), plan("b", 79, 60)},
			home:    "a", want: "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "plans first: keys wait while a plan is available", strategy: fleet.Balanced,
			members: []fleet.Member{key("api", 600), plan("a", 60, 1)},
			want:    "a", reason: fleet.ReasonLowestShare,
		},
		{
			name: "overflow: keys take work once every plan is near or at its limit", strategy: fleet.Balanced,
			members: []fleet.Member{with(plan("a", 85, 1), fleet.Near, time.Time{}),
				with(plan("b", 100, 1), fleet.Limit, later), key("api", 600)},
			want: "api", reason: fleet.ReasonOverflow,
		},
		{
			name: "overflow keys share by who served least recently", strategy: fleet.Balanced,
			members: []fleet.Member{key("api", 5), key("cloud", 90)},
			want:    "cloud", reason: fleet.ReasonOverflow,
		},
		{
			name: "a signed-out member leaves routing", strategy: fleet.Balanced,
			members: []fleet.Member{with(plan("a", 0, 900), fleet.SignedOut, time.Time{}), plan("b", 50, 1)},
			want:    "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "with none available, the near members take work", strategy: fleet.Balanced,
			members: []fleet.Member{with(plan("a", 90, 1), fleet.Near, time.Time{}),
				with(plan("b", 84, 1), fleet.Near, time.Time{})},
			want: "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "an unknown share ranks as the lowest known, so it serves once", strategy: fleet.Balanced,
			members: []fleet.Member{plan("a", 30, 1), unknown(plan("b", 0, 300)), plan("c", 45, 1)},
			want:    "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "a member with every slot taken is skipped for one with a free slot", strategy: fleet.Balanced,
			members: []fleet.Member{full(plan("a", 10, 1)), plan("b", 30, 1)},
			home:    "a", want: "b", reason: fleet.ReasonLowestShare,
		},
		{
			name: "every pool member full: the pick queues on the rule's member", strategy: fleet.Balanced,
			members: []fleet.Member{full(plan("a", 10, 1)), full(plan("b", 30, 1))},
			want:    "a", reason: fleet.ReasonLowestShare,
		},
		{
			name: "a re-ask after a failure hands over, the failed member excluded", strategy: fleet.Balanced,
			members:  []fleet.Member{plan("a", 10, 1), plan("b", 30, 1)},
			excluded: []string{"a"}, home: "a", want: "b", reason: fleet.ReasonLimitHandover,
		},
		{
			name: "excluding every plan leaves the overflow", strategy: fleet.Balanced,
			members:  []fleet.Member{plan("a", 10, 1), key("api", 1)},
			excluded: []string{"a"}, want: "api", reason: fleet.ReasonLimitHandover,
		},
		{
			name: "a sequential re-ask goes to the next in order", strategy: fleet.Sequential,
			members:  []fleet.Member{plan("a", 10, 1), with(plan("b", 0, 1), fleet.Limit, soon), plan("c", 0, 1)},
			excluded: []string{"a"}, want: "c", reason: fleet.ReasonLimitHandover,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fleet.Fleet{Key: "claude-opus", Strategy: tc.strategy, Threshold: 5, Members: tc.members}
			got, ok := f.Pick(tc.home, tc.excluded...)
			if !ok {
				t.Fatalf("Pick found nobody, want %s", tc.want)
			}
			if got.Key != tc.want || got.Reason != tc.reason {
				t.Errorf("Pick = %s (%s), want %s (%s)", got.Key, got.Reason, tc.want, tc.reason)
			}
		})
	}
}

// TestFleetWithNobodyToTakeWorkPicksNobody holds the waiting cases: an empty
// fleet, and one every member of which is at its limit or signed out, pick
// nobody, so the seat job waits rather than run anywhere.
func TestFleetWithNobodyToTakeWorkPicksNobody(t *testing.T) {
	cases := map[string][]fleet.Member{
		"an empty fleet": nil,
		"every member at its limit": {
			with(plan("a", 100, 1), fleet.Limit, t0.Add(time.Hour)),
			with(key("api", 1), fleet.Limit, t0.Add(time.Minute)),
		},
		"every member at its limit or signed out": {
			with(plan("a", 100, 1), fleet.Limit, t0.Add(time.Hour)),
			with(plan("b", 0, 1), fleet.SignedOut, time.Time{}),
		},
	}
	for name, members := range cases {
		for _, strategy := range fleet.Strategies {
			t.Run(name+"/"+string(strategy), func(t *testing.T) {
				f := fleet.Fleet{Key: "default-fleet", Strategy: strategy, Threshold: 5, Members: members}
				if got, ok := f.Pick(""); ok {
					t.Errorf("Pick = %s, want nobody", got.Key)
				}
			})
		}
	}
}

// TestWalkEndsOnTheEarliestRoom holds the walk a piece of work takes: every
// pick with the ones before it excluded, then the members at their limit,
// earliest room first, so a walk that meets every limit names the earliest
// instant any member has room, and a signed-out member only where nothing
// else is.
func TestWalkEndsOnTheEarliestRoom(t *testing.T) {
	soon, later := t0.Add(20*time.Minute), t0.Add(3*time.Hour)
	cases := []struct {
		name    string
		members []fleet.Member
		want    []string
		room    string
	}{
		{
			name: "the pool by the rule, then the limits by room",
			members: []fleet.Member{with(plan("a", 100, 1), fleet.Limit, later), plan("b", 40, 1),
				with(plan("c", 0, 1), fleet.SignedOut, time.Time{}), plan("d", 10, 1),
				with(plan("e", 100, 1), fleet.Limit, soon), with(plan("f", 100, 1), fleet.Limit, time.Time{})},
			want: []string{"d", "b", "e", "a", "f"}, room: "e",
		},
		{
			name: "every member at its limit: earliest room first",
			members: []fleet.Member{with(plan("a", 100, 1), fleet.Limit, later),
				with(key("api", 1), fleet.Limit, soon), with(plan("b", 0, 1), fleet.SignedOut, time.Time{})},
			want: []string{"api", "a"}, room: "api",
		},
		{
			name: "after the available members, the near ones",
			members: []fleet.Member{with(plan("a", 85, 1), fleet.Near, time.Time{}), plan("b", 40, 1),
				with(plan("c", 100, 1), fleet.Limit, soon)},
			want: []string{"b", "a", "c"}, room: "c",
		},
		{
			name: "every member signed out: the sign-in failure, as when pinned",
			members: []fleet.Member{with(plan("a", 0, 1), fleet.SignedOut, time.Time{}),
				with(plan("b", 0, 1), fleet.SignedOut, time.Time{})},
			want: []string{"a", "b"},
		},
		{name: "an empty fleet has no walk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fleet.Fleet{Key: "claude-opus", Strategy: fleet.Balanced, Threshold: 5, Members: tc.members}
			var got []string
			for _, c := range f.Walk("") {
				got = append(got, c.Key)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Walk = %v, want %v", got, tc.want)
			}
			_, room, ok := f.FirstRoom()
			if room != tc.room || ok != (tc.room != "") {
				t.Errorf("FirstRoom = %q (%v), want %q", room, ok, tc.room)
			}
		})
	}
}

// TestSpreadIsTheGapAmongMembersTakingWork holds the spread the Fleets page
// shows: Balanced only, among the known shares of the pool, so an overflow
// key or a member at its limit does not widen it.
func TestSpreadIsTheGapAmongMembersTakingWork(t *testing.T) {
	cases := []struct {
		name     string
		strategy fleet.Strategy
		members  []fleet.Member
		want     int
		ok       bool
	}{
		{"plans", fleet.Balanced, []fleet.Member{plan("a", 30, 1), plan("b", 22, 1), key("api", 1)}, 8, true},
		{"one plan in the pool", fleet.Balanced,
			[]fleet.Member{plan("a", 30, 1), with(plan("b", 100, 1), fleet.Limit, t0)}, 0, false},
		{"sequential is in order, not a spread", fleet.Sequential,
			[]fleet.Member{plan("a", 30, 1), plan("b", 22, 1)}, 0, false},
		{"an unknown share is not a gap", fleet.Balanced,
			[]fleet.Member{plan("a", 30, 1), unknown(plan("b", 0, 1))}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := fleet.Fleet{Strategy: tc.strategy, Members: tc.members}.Spread()
			if got != tc.want || ok != tc.ok {
				t.Errorf("Spread = %d (%v), want %d (%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestLedgerWalkRecordsTheMemberThatTakesTheWork holds the balancer's memory:
// a seat job's home is the last member that took its work, a member's last
// serve never moves back, and a walk from a home names the home and why it
// did not keep the work.
func TestLedgerWalkRecordsTheMemberThatTakesTheWork(t *testing.T) {
	now := t0
	l := fleet.NewLedger(func() time.Time { return now })
	job := fleet.Job{Fleet: "claude-opus", Subject: "engineer", Job: "execute"}
	f := fleet.Fleet{Key: "claude-opus", Strategy: fleet.Balanced, Threshold: 5,
		Members: []fleet.Member{plan("a", 10, 1), plan("b", 30, 1)}}
	if got := l.Home(job); got != "" {
		t.Fatalf("Home of a fresh ledger = %q, want none", got)
	}
	walk := l.Walk(f, job)
	if len(walk) != 2 || walk[0].Key != "a" || walk[0].Moved() {
		t.Fatalf("first walk = %v, want a then b with no home to move from", keysOf(walk))
	}
	walk[0].Take()
	now = t0.Add(time.Minute)
	walk[1].Take()
	if got := l.Home(job); got != "b" {
		t.Errorf("Home = %q, want b, the last member that took the work", got)
	}
	now = t0.Add(-time.Hour)
	l.Walk(f, fleet.Job{Fleet: "claude-opus", Subject: "writer", Job: "execute"})[0].Take()
	if got := l.LastServed("a"); !got.Equal(t0) {
		t.Errorf("LastServed(a) = %v, want %v: an older serve does not move it back", got, t0)
	}
	f.Members[1] = with(f.Members[1], fleet.Limit, t0.Add(time.Hour))
	f.Members[1].Why = "Reached its 5-hour limit"
	moved := l.Walk(f, job)[0]
	if !moved.Moved() || moved.Key != "a" || moved.Home != "b" || moved.HomeWhy != "Reached its 5-hour limit" {
		t.Errorf("walk from a home at its limit = %s from %s (%q), want a from b for its limit",
			moved.Key, moved.Home, moved.HomeWhy)
	}
}

func keysOf(walk []*fleet.Pick) []string {
	var out []string
	for _, p := range walk {
		out = append(out, p.Key)
	}
	return out
}
