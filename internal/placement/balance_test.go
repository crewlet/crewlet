package placement_test

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
)

// THE BALANCE IS MEASURED OVER THE OBJECT MAP'S GROUPS — groups that split, at
// the count objstore/placement.TargetPGBits picks — because that is the corpus
// every figure in [placement.Balance]'s doc was measured over. So these are
// object maps, balanced through their draws.

// generation is the lineage every test map carries.
var generation = uuid.MustParse("5f0c7a52-8e1d-4c3b-9a64-0d2e7f1b3c85")

// mapOf is a valid map of these members, sorted.
func mapOf(pgBits, replicas int, failureDomain string, members ...placement.Member) objplacement.Map {
	slices.SortFunc(members, func(a, b placement.Member) int { return strings.Compare(a.Node, b.Node) })
	return objplacement.Map{Generation: generation, Epoch: 1, Replicas: replicas, PGBits: pgBits,
		FailureDomain: failureDomain, Members: members}
}

// member is a node of this weight at its default share, in this domain.
func member(node string, weight int, domain string) placement.Member {
	return placement.Member{Node: node, Weight: weight, Share: placement.DefaultShare(weight), Domain: domain}
}

// fleet is n equal members, data-000 onwards.
func fleet(n, pgBits, replicas int) objplacement.Map {
	members := make([]placement.Member, n)
	for i := range members {
		members[i] = member(fmt.Sprintf("data-%03d", i), 1, "")
	}
	return mapOf(pgBits, replicas, "", members...)
}

// balance balances a map's draw and answers the map with the shares it found.
func balance(m objplacement.Map, opts placement.BalanceOptions) (objplacement.Map, placement.BalanceReport) {
	balanced, report := placement.Balance(m.Draw(), opts)
	m.Members = balanced.Members
	return m, report
}

// entitled is a map's entitlement.
func entitled(m objplacement.Map) placement.Entitlement { return placement.Entitle(m.Draw()) }

// meanOf is the mean of the named members' copies.
func meanOf(copies map[string]int, nodes ...string) float64 {
	var sum float64
	for _, n := range nodes {
		sum += float64(copies[n])
	}
	return sum / float64(len(nodes))
}

// A WEIGHT IS A FRACTION OF EVERY COPY, not only of the first: at three copies
// straw2 alone gives a weight-4 member under three times a weight-1 member's
// copies, and the balanced map gives it four, within five percent.
func TestABalancedWeightIsItsFractionOfEveryCopy(t *testing.T) {
	t.Parallel()
	m := fleet(10, objplacement.TargetPGBits(10, 3), 3)
	m.Members[0].Weight, m.Members[0].Share = 4, placement.DefaultShare(4)
	heavy := m.Members[0].Node
	var light []string
	for _, mem := range m.Members[1:] {
		light = append(light, mem.Node)
	}
	ratio := func(m objplacement.Map) float64 {
		copies := m.Layout().Copies()
		return float64(copies[heavy]) / meanOf(copies, light...)
	}
	if got := ratio(m); got > 3 {
		t.Errorf("unbalanced, the weight-4 member holds %.2f times a weight-1 "+
			"member's copies — the defect the balancer exists for has gone", got)
	}
	balanced, report := balance(m, placement.BalanceOptions{})
	if !report.Converged {
		t.Fatalf("the balance did not converge: %+v", report)
	}
	if got := ratio(balanced); math.Abs(got-4)/4 > 0.05 {
		t.Fatalf("balanced, the weight-4 member holds %.2f times a weight-1 member's "+
			"copies, want 4 within 5%% (%+v)", got, report)
	}
	// AND THE FLEET STAYS AT 1.0 PER UNIT OF WEIGHT, so a member joining
	// later at ShareFor starts at its weight rather than at wherever the
	// shares had drifted.
	if got := balanced.Draw().ShareFor(1); got < placement.ShareOne-1 || got > placement.ShareOne+1 {
		t.Fatalf("a balanced fleet runs at %d per unit of weight, want %d", got, placement.ShareOne)
	}
}

// FIFTY EQUAL MEMBERS END UP EVEN: the fullest within five percent of the mean
// at the group count this package picks for them.
func TestFiftyEqualMembersBalanceEvenly(t *testing.T) {
	t.Parallel()
	m := fleet(50, objplacement.TargetPGBits(50, 3), 3)
	balanced, report := balance(m, placement.BalanceOptions{})
	copies := balanced.Layout().Copies()
	most := 0
	for _, n := range copies {
		most = max(most, n)
	}
	mean := float64(m.Groups()*m.Size()) / 50
	if got := float64(most) / mean; got > 1.05 || !report.Converged {
		t.Fatalf("the fullest of fifty holds %.3f of the mean (%+v)", got, report)
	}
	if got := balanced.Layout().Deviation(); got != report.Deviation {
		t.Fatalf("the report says %.4f and the map it returned measures %.4f",
			report.Deviation, got)
	}
}

// A DOMAIN HOLDS AT MOST ONE COPY OF A GROUP, so its target is capped there
// and the excess goes to the others; within a domain the copies still follow
// the weights. Three zones of two, three and four equal nodes at three copies:
// every zone holds exactly one copy of every group, so a node in the zone of
// two holds half the groups and a node in the zone of four a quarter — not the
// ninth of all copies their weights alone would say.
func TestBalancingRespectsFailureDomains(t *testing.T) {
	t.Parallel()
	var members []placement.Member
	for zone, size := range map[string]int{"z2": 2, "z3": 3, "z4": 4} {
		for i := range size {
			members = append(members, member(fmt.Sprintf("%s-%d", zone, i), 1, zone))
		}
	}
	m := mapOf(10, 3, "zone", members...)
	balanced, report := balance(m, placement.BalanceOptions{})
	if !report.Converged {
		t.Fatalf("a fleet whose zones cap its targets did not converge: %+v", report)
	}
	l := balanced.Layout()
	for pg := range m.Groups() {
		zones := map[string]bool{}
		for _, node := range l.Up(pg) {
			mem, _ := m.Member(node)
			zones[mem.Domain] = true
		}
		if len(zones) != 3 {
			t.Fatalf("group %d is held by %v, in %d zones", pg, l.Up(pg), len(zones))
		}
	}
	groups := float64(m.Groups())
	for node, n := range l.Copies() {
		mem, _ := m.Member(node)
		want := groups / map[string]float64{"z2": 2, "z3": 3, "z4": 4}[mem.Domain]
		if math.Abs(float64(n)-want)/want > 0.05 {
			t.Errorf("%s holds %d copies, want %.0f within 5%%", node, n, want)
		}
	}
}

// NOBODY HOLDS TWO COPIES OF ONE GROUP, so a member whose weight asks for more
// than every group is capped at every group, and the rest share what is left:
// a weight-64 member among three of weight 1 at three copies holds all 256
// groups, and the others two thirds each.
func TestAMemberIsCappedAtOneCopyOfEveryGroup(t *testing.T) {
	t.Parallel()
	m := mapOf(objplacement.MinPGBits, 3, "", member("big", 64, ""), member("s1", 1, ""),
		member("s2", 1, ""), member("s3", 1, ""))
	balanced, report := balance(m, placement.BalanceOptions{})
	copies := balanced.Layout().Copies()
	if copies["big"] != m.Groups() || !report.Converged {
		t.Fatalf("the heavy member holds %d of %d groups (%+v)", copies["big"],
			m.Groups(), report)
	}
	want := float64(2*m.Groups()) / 3
	for _, node := range []string{"s1", "s2", "s3"} {
		if math.Abs(float64(copies[node])-want)/want > placement.DefaultTolerance {
			t.Errorf("%s holds %d, want %.1f", node, copies[node], want)
		}
	}
}

// BALANCING A BALANCED MAP CHANGES NOTHING — the maintainer runs the balance
// whenever placement changes, and a pass that reshuffled an already even map
// would move data for no reason. And a balance moves only shares: the epoch,
// the members and everything else are the caller's.
func TestBalancingABalancedMapChangesNothing(t *testing.T) {
	t.Parallel()
	m := fleet(20, objplacement.TargetPGBits(20, 3), 3)
	for i := range m.Members {
		m.Members[i].Weight = 1 + i%3
		m.Members[i].Share = placement.DefaultShare(m.Members[i].Weight)
	}
	once, first := balance(m, placement.BalanceOptions{})
	if !first.Converged {
		t.Fatalf("the first balance did not converge: %+v", first)
	}
	twice, second := balance(once, placement.BalanceOptions{})
	if second.Rounds != 1 || second.Deviation != first.Deviation {
		t.Errorf("rebalancing took %d rounds to %.4f, the first ended at %.4f",
			second.Rounds, second.Deviation, first.Deviation)
	}
	for i := range once.Members {
		a, b := float64(once.Members[i].Share), float64(twice.Members[i].Share)
		if math.Abs(a-b)/a > 0.01 {
			t.Errorf("%s moved from %v to %v", once.Members[i].Node, a, b)
		}
	}
	strip := func(m objplacement.Map) objplacement.Map {
		m.Members = slices.Clone(m.Members)
		for i := range m.Members {
			m.Members[i].Share = 0
		}
		return m
	}
	if a, b := strip(m), strip(once); a.Epoch != b.Epoch || a.Generation != b.Generation ||
		a.PGBits != b.PGBits || !slices.Equal(a.Members, b.Members) {
		t.Errorf("a balance changed more than the shares: %+v -> %+v", a, b)
	}
	if slices.Equal(m.Members, once.Members) {
		t.Error("the unbalanced fleet's shares did not move at all")
	}
}

// THE BALANCE ANSWERS ITS BEST ROUND, never merely its last, and a map with
// nothing to place is answered as it was.
func TestABalanceAnswersItsBestRound(t *testing.T) {
	t.Parallel()
	m := fleet(12, 9, 3)
	start := m.Layout().Deviation()
	one, report := balance(m, placement.BalanceOptions{MaxRounds: 1})
	if report.Rounds != 1 || report.Deviation != start || !slices.Equal(one.Members, m.Members) {
		t.Fatalf("one round measured %.4f (%+v) of a map at %.4f", report.Deviation,
			report, start)
	}
	_, report = balance(m, placement.BalanceOptions{MaxRounds: 4, Tolerance: 1e-9})
	if report.Rounds != 4 || report.Converged || report.Deviation >= start {
		t.Fatalf("four rounds toward an unreachable tolerance: %+v from %.4f", report, start)
	}
	empty := fleet(3, 8, 3)
	for i := range empty.Members {
		empty.Members[i].Out = true
	}
	if got, report := balance(empty, placement.BalanceOptions{}); !slices.Equal(got.Members, empty.Members) ||
		!report.Converged || report.Rounds != 0 {
		t.Fatalf("an all-out map balanced to %+v (%+v)", got, report)
	}
}

// Layout cost at the fleets the package doc quotes: the counts TargetPGBits
// picks for fifty and two hundred members at three copies.
func BenchmarkLayout(b *testing.B) {
	for _, n := range []int{50, 200} {
		m := fleet(n, objplacement.TargetPGBits(n, 3), 3)
		b.Run(fmt.Sprintf("members=%d/groups=%d", n, m.Groups()), func(b *testing.B) {
			for b.Loop() {
				m.Layout()
			}
		})
	}
}

// Balance cost at the same fleets, equal and of mixed weights. The mixed ones
// are the worst case the default bound is sized for: at two hundred members
// over 8192 groups a weight-1 member's target is 49 copies, so its two percent
// holds less than a copy, the balance may not reach it, and then it runs every
// round DefaultMaxRounds allows.
func BenchmarkBalance(b *testing.B) {
	for _, n := range []int{50, 200} {
		for _, mixed := range []bool{false, true} {
			m := fleet(n, objplacement.TargetPGBits(n, 3), 3)
			if mixed {
				for i := range m.Members {
					m.Members[i].Weight = 1 + i%4
					m.Members[i].Share = placement.DefaultShare(m.Members[i].Weight)
				}
			}
			b.Run(fmt.Sprintf("members=%d/groups=%d/mixed=%v", n, m.Groups(), mixed), func(b *testing.B) {
				var report placement.BalanceReport
				for b.Loop() {
					_, report = balance(m, placement.BalanceOptions{})
				}
				b.ReportMetric(float64(report.Rounds), "rounds")
				b.ReportMetric(100*report.Deviation, "deviation%")
			})
		}
	}
}

// A SMALL CHANGE MOVES LITTLE, BALANCED OR NOT: a balance starts from the
// shares the fleet already has, so an eleventh member joining a balanced
// fleet of ten mixed weights, then balanced in, moves barely more than the
// copies it is entitled to — where a balance started from the weights would
// re-place the whole fleet's tuning.
func TestBalancingAJoinMovesLittleMoreThanTheJoin(t *testing.T) {
	t.Parallel()
	mixed := func(n int) objplacement.Map {
		m := fleet(n, 10, 3)
		for i := range m.Members {
			m.Members[i].Weight = 1 + i%3
			m.Members[i].Share = placement.DefaultShare(m.Members[i].Weight)
		}
		return m
	}
	before, _ := balance(mixed(10), placement.BalanceOptions{})
	joined := mixed(11)
	for i := range before.Members {
		joined.Members[i].Share = before.Members[i].Share
	}
	joined.Members[10].Share = before.Draw().ShareFor(joined.Members[10].Weight)
	after, report := balance(joined, placement.BalanceOptions{})
	if !report.Converged {
		t.Fatalf("the joined fleet did not balance: %+v", report)
	}
	lb, la := before.Layout(), after.Layout()
	moved := 0
	for pg := range after.Groups() {
		for _, node := range la.Up(pg) {
			if !slices.Contains(lb.Up(pg), node) {
				moved++
			}
		}
	}
	copies := after.Groups() * after.Size()
	entitled := float64(copies) * float64(joined.Members[10].Weight) / 21
	t.Logf("%d of %d copies moved; the newcomer is entitled to %.0f", moved, copies, entitled)
	if float64(moved) > 1.25*entitled {
		t.Fatalf("joining and balancing moved %d copies, over 1.25 times the %.0f "+
			"the newcomer is entitled to", moved, entitled)
	}
}

// window is the tolerance of a map's smallest target, in copies: how many
// copies either side of it the tolerance admits.
func window(m objplacement.Map, tolerance float64) float64 {
	e := entitled(m)
	least := math.Inf(1)
	for _, t := range e.Member {
		if t > 0 {
			least = min(least, t)
		}
	}
	return tolerance * least
}

// promisedRounds is what this package's corpus holds a promised fleet's
// balance to. The corpus's slowest takes 15 rounds, so twenty leaves room for
// a change of tuning and shows a correction that has stopped converging well
// before DefaultMaxRounds would hide it.
const promisedRounds = 20

// balanceCase is one fleet of the corpus below.
type balanceCase struct {
	name string
	m    objplacement.Map
}

// balanceCorpus is a spread of fleets for the balance's promise: sizes from
// three to thirty-four, one to ten copies, equal, mixed and lopsided weights,
// no failure domain, domains honoured (one of them crowded up to its ceiling
// of a copy of every group) and domains too few for the copies (a light one
// held up at its floor of a copy of every group), a member with no domain
// among members with one, group counts at and one bit over TargetPGBits, and
// shares at the defaults or up to thirty percent off.
//
// ENUMERATED rather than drawn from a random source, so it is the same fleets
// on every Go release: each parameter cycles at a different period through
// the index, which spreads the combinations without the cost of all of them.
func balanceCorpus() []balanceCase {
	sizes := []int{3, 5, 8, 13, 21, 34}
	replicas := []int{3, 1, 2, 3, 5, 10, 4}
	weights := []struct {
		name string
		of   func(i int) int
	}{
		{"equal", func(int) int { return 1 }},
		{"1-3", func(i int) int { return 1 + i%3 }},
		{"1-8", func(i int) int { return 1 << (i % 4) }},
		{"one-heavy", func(i int) int { return 1 + 15*btoi(i == 0) }},
		{"1-64", func(i int) int { return 1 + (i*37)%64 }},
	}
	domains := []string{"none", "zones", "few-zones"}
	var out []balanceCase
	for k := range 120 {
		n, r := sizes[k%len(sizes)], replicas[k%len(replicas)]
		size := min(r, n)
		w := weights[k%len(weights)]
		domain := domains[(k/2)%len(domains)]
		pg := objplacement.TargetPGBits(n, size) + k%2
		if domain == "few-zones" {
			// A domain's extra copies are few, so its members'
			// targets are small: a bit more keeps some of these
			// fleets inside the promise.
			pg++
		}
		members := make([]placement.Member, n)
		for i := range members {
			zone := ""
			switch domain {
			case "zones":
				// One more zone than copies, so the domain is
				// honoured: a member in each of the first, and
				// everyone else crowded into the last, which a
				// fleet of any size pushes to its ceiling.
				zone = fmt.Sprintf("z%d", min(i, size))
			case "few-zones":
				// One zone fewer than copies, and the first holding
				// only the first two members — at the lopsided
				// weights, lighter than its floor.
				zone = fmt.Sprintf("z%d", min(i/2, max(size-2, 1)))
			}
			if domain != "none" && k%5 == 0 && i == n-1 {
				zone = "" // a domain of its own
			}
			members[i] = member(fmt.Sprintf("k%02d-%02d", k, i), w.of(i), zone)
			if k%3 == 2 {
				// Thirty percent either way, spread by the golden
				// ratio so no two neighbours start alike.
				off := 0.7 + 0.6*math.Mod(float64(i)*0.6180339887, 1)
				members[i].Share = placement.Quantise(float64(members[i].Share) * off)
			}
		}
		fd := ""
		if domain != "none" {
			fd = "zone"
		}
		m := mapOf(pg, r, fd, members...)
		out = append(out, balanceCase{
			name: fmt.Sprintf("%02d/members=%d/copies=%d/weights=%s/domains=%s/groups=%d/start=%s",
				k, n, r, w.name, domain, m.Groups(), map[bool]string{true: "off", false: "default"}[k%3 == 2]),
			m: m,
		})
	}
	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// THE TOLERANCE IS REACHED WHEREVER A COPY AND A HALF FITS IN IT: a copy
// count is an integer, so a balance can promise two percent only of a target
// large enough — and from seventy-five copies it keeps the promise, for every
// fleet of the corpus whatever its weights, copies, domains or starting
// shares, within promisedRounds. Between one copy and a copy and a half it
// reaches the tolerance nearly always (Balance's doc has the figure), and a
// sample this size is held to nine in ten of them. Finer than a copy the
// tolerance may not be reachable at all, and the balance answers its closest
// round — never one worse than it started at, and never before its last round
// unless it converged. Whatever it reaches, the report is the returned map's
// own measure.
//
// The corpus has to include the fleets the promise is hard for — a tolerance
// under a copy and a half, domains too few for the copies, a domain of several
// members held at its ceiling or its floor — or it proves nothing, so it is
// required to.
func TestBalanceReachesTheToleranceWhereverACopyAndAHalfFitsInIt(t *testing.T) {
	t.Parallel()
	corpus := balanceCorpus()
	var promised, near, limited, finer, ceilings, floors int
	for _, c := range corpus {
		w := window(c.m, placement.DefaultTolerance)
		switch {
		case w < 1:
			finer++
		case w < placement.PromisedWindow:
			near++
		default:
			promised++
			if c.m.DomainLimited() {
				limited++
			}
			e := entitled(c.m)
			for u, n := range e.Count {
				switch {
				case n > 1 && e.Domain[u] == e.Hi[u] && e.Lo[u] == 0:
					ceilings++
				case n > 1 && e.Domain[u] == e.Lo[u] && e.Lo[u] > 0:
					floors++
				}
			}
		}
	}
	t.Logf("%d fleets: %d promised the tolerance (%d with too few domains, %d domains "+
		"of several members at a ceiling and %d at a floor), %d within a copy and a "+
		"half, %d finer than a copy", len(corpus), promised, limited, ceilings, floors,
		near, finer)
	if promised < 50 || limited < 5 || ceilings < 10 || floors < 5 || near < 10 || finer < 10 {
		t.Fatalf("the corpus no longer covers the hard cases: %d promised, %d limited, "+
			"%d ceilings, %d floors, %d near, %d finer", promised, limited, ceilings,
			floors, near, finer)
	}

	var mu sync.Mutex
	nearReached := 0
	t.Run("fleets", func(t *testing.T) {
		for _, c := range corpus {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				start := c.m.Layout().Deviation()
				balanced, report := balance(c.m, placement.BalanceOptions{})
				w := window(c.m, placement.DefaultTolerance)
				if got := balanced.Layout().Deviation(); got != report.Deviation ||
					report.Converged != (got <= placement.DefaultTolerance) {
					t.Fatalf("the report says %+v and the map it returned measures %.4f",
						report, got)
				}
				if report.Deviation > start {
					t.Fatalf("the balance answered %.4f for a map that started at %.4f",
						report.Deviation, start)
				}
				if !report.Converged && report.Rounds < placement.DefaultMaxRounds {
					t.Fatalf("the balance stopped unconverged with rounds to spare: %+v", report)
				}
				switch {
				case w >= placement.PromisedWindow && !report.Converged:
					t.Fatalf("a tolerance of %.2f copies at its smallest target was not "+
						"reached: %+v", w, report)
				case w >= placement.PromisedWindow && report.Rounds > promisedRounds:
					t.Fatalf("a tolerance of %.2f copies took %d rounds, want at most %d",
						w, report.Rounds, promisedRounds)
				case w >= 1 && w < placement.PromisedWindow && report.Converged:
					mu.Lock()
					nearReached++
					mu.Unlock()
				}
			})
		}
	})
	if nearReached*10 < near*9 {
		t.Fatalf("%d of %d fleets between one copy and a copy and a half of tolerance "+
			"reached it, want nine in ten", nearReached, near)
	}
}

// THE FLEETS THIS BALANCE WAS REWRITTEN FOR. Nine members of weight 1 and one
// of weight 4 at three copies ended at 5.3% (the heavy member, holding most
// groups, answered each correction too little, and one fleet-wide step shrank
// to nothing before it arrived); a weight-4 member joining ten balanced
// members ended at 3.6% from the shares the fleet had and 2.1% from the
// defaults. At the group count TargetPGBits picks for them and one bit over,
// every one is within the promise — its smallest target over a hundred copies
// — so every one is held to it, across the names that decide their draws.
func TestTheFleetsTheBalanceMissedNowBalance(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, what string, m objplacement.Map) {
		t.Helper()
		if w := window(m, placement.DefaultTolerance); w < placement.PromisedWindow {
			t.Fatalf("%s: a tolerance of %.2f copies is outside the promise", what, w)
		}
		if _, report := balance(m, placement.BalanceOptions{}); !report.Converged {
			t.Errorf("%s: %+v", what, report)
		}
	}
	for _, prefix := range []string{"data-", "node-", "n", "d", "x", "host-"} {
		for _, pg := range []int{objplacement.TargetPGBits(11, 3), objplacement.TargetPGBits(11, 3) + 1} {
			heavy := make([]placement.Member, 10)
			for i := range heavy {
				heavy[i] = member(fmt.Sprintf("%s%d", prefix, i), 1+3*btoi(i == 0), "")
			}
			m := mapOf(pg, 3, "", heavy...)
			check(t, fmt.Sprintf("nine light members and a heavy one (%s, %d groups)",
				prefix, m.Groups()), m)

			ten := make([]placement.Member, 10)
			for i := range ten {
				ten[i] = member(fmt.Sprintf("%s%d", prefix, i), 1, "")
			}
			before, report := balance(mapOf(pg, 3, "", ten...), placement.BalanceOptions{})
			if !report.Converged {
				t.Fatalf("ten equal members (%s, %d groups): %+v", prefix, m.Groups(), report)
			}
			check(t, fmt.Sprintf("a weight-4 member joining from the fleet's shares (%s, %d groups)",
				prefix, m.Groups()), mapOf(pg, 3, "", append(slices.Clone(before.Members),
				placement.Member{Node: prefix + "new", Weight: 4, Share: before.Draw().ShareFor(4)})...))
			check(t, fmt.Sprintf("a weight-4 member joining from the defaults (%s, %d groups)",
				prefix, m.Groups()), mapOf(pg, 3, "", append(ten, member(prefix+"new", 4, ""))...))
		}
	}
}

// A BALANCE NEVER STOPS WITH ROUNDS TO SPARE UNLESS IT CONVERGED. It stops
// early when no share moved, and every member it leaves alone is within the
// tolerance — which held only while a member of a domain that had settled was
// aimed at its own target rather than at its part of its domain's copies, a
// copy apart. This fleet, sixteen members of weights from 2 to 60 over seven
// zones at one copy, stopped at the seventeenth round at 2.02% with nothing
// moving; it converges now.
func TestABalanceStopsEarlyOnlyHavingConverged(t *testing.T) {
	t.Parallel()
	var members []placement.Member
	for _, m := range []struct {
		node   string
		weight int
		zone   string
	}{
		{"s2-407-m0", 5, "z6"}, {"s2-407-m1", 60, "z6"}, {"s2-407-m10", 24, ""},
		{"s2-407-m11", 5, "z4"}, {"s2-407-m12", 13, "z5"}, {"s2-407-m13", 15, ""},
		{"s2-407-m14", 7, "z2"}, {"s2-407-m15", 10, "z2"}, {"s2-407-m2", 36, "z6"},
		{"s2-407-m3", 30, "z0"}, {"s2-407-m4", 20, "z1"}, {"s2-407-m5", 6, "z4"},
		{"s2-407-m6", 30, "z0"}, {"s2-407-m7", 15, "z4"}, {"s2-407-m8", 2, "z1"},
		{"s2-407-m9", 59, "z6"},
	} {
		members = append(members, member(m.node, m.weight, m.zone))
	}
	m := mapOf(10, 1, "zone", members...)
	if _, report := balance(m, placement.BalanceOptions{}); !report.Converged &&
		report.Rounds < placement.DefaultMaxRounds {
		t.Fatalf("the balance stopped unconverged with rounds to spare: %+v", report)
	}
}

// A LONGER BALANCE NEVER ANSWERS WORSE THAN A SHORTER ONE: its rounds are the
// shorter one's and more, and it answers the best of them — so the deviation
// it reports never rises with MaxRounds, however the rounds themselves wander
// near the end.
func TestALongerBalanceNeverAnswersWorse(t *testing.T) {
	t.Parallel()
	tried := 0
	for _, c := range balanceCorpus() {
		// The small fleets: each length re-runs the balance from the
		// start.
		if c.m.Groups()*len(c.m.Members) > 1<<14 || tried == 12 {
			continue
		}
		tried++
		last := math.Inf(1)
		for rounds := 1; rounds <= 10; rounds++ {
			_, report := balance(c.m, placement.BalanceOptions{MaxRounds: rounds, Tolerance: 1e-9})
			if report.Deviation > last {
				t.Fatalf("%s: %d rounds answered %.4f, %d answered %.4f", c.name, rounds,
					report.Deviation, rounds-1, last)
			}
			last = report.Deviation
		}
	}
}

// A MEMBER ENTITLED TO EVERY GROUP GETS THERE IN A FEW ROUNDS. Near its
// ceiling a member's count answers its share hardly at all — it misses a group
// only when every other copy of it outranks it — and a correction by the plain
// ratio of target to copies crept up on the last few groups a fraction of a
// percent a round: a member of weight 26 among ten of weight 1 at two copies
// took 12 to 22 rounds, and one of weight 43 among 38 took 28 to 39, where
// the correction for the ceiling takes 3 to 10.
func TestAMemberAtItsCeilingIsReachedQuickly(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"data-", "node-", "n", "x"} {
		for _, c := range []struct{ light, weight, pgBits int }{
			{10, 26, 10}, {10, 26, 11}, {38, 43, 12},
		} {
			members := []placement.Member{member(prefix+"heavy", c.weight, "")}
			for i := range c.light {
				members = append(members, member(fmt.Sprintf("%s%02d", prefix, i), 1, ""))
			}
			m := mapOf(c.pgBits, 2, "", members...)
			heavy := slices.IndexFunc(m.Members, func(mem placement.Member) bool { return mem.Node == prefix+"heavy" })
			if want := entitled(m).Member[heavy]; want != float64(m.Groups()) {
				t.Fatalf("the heavy member is entitled to %.1f of %d groups", want, m.Groups())
			}
			_, report := balance(m, placement.BalanceOptions{})
			if !report.Converged || report.Rounds > 15 {
				t.Errorf("weight %d among %d of weight 1 over %d groups (%s): %+v",
					c.weight, c.light, m.Groups(), prefix, report)
			}
		}
	}
}

// WITH FEWER DOMAINS THAN COPIES A DOMAIN HOLDS AT LEAST ONE COPY OF EVERY
// GROUP, whatever its weight says, so that is what it is entitled to. A lone
// node in one zone and five in another at three copies: the lone node holds
// every group, and the five share the other two copies of each. And two light
// nodes in one zone beside two zones of heavy ones at four copies: the light
// zone holds a copy of every group between them, though their weight is a
// thirty-second of the fleet's, and the heavy zones share the rest in
// proportion. A target of the weight's part alone was one no share could
// reach, and the balance reported forty percent for a layout as even as the
// fleet allowed.
func TestTooFewDomainsEntitleADomainToACopyOfEveryGroup(t *testing.T) {
	t.Parallel()
	lone := []placement.Member{member("alone", 1, "zone-a")}
	for i := range 5 {
		lone = append(lone, member(fmt.Sprintf("crowd-%d", i), 1, "zone-b"))
	}
	light := []placement.Member{member("light-0", 1, "zone-a"), member("light-1", 1, "zone-a")}
	for i := range 3 {
		light = append(light, member(fmt.Sprintf("heavy-b%d", i), 10, "zone-b"),
			member(fmt.Sprintf("heavy-c%d", i), 10, "zone-c"))
	}
	for name, c := range map[string]struct {
		m    objplacement.Map
		want func(node string, groups float64) float64
	}{
		"a lone node": {mapOf(9, 3, "zone", lone...), func(node string, groups float64) float64 {
			if node == "alone" {
				return groups
			}
			return 2 * groups / 5
		}},
		"a light zone": {mapOf(9, 4, "zone", light...), func(node string, groups float64) float64 {
			if node == "light-0" || node == "light-1" {
				return groups / 2
			}
			return 1.5 * groups / 3
		}},
	} {
		if !c.m.DomainLimited() {
			t.Fatalf("%s: the fleet is not domain-limited", name)
		}
		groups := float64(c.m.Groups())
		e := entitled(c.m)
		for i, mem := range c.m.Members {
			if want := c.want(mem.Node, groups); math.Abs(e.Member[i]-want) > 1e-9 {
				t.Errorf("%s: %s is entitled to %.2f copies, want %.2f", name, mem.Node,
					e.Member[i], want)
			}
		}
		balanced, report := balance(c.m, placement.BalanceOptions{})
		if !report.Converged {
			t.Errorf("%s: the fleet did not balance to its entitlement: %+v", name, report)
		}
		copies := balanced.Layout().Copies()
		for _, mem := range c.m.Members {
			want := c.want(mem.Node, groups)
			if got := float64(copies[mem.Node]); math.Abs(got-want) > placement.DefaultTolerance*want {
				t.Errorf("%s: %s holds %.0f copies, want %.2f", name, mem.Node, got, want)
			}
		}
	}
}

// EVERY COPY IS SOMEBODY'S ENTITLEMENT, and nobody's is more than a layout can
// give it: across the corpus the targets sum to every copy the map places,
// no member's exceeds one copy of every group, and every domain's lies
// between its floor and its ceiling.
func TestEntitlementsAddUpToEveryCopyWithinTheirBounds(t *testing.T) {
	t.Parallel()
	for _, c := range balanceCorpus() {
		e := entitled(c.m)
		groups := float64(c.m.Groups())
		total := groups * float64(c.m.Size())
		var sum float64
		for i, target := range e.Member {
			sum += target
			if target > groups+1e-9 || target < 0 || (c.m.Members[i].Out && target != 0) {
				t.Errorf("%s: %s is entitled to %.3f of %v groups",
					c.name, c.m.Members[i].Node, target, groups)
			}
		}
		if math.Abs(sum-total) > 1e-6*total {
			t.Errorf("%s: the targets sum to %.4f of %.0f copies", c.name, sum, total)
		}
		for u, want := range e.Domain {
			if e.Count[u] > 0 && (want < e.Lo[u]-1e-9 || want > e.Hi[u]+1e-9) {
				t.Errorf("%s: domain %d is entitled to %.2f, outside [%.0f, %.0f]",
					c.name, u, want, e.Lo[u], e.Hi[u])
			}
		}
	}
}

// THE SAME MAP BALANCES TO THE SAME SHARES: the maintainer's answer is stored
// and compared, and a balance that answered differently on a second run would
// move an epoch for nothing.
func TestABalanceIsDeterministic(t *testing.T) {
	t.Parallel()
	for _, c := range balanceCorpus()[:12] {
		a, ra := balance(c.m, placement.BalanceOptions{})
		b, rb := balance(c.m, placement.BalanceOptions{})
		if !slices.Equal(a.Members, b.Members) || ra != rb {
			t.Fatalf("%s balanced two ways: %+v and %+v", c.name, ra, rb)
		}
	}
}
