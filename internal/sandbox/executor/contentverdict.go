package executor

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE VERDICT OF A READING BY CONTENT ([contentVerdict]): which of a round's
// calls a session holds where its harness names no call to the engine, from
// the parts each class of calls ([sandbox.CallClass]) has and the steps they
// lie in. It is any such reader's, harness-neutral: the reader walks its own
// records for the facts (OpenCode's, opencodeholdings.go; Qwen Code's,
// qwenholdings.go; Gemini CLI's, geminiholdings.go), and matches a part to its
// class here ([matchedClass]). The package doc's "A session's calls are read by
// what they were" has the pool, the order check and why it never holds falsely.

// contentVerdict is a reading's answer from its walk's facts, parts each
// stated class's counted parts by the steps they lie in, in the order read:
// the pool's calls counted by class, each class held where every call of it
// has a part and no call the log cannot state may be of it, and the held
// classes' parts in an order the log's explains (the order check). A PURE
// FUNCTION of the facts and the ask, so a take can take it outside a relaunch.
func contentVerdict(ask sandbox.HoldingsAsk, parts map[sandbox.CallClass][]int,
	compacted bool) (sandbox.SessionHoldings, error) {

	pool := contentPool(ask)
	classes := make(map[sandbox.CallClass]sandbox.ClassRecords, len(pool.calls))
	for class, calls := range pool.calls {
		counted := min(len(parts[class]), calls+1)
		records := sandbox.ClassRecords{Calls: calls, Parts: counted, Wild: pool.wild(class)}
		records.Held = records.Calls >= 1 && records.Parts == records.Calls && !records.Wild
		classes[class] = records
	}
	if !contentInOrder(ask, parts, classes) {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session holds the run's calls in an order its " +
			"log does not explain"}
	}
	members := map[string]sandbox.CallClass{}
	for _, c := range ask.Calls {
		if class, ok := sandbox.ClassOf(c); ok && c.Asked {
			members[c.ID] = class
		}
	}
	return sandbox.SessionHoldings{Read: true, Kind: sandbox.HoldingsByContent, Members: members, Classes: classes,
		Compacted: compacted}, nil
}

// contentInOrder reports whether the held classes' calls, taken in admission
// order, meet each class's next part in a step never before the last one met:
// a step's calls run at once, and the next step's only after they all joined.
func contentInOrder(ask sandbox.HoldingsAsk, parts map[sandbox.CallClass][]int,
	classes map[sandbox.CallClass]sandbox.ClassRecords) bool {

	calls := slices.Clone(ask.Calls)
	slices.SortStableFunc(calls, func(a, b sandbox.LoggedCall) int { return cmp.Compare(a.Seq, b.Seq) })
	next, at := map[sandbox.CallClass]int{}, 0
	for _, c := range calls {
		class, ok := sandbox.ClassOf(c)
		if !ok || c.Held || !classes[class].Held {
			continue
		}
		step := parts[class][next[class]]
		if step < at {
			return false
		}
		next[class]++
		at = step
	}
	return true
}

// contentCallPool is a reading's pool: the calls of each stated class, the
// digests they carry and whether one carries none; and the wildcards, every
// call that may have left a part the log cannot state the class of.
type contentCallPool struct {
	calls      map[sandbox.CallClass]int
	digests    map[sandbox.CallClass]map[string]bool
	digestless map[sandbox.CallClass]bool
	// named are the wildcards known by tool and digest ("" where none); held
	// the calls a person's approval held, by tool, failure and answer; and
	// unnamed whether a call or count unlisted names no tool, onlyFailed
	// whether every call they count failed, never so for one in flight.
	named               []sandbox.CallIdentity
	held                []heldWildcard
	unnamed, onlyFailed bool
}

// heldWildcard is a call a person's approval held: its part, if any, answers
// with the held answer, whose text key is text where stated.
type heldWildcard struct {
	name   string
	failed bool
	text   sandbox.TextKey
	stated bool
}

// contentPool is ask's pool: every call but a held one by its class, or a
// wildcard where its class is not stated; each held call, in flight or
// dropped call a wildcard.
func contentPool(ask sandbox.HoldingsAsk) contentCallPool {
	pool := contentCallPool{calls: map[sandbox.CallClass]int{}, digests: map[sandbox.CallClass]map[string]bool{},
		digestless: map[sandbox.CallClass]bool{}, onlyFailed: true}
	for _, c := range ask.Calls {
		if c.Held {
			text, stated := sandbox.TextKeyOf(c.Output, c.OutputBytes)
			pool.held = append(pool.held, heldWildcard{name: c.Name, failed: c.Failed, text: text, stated: stated})
			continue
		}
		class, ok := sandbox.ClassOf(c)
		if !ok {
			pool.named = append(pool.named, sandbox.CallIdentity{Name: c.Name, Digest: c.Digest})
			continue
		}
		pool.calls[class]++
		if c.Digest == "" {
			pool.digestless[class] = true
			continue
		}
		if pool.digests[class] == nil {
			pool.digests[class] = map[string]bool{}
		}
		pool.digests[class][c.Digest] = true
	}
	for _, id := range ask.Unlisted {
		if id.Name == "" {
			pool.unnamed, pool.onlyFailed = true, pool.onlyFailed && !id.InFlight && id.OnlyFailed
			continue
		}
		pool.named = append(pool.named, id)
	}
	return pool
}

// wild reports whether a call the log cannot state the class of may be of
// class: one of its tool whose digest is none, or one a call of the class
// carries, or whose class lacks a digest; a held call of its tool, failure
// and answer; or a count with no name, of failures only where it says so.
func (p contentCallPool) wild(class sandbox.CallClass) bool {
	if p.unnamed && (!p.onlyFailed || class.Failed) {
		return true
	}
	for _, id := range p.named {
		if id.Name == class.Name && (id.Digest == "" || p.digestless[class] || p.digests[class][id.Digest]) {
			return true
		}
	}
	for _, h := range p.held {
		if h.name == class.Name && h.failed == class.Failed && (!h.stated || h.text == class.Text) {
			return true
		}
	}
	return false
}

// matchedClass is the one class of classes whose output a part's text is,
// read off its digest d, false where none is or more than one: a part two cut
// classes both match could hold both classes' calls, so it counts for
// neither. A completed part's empty or blank text never counts, since a
// provider may hand it otherwise.
func matchedClass(classes []sandbox.CallClass, d *textDigest, failed bool) (sandbox.CallClass, bool) {
	if !slices.ContainsFunc(classes, func(c sandbox.CallClass) bool { return d.mayBe(c.Text) }) ||
		!failed && d.blank {
		return sandbox.CallClass{}, false
	}
	var (
		found   sandbox.CallClass
		matches int
	)
	for _, c := range classes {
		if d.mayBe(c.Text) && d.matches(c.Text) {
			found, matches = c, matches+1
		}
	}
	return found, matches == 1
}

// mayBe reports whether d's text can be the output key is of: of its size,
// and never of a key of size zero, so an empty failed part never holds a call
// logged with an empty output. The decode walk also held a text's span as
// written to within a third and six times the size; that span holds of every
// text of the size (a byte as written decodes to at most three, an escape of
// at most six bytes to at least one), so the size decides alone.
func (d *textDigest) mayBe(key sandbox.TextKey) bool { return key.Bytes > 0 && d.n == key.Bytes }

// matches reports whether d's text, of key's size, is the output key is of:
// every byte, where the log kept it whole; all the opening the log kept,
// where it cut it, never a text the take's scrub rewrote, which may differ
// past it.
func (d *textDigest) matches(key sandbox.TextKey) bool {
	if key.Whole {
		return d.whole == key.Sum
	}
	i, kept := slices.BinarySearch(d.openings, key.Opening)
	return kept && key.Opening <= d.n && !d.marker && d.sums[i] == key.Sum
}

// logPlaceAskFault is why ask is not one a round's log can make of calls keyed
// by their place in it, "" where it is: what a reading by content checks
// before anything is opened ([OpenCode.checkHoldings]).
func logPlaceAskFault(ask sandbox.HoldingsAsk) string {
	if len(ask.Calls) == 0 || len(ask.Calls) > sandbox.MaxBridgeCalls {
		return fmt.Sprintf("%d calls, where a round's log lists 1 to %d", len(ask.Calls), sandbox.MaxBridgeCalls)
	}
	carried, asked := make(map[string]int, len(ask.Calls)), 0
	for _, c := range ask.Calls {
		carried[c.ID]++
		if c.Asked {
			asked++
		}
	}
	if asked == 0 {
		return "none of them asked about"
	}
	for i, c := range ask.Calls {
		switch {
		case c.Seq <= 0:
			return fmt.Sprintf("call %d has no admission place", i+1)
		case !c.Asked:
		case c.Appended <= 0 || c.ID != strconv.Itoa(c.Appended):
			return fmt.Sprintf("call %d is asked about, and is not keyed by its place in the log", i+1)
		case c.Held:
			return fmt.Sprintf("call %d is asked about, and a person's approval held it", i+1)
		case carried[c.ID] > 1:
			return fmt.Sprintf("call %d is asked about, and another call of the ask carries its key", i+1)
		}
	}
	inFlight, unnamed, named := 0, 0, 0
	for _, id := range ask.Unlisted {
		switch {
		case id.InFlight:
			inFlight++
		case id.Name == "":
			unnamed++
		default:
			named += len(id.Name) + len(id.Digest)
		}
	}
	switch {
	case inFlight > sandbox.MaxCallsInFlight:
		return fmt.Sprintf("%d calls in flight, where the log holds at most %d", inFlight, sandbox.MaxCallsInFlight)
	case unnamed > 1:
		return "more than one count of dropped calls with no name"
	case named > sandbox.MaxElidedIdentityBytes:
		return "dropped calls whose names and digests weigh more than the log's tally holds"
	}
	return ""
}
