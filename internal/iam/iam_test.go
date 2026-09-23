package iam

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// TestTheZeroValueOfEveryNamedTypeIsInvalid protects the property the whole
// package rests on: a gate somebody forgot to fill in holds a zero value, and
// a zero that validated would ship that gate as "granted".
//
// Both directions, because a Valid that answered false to everything would
// pass the first half and protect nothing.
func TestTheZeroValueOfEveryNamedTypeIsInvalid(t *testing.T) {
	cases := []struct {
		name  string
		zero  func() bool // Valid() on the type's zero value
		known func() bool // Valid() on a value this build declares
	}{
		{"Kind", func() bool { return Kind("").Valid() }, func() bool { return KindSeat.Valid() }},
		{"Grant", func() bool { return Grant("").Valid() }, func() bool { return GrantConfigWrite.Valid() }},
		{"Access", func() bool { return Access("").Valid() }, func() bool { return AccessWrite.Valid() }},
		{"Stage", func() bool { return Stage("").Valid() }, func() bool { return StageActive.Valid() }},
		{"ActorKind", func() bool { return ActorKind("").Valid() }, func() bool { return ActorOperator.Valid() }},
		{"Resolution", func() bool { return Resolution("").Valid() }, func() bool { return Unknown.Valid() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.zero() {
				t.Errorf("the zero %s validates — a gate nobody filled in would ship as granted", tc.name)
			}
			if !tc.known() {
				t.Errorf("a declared %s does NOT validate, so the check above proves nothing", tc.name)
			}
		})
	}

	// The two closed sets that also carry a behavioural predicate: a zero
	// must not be able to act, and must not classify as anything.
	if Stage("").MayAct() {
		t.Error("the zero Stage may act")
	}
	if !StageActive.MayAct() {
		t.Error("StageActive may NOT act, so the check above proves nothing")
	}
	if got := Grant("").Access(); got != "" {
		t.Errorf("the zero Grant classifies as %q, want the invalid zero Access", got)
	}
}

// TestEveryDeclaredSetIsTheSizeItClaims pins the counts the package doc and
// the constants' own comments state, so a member added or removed without a
// reader is caught rather than quietly changing what "the ten" means.
func TestEveryDeclaredSetIsTheSizeItClaims(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"AllGrants", len(AllGrants), 11},
		{"Kinds", len(Kinds), 4},
		{"ActorKinds", len(ActorKinds), 4},
		{"Stages", len(Stages), 5},
		{"Accesses", len(Accesses), 2},
		{"Resolutions", len(Resolutions), 3},
	} {
		if tc.got != tc.want {
			t.Errorf("%s has %d members, want %d", tc.name, tc.got, tc.want)
		}
	}
	// A duplicate would make a set look the right size while covering one
	// value twice and another not at all.
	seen := map[Grant]bool{}
	for _, g := range AllGrants {
		if seen[g] {
			t.Errorf("AllGrants lists %q twice", g)
		}
		seen[g] = true
	}
}

// TestTheLoginAndHandleRegexesAreDisjoint is the guard behind the package
// doc's claim that three name spaces can never collide.
//
// Over a GENERATED corpus rather than a handful of literals: a handful is a
// handful of cases somebody already thought of, and the property being
// protected is universal — no string at all may match both, and neither
// grammar may mint a name a seat handle could also be.
func TestTheLoginAndHandleRegexesAreDisjoint(t *testing.T) {
	corpus := nameCorpus()

	logins, handles := 0, 0
	for _, s := range corpus {
		isLogin, isHandle := ValidLogin(s), ValidMachineHandle(s)
		if isLogin && isHandle {
			t.Fatalf("%q is BOTH a person login and a machine handle — "+
				"one audit row, two actors, no way to tell which", s)
		}
		if isLogin {
			logins++
		}
		if isHandle {
			handles++
		}
		// The second half of the claim: a seat handle's grammar
		// ([a-z0-9][a-z0-9-]*, internal/org/role.go) carries neither
		// '.' nor ':', so every name either grammar admits must carry
		// one of them or it could be a seat's.
		if (isLogin || isHandle) && !strings.ContainsAny(s, ".:") {
			t.Fatalf("%q is admitted as a principal name and carries no '.' or ':' — "+
				"nothing stops it colliding with a seat handle", s)
		}
	}

	// THE CONTROL. A corpus that exercised neither grammar would pass
	// every assertion above while proving nothing at all.
	const floor = 50
	if logins < floor || handles < floor {
		t.Fatalf("corpus of %d strings produced %d logins and %d handles; "+
			"want at least %d of each or the disjointness above is vacuous",
			len(corpus), logins, handles, floor)
	}
}

// nameCorpus is every string of length 1..4 over the characters that decide a
// name's shape, plus deterministic longer ones over a wider alphabet so the
// rejection paths are exercised too.
func nameCorpus() []string {
	shape := []rune{'a', '9', '-', '.', ':', '_'}
	corpus := []string{""}
	level := []string{""}
	for range 4 {
		next := make([]string, 0, len(level)*len(shape))
		for _, prefix := range level {
			for _, r := range shape {
				next = append(next, prefix+string(r))
			}
		}
		corpus = append(corpus, next...)
		level = next
	}

	// Names BUILT the way a person or a provisioner would build them:
	// plausible segments joined by every separator worth trying, so both
	// grammars and their near-misses are exercised rather than waited for.
	segments := []string{"a", "9", "b9", "x-y", "-a", "a-", "x--y", "", "A", "x_y"}
	joins := []string{".", ":", "-", "_", "", "..", "::", ".:"}
	for _, join := range joins {
		for _, first := range segments {
			for _, second := range segments {
				corpus = append(corpus, first+join+second)
				for _, third := range segments {
					corpus = append(corpus, first+join+second+join+third)
				}
			}
		}
	}

	// A fixed seed: a corpus that differs between runs turns a real
	// counter-example into a flake nobody can reproduce.
	rng := rand.New(rand.NewPCG(0x1A, 0x105E))
	wide := []rune("aq9-.:_ZA/ @")
	for range 4000 {
		n := 5 + rng.IntN(10)
		var b strings.Builder
		for range n {
			b.WriteRune(wide[rng.IntN(len(wide))])
		}
		corpus = append(corpus, b.String())
	}
	return corpus
}
