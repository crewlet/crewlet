package placement

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
)

// THE SALT SEPARATES THE MAPS. The object map and the estate map place over
// the same data nodes, and a draw is a function of (seed, node key) — so
// without a salt of its own, a partition whose seed happened to equal an
// object group's would be held by exactly that group's holders, and the node
// whose loss costs one map the most would be the node whose loss costs the
// other the most. Under the two salts the rankings of one group over one node
// set are INDEPENDENT: over every fleet here the two maps agree on a group's
// primary, and on its whole up set, about as often as two independent draws
// from the same distribution would — the sum, over every primary or every up
// set, of the product of its frequency under each salt.
//
// It is a statistical bound, wide enough to hold for any honest pair of
// salts — five standard deviations of a binomial — and nowhere near what one
// salt shared by both maps gives, which is agreement on every group.
func TestEstateSaltSeparatesTheMaps(t *testing.T) {
	t.Parallel()
	if ObjectSalt == EstateSalt {
		t.Fatal("the two maps share a salt")
	}
	for _, c := range []struct {
		name string
		d    Draw
	}{
		{"three members, three copies", fleet(3, 3)},
		{"five members, two copies", fleet(5, 2)},
		{"twelve members, three copies", fleet(12, 3)},
		{"forty members, three copies", fleet(40, 3)},
		{"weights and zones", mixedDraw()},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			objects, estate := c.d, c.d
			objects.Salt, estate.Salt = ObjectSalt, EstateSalt
			for _, m := range c.d.Members {
				if nodeKey(ObjectSalt, m.Node) == nodeKey(EstateSalt, m.Node) {
					t.Fatalf("%s has one key under both salts", m.Node)
				}
			}
			lo, le := objects.Layout(), estate.Layout()
			groups := c.d.Groups.Count()
			for _, q := range []struct {
				what string
				of   func(up []string) string
			}{
				{"primaries", func(up []string) string { return up[0] }},
				{"up sets", func(up []string) string {
					return fmt.Sprint(slices.Sorted(slices.Values(up)))
				}},
			} {
				agree := 0
				seenO, seenE := map[string]int{}, map[string]int{}
				for pg := range groups {
					a, b := q.of(lo.Up(pg)), q.of(le.Up(pg))
					seenO[a]++
					seenE[b]++
					if a == b {
						agree++
					}
				}
				// The chance two independent draws from these two
				// distributions agree, read off the layouts themselves.
				var chance float64
				for k, n := range seenO {
					chance += float64(n) / float64(groups) * float64(seenE[k]) / float64(groups)
				}
				want := float64(groups) * chance
				sd := math.Sqrt(float64(groups) * chance * (1 - chance))
				t.Logf("%s: %d of %d agree, %.1f expected of independent draws", q.what,
					agree, groups, want)
				if math.Abs(float64(agree)-want) > 5*sd {
					t.Errorf("the two maps agree on %d of %d %s, want %.1f ± %.1f — the "+
						"salts do not separate them", agree, groups, q.what, want, 5*sd)
				}
			}
		})
	}
}

// STABILITY: a member joining moves only the groups it now holds, each by
// exactly that member, and every other group keeps its up set in its order; a
// member taken out moves only the groups it held, each by exactly one
// replacement. Whatever the groups and under either salt — the property that
// makes a change to either map copy the least it can.
func TestAChangeOfMembersMovesOnlyWhatItMust(t *testing.T) {
	t.Parallel()
	for _, salt := range []Salt{ObjectSalt, EstateSalt} {
		before, after := fleet(10, 3), fleet(11, 3)
		before.Salt, after.Salt = salt, salt
		newcomer := after.Members[10].Node
		lb, la := before.Layout(), after.Layout()
		entered := 0
		for pg := range before.Groups.Count() {
			was, is := lb.Up(pg), la.Up(pg)
			if !slices.Contains(is, newcomer) {
				if !slices.Equal(was, is) {
					t.Fatalf("%q: group %d moved %v -> %v without the new member taking it",
						salt, pg, was, is)
				}
				continue
			}
			entered++
			if gained := len(is) - overlap(was, is); gained != 1 {
				t.Fatalf("%q: group %d changed by %d members, %v -> %v", salt, pg, gained, was, is)
			}
		}
		if got := la.Copies()[newcomer]; got != entered || entered == 0 {
			t.Fatalf("%q: the newcomer entered %d groups and holds %d", salt, entered, got)
		}

		out := before
		out.Members = slices.Clone(before.Members)
		out.Members[4].Out = true
		leaver := out.Members[4].Node
		lo := out.Layout()
		for pg := range before.Groups.Count() {
			was, is := lb.Up(pg), lo.Up(pg)
			if !slices.Contains(was, leaver) {
				if !slices.Equal(was, is) {
					t.Fatalf("%q: group %d moved %v -> %v though the member taken out "+
						"did not hold it", salt, pg, was, is)
				}
				continue
			}
			if kept := overlap(was, is); kept != len(was)-1 || slices.Contains(is, leaver) {
				t.Fatalf("%q: group %d went %v -> %v taking %s out", salt, pg, was, is, leaver)
			}
		}
	}
}

// overlap is how many members two up sets share.
func overlap(a, b []string) int {
	n := 0
	for _, node := range a {
		if slices.Contains(b, node) {
			n++
		}
	}
	return n
}

// SPREAD, WHATEVER THE GROUPS: with as many failure domains as copies no group
// holds two copies in one domain; with fewer every group spans all there are
// and the draw says it is limited; a member without the label is a domain of
// its own — and at one copy each member's primaries are its share over the
// total, a chi-square over four members below its 0.1% critical value.
func TestCopiesSpreadOverAnyGroups(t *testing.T) {
	t.Parallel()
	var nine, six []Member
	for i := range 9 {
		nine = append(nine, member(fmt.Sprintf("n%d", i), 1+i%2, fmt.Sprintf("z%d", i%3)))
	}
	for i := range 6 {
		six = append(six, member(fmt.Sprintf("n%d", i), 1, fmt.Sprintf("z%d", i%2)))
	}
	mixed := []Member{
		member("a1", 1, "za"), member("a2", 1, "za"), member("a3", 1, "za"),
		member("u1", 1, ""), member("u2", 1, ""),
	}
	for _, salt := range []Salt{ObjectSalt, EstateSalt} {
		for _, c := range []struct {
			name    string
			d       Draw
			spans   int
			limited bool
		}{
			{"nine members in three zones", drawOf(3, "zone", nine...), 3, false},
			{"six members in two zones", drawOf(3, "zone", six...), 2, true},
			{"three in one zone and two unlabelled", drawOf(3, "zone", mixed...), 3, false},
		} {
			c.d.Salt = salt
			if err := c.d.Validate(); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got := c.d.DomainLimited(); got != c.limited {
				t.Errorf("%q, %s: DomainLimited = %v, want %v", salt, c.name, got, c.limited)
			}
			l := c.d.Layout()
			for pg := range c.d.Groups.Count() {
				zones := map[string]bool{}
				for _, node := range l.Up(pg) {
					mem, _ := c.d.Member(node)
					d := mem.Domain
					if d == "" {
						d = "node:" + node
					}
					zones[d] = true
				}
				if len(zones) != c.spans || len(l.Up(pg)) != 3 {
					t.Fatalf("%q, %s: group %d is held by %v, want 3 copies spanning %d domains",
						salt, c.name, pg, l.Up(pg), c.spans)
				}
			}
		}

		d := Draw{Salt: salt, Replicas: 1, Groups: partitionsOf(8192), Members: []Member{
			member("w1", 1, ""), member("w2", 2, ""), member("w3", 3, ""), member("w4", 4, "")}}
		d.Members[2].Share = DefaultShare(3) + 7777 // a share that is not a whole weight
		counts := d.Layout().Copies()
		var total float64
		for _, mem := range d.Members {
			total += float64(mem.Share)
		}
		chi := 0.0
		for _, mem := range d.Members {
			want := float64(d.Groups.Count()) * float64(mem.Share) / total
			got := float64(counts[mem.Node])
			chi += (got - want) * (got - want) / want
		}
		if chi > 16.27 { // three degrees of freedom at p = 0.001
			t.Errorf("%q: primaries %v against shares — chi-square %.2f", salt, counts, chi)
		}
	}
}

// A MEMBER THAT TAKES NO COPIES RANKS LAST, placeable or not by either flag,
// and a ranking is every member once, starting with the up set — under either
// salt, whatever the groups.
func TestMembersThatTakeNoCopiesRankLast(t *testing.T) {
	t.Parallel()
	for _, salt := range []Salt{ObjectSalt, EstateSalt} {
		d := fleet(6, 3)
		d.Salt = salt
		d.Members[1].Probation = true
		d.Members[4].Out = true
		idle := []string{d.Members[1].Node, d.Members[4].Node}
		if got := d.Size(); got != 3 || len(d.Placeable()) != 4 || d.DistinctDomains() != 4 {
			t.Fatalf("%q: size %d over %d placeable in %d domains, want 3 over 4 in 4", salt,
				got, len(d.Placeable()), d.DistinctDomains())
		}
		l := d.Layout()
		for _, node := range idle {
			if got := l.Copies()[node]; got != 0 {
				t.Fatalf("%q: %s holds %d groups", salt, node, got)
			}
		}
		for pg := range d.Groups.Count() {
			ranked := d.Ranked(pg)
			if !slices.Equal(ranked[:d.Size()], l.Up(pg)) || len(ranked) != len(d.Members) {
				t.Fatalf("%q: group %d ranks %v, up %v", salt, pg, ranked, l.Up(pg))
			}
			if tail := slices.Sorted(slices.Values(ranked[len(ranked)-2:])); !slices.Equal(tail, idle) {
				t.Fatalf("%q: group %d ranks %v, not ending with %v", salt, pg, ranked, idle)
			}
		}
		if got := (&Layout{}).Up(0); got != nil {
			t.Fatalf("the zero layout places group 0 on %v", got)
		}
	}
}

// A DRAW THAT CANNOT PLACE IS REFUSED, naming why — a salt no map declares, no
// groups, and everything a member list can get wrong.
func TestADrawThatCannotPlaceIsRefused(t *testing.T) {
	t.Parallel()
	valid := func(mutate func(*Draw)) Draw {
		d := drawOf(3, "zone", member("a", 1, "eu"), member("b", 1, ""))
		mutate(&d)
		return d
	}
	for _, salt := range []Salt{ObjectSalt, EstateSalt} {
		if err := valid(func(d *Draw) { d.Salt = salt }).Validate(); err != nil {
			t.Fatalf("the valid draw under %q is refused: %v", salt, err)
		}
	}
	for name, d := range map[string]Draw{
		"no salt":                     valid(func(d *Draw) { d.Salt = "" }),
		"a salt nobody declares":      valid(func(d *Draw) { d.Salt = "crewlet-other-node\x00" }),
		"no groups":                   valid(func(d *Draw) { d.Groups = nil }),
		"no group at all":             valid(func(d *Draw) { d.Groups = partitions{} }),
		"no replicas":                 valid(func(d *Draw) { d.Replicas = 0 }),
		"replicas past the cap":       valid(func(d *Draw) { d.Replicas = MaxReplicas + 1 }),
		"a domain label with a space": valid(func(d *Draw) { d.FailureDomain = "my zone" }),
		"an empty node":               valid(func(d *Draw) { d.Members[0].Node = " " }),
		"weight zero":                 valid(func(d *Draw) { d.Members[0].Weight = 0 }),
		"weight past the cap":         valid(func(d *Draw) { d.Members[0].Weight = MaxWeight + 1 }),
		"share zero":                  valid(func(d *Draw) { d.Members[0].Share = 0 }),
		"out of order":                valid(func(d *Draw) { d.Members[0], d.Members[1] = d.Members[1], d.Members[0] }),
		"a duplicate":                 valid(func(d *Draw) { d.Members[1].Node = "a" }),
		"a domain with no label":      valid(func(d *Draw) { d.FailureDomain = "" }),
	} {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

// A MEMBER JOINS AT THE FLEET'S RATE: its weight at the placeable members'
// mean share per unit of weight, or the default in a draw with none — and the
// share of a member that takes no copies is nobody's rate.
func TestANewMembersShareIsTheFleetsRate(t *testing.T) {
	t.Parallel()
	d := drawOf(3, "", member("a", 1, ""), member("b", 3, ""))
	d.Members[0].Share = 3 * shareOne // the fleet runs at 1.5 per weight
	d.Members[1].Share = 3 * shareOne
	if got, want := d.ShareFor(2), uint32(3*shareOne); got != want {
		t.Errorf("ShareFor(2) = %d, want %d", got, want)
	}
	for _, idle := range []func(*Member){
		func(m *Member) { m.Out = true },
		func(m *Member) { m.Probation = true },
	} {
		stale := member("c", 1, "")
		stale.Share = 40 * shareOne
		idle(&stale)
		with := d
		with.Members = append(slices.Clone(d.Members), stale)
		if got, want := with.ShareFor(2), uint32(3*shareOne); got != want {
			t.Errorf("ShareFor(2) beside %+v = %d, want %d", stale, got, want)
		}
	}
	none := d
	none.Members = slices.Clone(d.Members)
	none.Members[0].Out, none.Members[1].Probation = true, true
	if got := none.ShareFor(2); got != DefaultShare(2) {
		t.Errorf("ShareFor(2) with nothing placeable = %d, want the default", got)
	}
}
