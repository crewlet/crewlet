package engine_test

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PRESENCE LEASE AT ITS WORST FITS ITS BOUND, AND THE BOUND IS THAT WORST
// CASE ROUNDED UP.
//
// The store bounds presence (coord.MaxLeaseMetaBytes) and a refused one drops
// the node from its peers' count, so the bound is derived: seat.PresenceMeta
// with every input at its limit (every role, the longest broker kind,
// placement.MaxLabels labels at the key and value limits in the admitted
// characters the store's encoder spells longest, the held count, a Gitlet
// budget and every status count at MaxInt, the withdrawal, the longest
// postures, the widest timestamp, a build cut to coord.MaxBuildBytes, one
// alarm mark per kind), held to it rounded up to the next KiB both ways.
// Mutations red: the bound at 4 KiB, HTML escaping, build uncut, MaxLabels up.
func TestThePresenceLeaseAtItsWorstFitsItsBound(t *testing.T) {
	t.Parallel()
	keys := widestDistinct(t, "label key", placement.CheckLabelKey, placement.MaxLabelKeyBytes,
		placement.MaxLabels)
	values := widestDistinct(t, "label value", placement.CheckLabelValue,
		placement.MaxLabelValueBytes, 1)
	labels := make(map[string]string, placement.MaxLabels)
	for _, key := range keys {
		labels[key] = values[0]
	}

	profile := placement.NodeProfile{
		ID:     "node-a",
		Roles:  placement.Roles(placement.Vocabulary()...),
		Labels: labels,
		Broker: longest(t, placement.BrokerKinds()...),
	}
	widest := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	status := coord.NodeStatus{
		InFlight:            math.MaxInt64,
		Draining:            false, // "false" is the longer of the two
		Posture:             string(longest(t, configplane.Postures()...)),
		StartedAt:           widest,
		Build:               strings.Repeat(costliestByte(t), 16*coord.MaxBuildBytes),
		ParkedStateVersion:  math.MaxInt64,
		CaptureStampVersion: math.MaxInt64,
		AppliersReady:       math.MaxInt64,
		AppliersTotal:       math.MaxInt64,
		AdminPosture:        longest(t, iam.AdminPostures...),
		AlarmsEvaluated:     true,
	}
	kinds := statelog.Kinds()
	if len(kinds) == 0 {
		t.Fatal("the alarm table names no kind: the case would bound a status with no marks")
	}
	for _, kind := range kinds {
		status.Alarms = append(status.Alarms, coord.AlarmMark{Kind: string(kind), Since: widest})
	}
	budget := int64(math.MaxInt64)
	meta := seat.PresenceMeta(profile, math.MaxInt, true, &budget, &status)

	worst, err := coord.LeaseMetaBytes(meta)
	if err != nil {
		t.Fatalf("measure the worst presence: %v", err)
	}
	t.Logf("the presence at its worst: %d bytes (%d labels, %d alarm kinds); the bound %d",
		worst, len(labels), len(kinds), coord.MaxLeaseMetaBytes)
	if worst > coord.MaxLeaseMetaBytes {
		t.Fatalf("the presence at its worst is %d bytes, past the %d a lease carries: a node "+
			"legal under Tier A would be refused its presence", worst, coord.MaxLeaseMetaBytes)
	}
	const kib = 1 << 10
	if want := (worst + kib - 1) / kib * kib; coord.MaxLeaseMetaBytes != want {
		t.Fatalf("coord.MaxLeaseMetaBytes is %d, and the presence at its worst, %d bytes, rounds "+
			"up to %d: derive the bound again", coord.MaxLeaseMetaBytes, worst, want)
	}

	// THE STORE TAKES IT, through the one rule every backend refuses a
	// Meta by: measured above as the store measures it, and so taken.
	store := memory.New()
	lease, _, err := store.TryAcquire(t.Context(), coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute, Ungated: true, Meta: meta})
	if err != nil || lease == nil {
		t.Fatalf("the store refused the presence at its worst: (%v, %v)", lease, err)
	}
}

// longest is the value among values whose text the store's encoder spells
// longest.
func longest[S ~string](t *testing.T, values ...S) S {
	t.Helper()
	var out S
	best := -1
	for _, v := range values {
		if n := encodedLen(t, string(v)); n > best {
			out, best = v, n
		}
	}
	return out
}

// costliestByte is the one byte the store's encoder spells longest, for a
// field held to a byte count and no grammar: a control byte, spelled \u00XX.
func costliestByte(t *testing.T) string {
	t.Helper()
	var out string
	best := -1
	for b := range 256 {
		s := string([]byte{byte(b)})
		if n := encodedLen(t, s); n > best {
			out, best = s, n
		}
	}
	return out
}

// widestDistinct is n distinct strings of at most size bytes, each one the
// grammar check admits, whose encoding by the store is as long as any such
// string's can be.
//
// The widest string is built by length class: for each UTF-8 length of a
// character, the characters check admits whose encoding is longest; then the
// mix of classes filling size bytes whose encoding is longest. Distinct
// strings at that width differ only in which of a class's tied characters
// each position holds, so it fails rather than settle for a narrower set
// when the ties cannot tell n strings apart.
func widestDistinct(t *testing.T, what string, check func(string) error, size, n int) []string {
	t.Helper()
	var cost [utf8.UTFMax + 1]int
	var ties [utf8.UTFMax + 1][]rune
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		s := string(r)
		// Alone, a character is also the string's edges, so one only an
		// interior admits is not counted: the widest string is then made of
		// characters every position admits.
		if check(s) != nil {
			continue
		}
		l, c := len(s), encodedLen(t, s)
		switch {
		case c > cost[l]:
			cost[l], ties[l] = c, []rune{r}
		case c == cost[l]:
			ties[l] = append(ties[l], r)
		}
	}
	// best[b] is the longest encoding b bytes of admitted characters take,
	// and pick[b] the length class of its first character.
	best := make([]int, size+1)
	pick := make([]int, size+1)
	for b := 1; b <= size; b++ {
		best[b] = best[b-1] // a byte left unused
		for l := 1; l <= utf8.UTFMax && l <= b; l++ {
			if len(ties[l]) > 0 && best[b-l]+cost[l] > best[b] {
				best[b], pick[b] = best[b-l]+cost[l], l
			}
		}
	}
	var classes []int
	for b := size; b > 0; {
		if pick[b] == 0 {
			b--
			continue
		}
		classes = append(classes, pick[b])
		b -= pick[b]
	}
	out := make([]string, 0, n)
	seen := map[string]bool{}
	for i := 0; len(out) < n; i++ {
		var s strings.Builder
		rest := i
		for _, l := range classes {
			s.WriteRune(ties[l][rest%len(ties[l])])
			rest /= len(ties[l])
		}
		if rest > 0 {
			t.Fatalf("the widest %s admits only %d distinct strings, and the case needs %d",
				what, len(out), n)
		}
		v := s.String()
		if seen[v] {
			continue
		}
		if err := check(v); err != nil {
			t.Fatalf("the widest %s %q is refused by its own grammar: %v", what, v, err)
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// encodedLen is how many bytes the store's encoder spells s in, quotes aside.
func encodedLen(t *testing.T, s string) int {
	t.Helper()
	raw, err := coord.EncodeLeaseRecord(s)
	if err != nil {
		t.Fatalf("encode %q: %v", s, err)
	}
	return len(raw) - 2
}
