package codeindex

import (
	"cmp"
	"math"
	"slices"
)

// HOW A PLAN IS LOOKED UP IN A SEGMENT (Plan.Candidates): each clause's blobs,
// intersected. A clause is an OR of literals and a literal an AND of its
// trigrams, so a clause's blobs are those holding every trigram of one of
// its literals — and, where it holds a folded literal, every blob holding an
// escape.
//
// A LOOKUP READS EACH LIST ITS PLAN NAMES ONCE. A clause's literals share
// trigrams: an alternation crossed with a word holds the word in every
// literal — (?i)(create|delete|update)User is three literals that each hold
// "use" and "ser" — and an alternation of names holds their common parts in
// some of them; a plan's clauses share them too, foo_a.*foo_b's two holding
// foo's; and the commonest trigrams have a segment's longest lists. Looked
// up a literal at a time, a list was read once for every literal holding it,
// and decoded whole each time into a buffer of its own: work and garbage
// k·L for a list k literals hold, where L would do — a clause of 64 words
// sharing lists over 2 048 blobs read 289 280 bytes of postings and
// allocated 1 457 152, where reading each list once reads 12 288 and
// allocates 100 288. So a plan sorts its trigrams by where a lookup reads
// them (clause, ref), and each is read once:
//
//   - THE TRIGRAMS EVERY LITERAL OF A CLAUSE HOLDS (clause.shared) are
//     hoisted out of the union of what its literals admit: the blobs holding
//     them all, C, are read once, and each literal admits only blobs among
//     them — ∪ᵢ(C ∩ Rᵢ) = C ∩ ∪ᵢRᵢ, Rᵢ the blobs holding literal i's other
//     trigrams. A literal holding nothing beyond them admits all of C, and
//     so the clause does.
//   - A TRIGRAM MORE THAN ONE PLACE READS — two literals' trigram, of one
//     clause or of two, or a clause's shared trigram that some other place
//     reads too — is decoded once a lookup for all of them (Plan.held,
//     decoded), and only as far as the furthest blob one of them needs.
//     Hoisting alone reads such a list once a literal holding it, and an
//     alternation whose one literal lacks a list the rest share —
//     (createUser|deleteUser|findAccount) — hoists nothing at all.
//   - A TRIGRAM ONE PLACE READS is read there alone, as that place narrows
//     what it admits: into nothing, and only until the list passes the last
//     blob it keeps (reading.listed).
//
// And a clause another implies — every blob the other admits, it admits
// too — is not looked up at all (unimplied): a pattern repeating a part,
// (foo.*){100}, is a hundred clauses of foo to regexlit.Required, each of
// which would hold every blob holding foo.
//
// A LITERAL READS FIRST THE LEAST OF WHAT IT MUST HOLD, as one looked up
// alone read its shortest list first: a list of its own decoded straight
// into the clause's union, or blobs decoded for other places too, which it
// does not narrow where they lie but into the union, by its next need. Each
// other need then narrows what the union holds of the literal, in place: a
// list of its own read as it is intersected, decoded blobs searched by steps
// that double (gallop) — so what narrowing by C or a shared list costs a
// literal is what it holds, never the length of what it shares. A lookup so
// allocates each clause's C, each list more than one place reads decoded
// once, and what its literals admit, in the two buffers the union merges
// them between; and it reads no list twice. Its reading counts the bytes,
// which the suite holds every lookup to (TestAClauseReadsEachListItNamesOnce,
// TestAPlanReadsEachListItsClausesNameOnce,
// TestAClauseAdmitsWhatItsLiteralsLookedUpAloneAdmit).

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
	for d.read.next() {
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
	return gallop(dst, set, n.held.got), nil
}

// listed appends to dst the ordinals of set, ascending, that list names too.
// list is read as it is intersected, into nothing, and only until it passes
// set's last ordinal: what narrowing a few blobs by a list costs is the list
// as far as the last of them, never a buffer its length, and never its tail.
// dst may be set[:0], to narrow set in its own storage, since what is kept is
// never ahead of what is read.
func (r *reading) listed(dst, set []uint32, list []byte) ([]uint32, error) {
	o, i := r.s.ordinals(list), 0
	for i < len(set) && o.next() {
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
// ascending. Each of set's is looked for from where the one before it was:
// by a step doubled until it passes it, then a binary search back. So a few
// ordinals against many cost the few times the logarithm of the distance
// between them, never a walk of sorted — which a clause's literals, each
// narrowing a few blobs by blobs they all share, would otherwise take once
// a literal. dst may be set[:0], since what is kept is never ahead of what is
// read.
func gallop(dst, set, sorted []uint32) []uint32 {
	lo := 0
	for _, x := range set {
		// Every ordinal of sorted before lo is below x; probe ahead of it
		// until one is not, or sorted ends.
		hi, step := lo, 1
		for hi < len(sorted) && sorted[hi] < x {
			lo, hi, step = hi+1, hi+step, step<<1
		}
		i, found := slices.BinarySearch(sorted[lo:min(hi+1, len(sorted))], x)
		lo += i
		if found {
			dst = append(dst, x)
			lo++
		}
		if lo == len(sorted) {
			break
		}
	}
	return dst
}
