package codeindex

import (
	"cmp"
	"math"
	"math/bits"
	"slices"
)

// HOW A PLAN IS LOOKED UP IN A SEGMENT (Plan.Candidates): each clause's blobs,
// intersected. A clause is an OR of literals and a literal an AND of its
// trigrams; where it holds a folded literal, every blob holding an escape also
// counts. A LOOKUP READS EACH LIST ITS PLAN NAMES ONCE: literals and clauses
// share trigrams ((?i)(create|delete|update)User holds "use" in all three), and
// the commonest have the longest lists, so a list looked up per literal costs
// k·L work and garbage where L would do. So a plan sorts its trigrams by where
// a lookup reads them (clause, ref), in three kinds below. A clause another
// implies is not looked up at all (unimplied): (foo.*){100} is a hundred
// clauses of foo to regexlit.Required.

// THE TRIGRAMS EVERY LITERAL OF A CLAUSE HOLDS (clause.shared) are hoisted out
// of the union: the blobs holding them all, C, are read once, and
// ∪ᵢ(C ∩ Rᵢ) = C ∩ ∪ᵢRᵢ, Rᵢ the blobs holding literal i's other trigrams.
// A TRIGRAM MORE THAN ONE PLACE READS is decoded once a lookup for all of them
// (Plan.held, decoded), only as far as the furthest blob any needs; hoisting
// alone fails when one literal lacks a list the rest share
// ((createUser|deleteUser|findAccount) hoists nothing). A TRIGRAM ONE PLACE
// READS is read there alone, until the list passes the last blob that place
// keeps (reading.listed).

// A LITERAL READS FIRST THE LEAST OF WHAT IT MUST HOLD, as one looked up alone
// read its shortest list first: a list of its own decoded straight into the
// clause's union, or blobs decoded for other places too, narrowed by its next
// need. Each other need narrows the literal's share in place (own lists as
// intersected, decoded blobs by doubling steps, gallop), so narrowing costs what
// the literal holds, never the length of what it shares
// (TestGallopLooksAtAFewOrdinalsForEachItLooksFor). The reading counts bytes, and
// TestAClauseReadsEachListItNamesOnce, TestAPlanReadsEachListItsClausesNameOnce,
// TestALookupReadsAListOnlyAsFarAsTheBlobsItKeeps and
// TestALookupAdmitsWhatItsLiteralsLookedUpAloneAdmit hold it. Time: BenchmarkLookup.

// reading is one lookup's reads of a segment's posting lists — every clause
// of one plan, in one segment — and the bytes of them it has read, each list
// as far as it was read: what decoding cost the lookup.
type reading struct {
	s    *Segment
	read int
}

// blobs is the ordinals of the blobs of r's segment that every clause of p
// admits, ascending: what Candidates answers, before it drops a skipped one.
func (p *Plan) blobs(r *reading) ([]uint32, error) {
	held := r.held(p.held)
	var set []uint32
	for i, c := range p.index {
		got, err := c.blobs(r, held)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			set = got
		} else {
			set = intersect(set, got)
		}
		if len(set) == 0 {
			return nil, nil
		}
	}
	return set, nil
}

// blobs is the ordinals of the blobs of r's segment that can hold one of c's
// literals, ascending — and, where c holds a folded literal, of every blob
// holding an escape, which is admitted whatever trigrams it holds. A clause
// of an alternation can hold a hundred literals, so their blobs are merged
// as one union, never folded in one at a time. held is the plan's lists
// more than one place reads, decoded once for this lookup (reading.held).
func (c clause) blobs(r *reading, held []decoded) ([]uint32, error) {
	var u union
	if err := c.literals(r, &u, held); err != nil {
		return nil, err
	}
	if c.folded {
		u.add(r.s.escapes)
	}
	return u.merged(), nil
}

// literals adds to u the blobs of r's segment that hold one of c's literals,
// each list c names read once (the head of this file).
func (c clause) literals(r *reading, u *union, held []decoded) error {
	var needs []need
	var shared decoded
	if len(c.shared) > 0 {
		var ok bool
		if needs, ok = r.needs(needs, c.shared, held); !ok {
			// A trigram every literal holds is in no blob: none holds one.
			return nil
		}
		sortNeeds(needs)
		var err error
		if c.whole {
			// The clause admits every blob holding them: read straight
			// into the union, and no literal read beyond them.
			u.buf, err = r.admitted(u.buf, needs)
			u.end()
			return err
		}
		// Into a buffer made once, as long as the least list's bytes, which
		// bound the blobs it names and so every blob they all name.
		shared.got, err = r.admitted(make([]uint32, 0, needs[0].size), needs)
		if err != nil || len(shared.got) == 0 {
			return err
		}
	}
	for _, a := range c.alts {
		needs = needs[:0]
		if len(shared.got) > 0 {
			needs = append(needs, need{held: &shared, size: len(shared.got)})
		}
		var ok bool
		if needs, ok = r.needs(needs, a.rest, held); !ok {
			// A trigram of it is in no blob: it admits none.
			continue
		}
		sortNeeds(needs)
		var err error
		if u.buf, err = r.admitted(u.buf, needs); err != nil {
			return err
		}
		u.end()
	}
	return nil
}

// need is one thing every blob a place admits holds, as the place reads it:
// a posting list it reads alone (list), as it narrows; or blobs decoded once
// for every place that needs them (held) — those holding every trigram a
// clause's literals all hold, or a list more than one place reads.
type need struct {
	list []byte
	held *decoded
	// size orders a place's needs, least first: a list's bytes, which bound
	// the blobs it names from above, since each takes one at the least — or
	// the count of the blobs a clause's literals all hold, decoded whole.
	size int
}

// sortNeeds puts needs in the order a lookup reads them: least first.
func sortNeeds(needs []need) {
	slices.SortFunc(needs, func(a, b need) int { return cmp.Compare(a.size, b.size) })
}

// decoded is a posting list decoded once a lookup for every place that
// needs it, and kept as far as one has: the read, where it stands, and the
// ordinals it has read, ascending. The blobs holding every trigram a
// clause's literals all hold are one decoded whole as they are made.
type decoded struct {
	read ordinals
	got  []uint32
	// bytes is the list's length, encoded: how a place orders it among its
	// needs, and none for a trigram in no blob, whose list is nothing.
	bytes int
}

// upTo reads d until it holds an ordinal at or past last, or its list ends:
// as far as a place narrowing blobs up to last needs it. Its buffer is made
// once, as long as the list's bytes, which bound every ordinal it can hold.
func (d *decoded) upTo(r *reading, last uint32) error {
	if n := len(d.got); n > 0 && d.got[n-1] >= last {
		return nil
	}
	if d.got == nil {
		d.got = make([]uint32, 0, len(d.read.list))
	}
	left := len(d.read.list)
	for d.read.delta() || d.read.next() {
		d.got = append(d.got, d.read.at)
		if d.read.at >= last {
			break
		}
	}
	r.read += left - len(d.read.list)
	return d.read.err
}

// held is the decoded list of each of ts for one lookup, each looked up and
// none of it yet read: the plan's trigrams more than one place reads
// (Plan.held), where every place that reads one finds it.
func (r *reading) held(ts []Trigram) []decoded {
	out := make([]decoded, len(ts))
	for i, t := range ts {
		if list, ok := r.s.lookup(t); ok {
			out[i] = decoded{read: r.s.ordinals(list), bytes: len(list)}
		}
	}
	return out
}

// needs appends to needs one for each of refs — a list the one place
// reading it reads alone, or one decoded for every place (held) — or answers
// false where one of refs is in no blob of the segment, so that no blob
// holds them all.
func (r *reading) needs(needs []need, refs []ref, held []decoded) ([]need, bool) {
	for _, f := range refs {
		if f.held >= 0 {
			d := &held[f.held]
			if d.bytes == 0 {
				return needs, false
			}
			needs = append(needs, need{held: d, size: d.bytes})
			continue
		}
		list, ok := r.s.lookup(f.t)
		if !ok {
			return needs, false
		}
		needs = append(needs, need{list: list, size: len(list)})
	}
	return needs, true
}

// admitted appends to dst, ascending, the ordinals of the blobs every one of
// needs holds, needs least first. The least is read whole: a list decoded
// straight into dst, grown once by the list's bytes; or decoded blobs, which
// other places still read, narrowed by the next need into dst. Every other
// need only narrows what dst holds of them, in place, until nothing is left.
func (r *reading) admitted(dst []uint32, needs []need) ([]uint32, error) {
	from, rest := len(dst), needs[1:]
	var err error
	if first := needs[0]; first.list != nil {
		dst, err = r.decode(first.list, slices.Grow(dst, len(first.list)))
	} else if err = first.held.upTo(r, math.MaxUint32); err == nil {
		if len(rest) == 0 {
			return append(dst, first.held.got...), nil
		}
		dst, err = r.narrow(slices.Grow(dst, len(first.held.got)), first.held.got, rest[0])
		rest = rest[1:]
	}
	for _, n := range rest {
		if err != nil || len(dst) == from {
			break
		}
		var kept []uint32
		if kept, err = r.narrow(dst[from:from], dst[from:], n); err == nil {
			dst = dst[:from+len(kept)]
		}
	}
	if err != nil {
		return nil, err
	}
	return dst, nil
}

// narrow appends to dst the ordinals of set, ascending, that n holds too: a
// list its place reads alone, read as it is intersected (listed), or decoded
// blobs read as far as set's last and searched (gallop). dst may be set[:0],
// since what either keeps is never ahead of what it has read.
func (r *reading) narrow(dst, set []uint32, n need) ([]uint32, error) {
	if n.list != nil {
		return r.listed(dst, set, n.list)
	}
	if len(set) == 0 {
		return dst, nil
	}
	if err := n.held.upTo(r, set[len(set)-1]); err != nil {
		return nil, err
	}
	kept, _ := gallop(dst, set, n.held.got)
	return kept, nil
}

// listed appends to dst the ordinals of set, ascending, that list names too.
// list is read as it is intersected, into nothing, and only until it passes
// set's last ordinal: what narrowing a few blobs by a list costs is the list
// as far as the last of them, never a buffer its length, and never its tail.
// dst may be set[:0], to narrow set in its own storage, since what is kept is
// never ahead of what is read.
func (r *reading) listed(dst, set []uint32, list []byte) ([]uint32, error) {
	o, i := r.s.ordinals(list), 0
	for i < len(set) && (o.delta() || o.next()) {
		for i < len(set) && set[i] < o.at {
			i++
		}
		if i < len(set) && set[i] == o.at {
			dst = append(dst, o.at)
			i++
		}
	}
	r.read += len(list) - len(o.list)
	if o.err != nil {
		return nil, o.err
	}
	return dst, nil
}

// decode is Segment.decode, its bytes counted.
func (r *reading) decode(list []byte, dst []uint32) ([]uint32, error) {
	r.read += len(list)
	return r.s.decode(list, dst)
}

// gallop appends to dst the ordinals of set that sorted holds too, both
// ascending. Each of set's is looked for from where the one before it was: by
// a step doubled until it passes it, then a binary search back. So a few
// ordinals against many cost the few times the log of the distance between
// them, never a walk of sorted, which each literal narrowing by shared blobs
// would otherwise take. dst may be set[:0]: what is kept is never ahead of what
// is read.
//
// probes is the ordinals of sorted it looked at, at most 2(⌊log₂ n⌋+2) per
// element of set, n sorted's length. What it keeps cannot show gallop's point
// (a walk keeps the same), so the suite holds the probes
// (TestGallopLooksAtAFewOrdinalsForEachItLooksFor).
func gallop(dst, set, sorted []uint32) (kept []uint32, probes int) {
	lo := 0
	for _, x := range set {
		// Every ordinal of sorted before lo is below x; probe ahead of it
		// until one is not, or sorted ends.
		hi, step := lo, 1
		for hi < len(sorted) && sorted[hi] < x {
			lo, hi, step = hi+1, hi+step, step<<1
			probes++
		}
		window := sorted[lo:min(hi+1, len(sorted))]
		i, found := slices.BinarySearch(window, x)
		// The probe that stopped the steps, and the search's.
		probes += 1 + bits.Len(uint(len(window)))
		lo += i
		if found {
			dst = append(dst, x)
			lo++
		}
		if lo == len(sorted) {
			break
		}
	}
	return dst, probes
}
