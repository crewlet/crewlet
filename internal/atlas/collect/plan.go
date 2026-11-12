package collect

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/integration"
)

// RetireAfter is how long after a first complete miss a second complete miss
// retires an entry: a day, so one read that fell on a vendor's listing while
// it changed under it, or on an hour the vendor's own view lagged, never
// removes anything, and a zone deleted on purpose leaves the map within two
// reads of the slowest allowed cadence rather than within one.
const RetireAfter = 24 * time.Hour

// MaxMissing bounds the entries one source's standing remembers as missed.
// A standing is one coordination record, and the store bounds a record well
// under a mebibyte: 8,192 entry ids with their first-missed instants encode
// to about 330 KiB. A deletion larger than that (every record of a large zone
// at once) retires the oldest misses first and the rest as the standing
// empties, a few reads later rather than never.
const MaxMissing = 8192

// Status is one source's standing in the coordination store: when it was
// last read, whether that read completed and was recorded whole, what it
// hashed to, what it cost, and the entries the source's voice holds that its
// reads have stopped seeing, since when.
type Status struct {
	Kind integration.Kind `json:"kind"`

	// LastAttemptAt is when the source was last read, and Completed whether
	// that read finished and every record it called for was published.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	Completed     bool      `json:"completed,omitempty"`

	// LastCompleteAt is when the source was last read whole and recorded
	// whole, Digest what that read hashed to, Entries how many entries it
	// held, and Calls and Withheld what it cost and what it refused.
	LastCompleteAt time.Time `json:"last_complete_at,omitzero"`
	Digest         string    `json:"digest,omitempty"`
	Entries        int       `json:"entries,omitempty"`
	Calls          int       `json:"calls,omitempty"`
	Withheld       int       `json:"withheld,omitempty"`

	// Recorded and Retired are what the last attempt wrote: records on the
	// log, and entries among them it retired; Purged is how many records
	// the log refused because an administrator purged what they named.
	Recorded int `json:"recorded,omitempty"`
	Retired  int `json:"retired,omitempty"`
	Purged   int `json:"purged,omitempty"`

	// LastError is the fault of the last attempt that returned one, cleared
	// by any attempt that does not.
	LastError string `json:"last_error,omitempty"`

	// Missing is every entry the voice holds that complete reads have
	// stopped seeing, grouped by the instant of the first read that missed
	// it; MissingOverflow how many more a deletion past [MaxMissing] holds
	// back for a later read.
	Missing         []Missing `json:"missing,omitempty"`
	MissingOverflow int       `json:"missing_overflow,omitempty"`
}

// Missing is a group of entries first missed by one complete read.
type Missing struct {
	Since   time.Time `json:"since"`
	Entries []string  `json:"entries"`
}

// RetryAfter is how soon a source whose last read did not complete is read
// again: the floor of the refresh interval, so a transient fault costs five
// minutes rather than an hour, and a lasting one costs no more than the
// smallest cadence a company may choose.
const RetryAfter = 5 * time.Minute

// Due reports whether the source is due a read at now, given the company's
// refresh interval: never read, read a whole interval ago, or read less than
// whole [RetryAfter] ago. Judged against the interval in force rather than
// one recorded, so a shortened interval applies at once.
func (s Status) Due(now time.Time, interval time.Duration) bool {
	if s.LastAttemptAt.IsZero() {
		return true
	}
	wait := interval
	if !s.Completed {
		wait = min(interval, RetryAfter)
	}
	return !now.Before(s.LastAttemptAt.Add(wait))
}

// missedSince is each missed entry's first-missed instant.
func (s Status) missedSince() map[string]time.Time {
	out := map[string]time.Time{}
	for _, g := range s.Missing {
		for _, id := range g.Entries {
			if prior, ok := out[id]; !ok || g.Since.Before(prior) {
				out[id] = g.Since
			}
		}
	}
	return out
}

// EntryRead is one record the plan calls for: one entry, the claims on it
// that are new or changed, and the slots of the voice's claims it withdraws.
// A retirement is one denied exists claim and every other slot withdrawn.
type EntryRead struct {
	Entry     atlas.Ref
	Claims    []atlas.Claim
	Withdrawn []string
	Retires   bool
}

// Plan is what one complete read changes: the records to publish, in a fixed
// order, and the source's standing after the read, its misses advanced.
type Plan struct {
	Records []EntryRead
	Status  Status
}

// Decide diffs one complete read against every live claim the log holds in
// the source's voice, and advances the source's standing. PURE: the same
// inputs give the same plan, so a pass cut off before it published is
// re-planned alike by the next.
//
// An entry the read lists is recorded whole and at once: a slot the read no
// longer holds on it is a change the vendor reported, never a skipped row. An
// entry the read does not list is a miss, retired only once a complete read
// at least [RetireAfter] after the first miss misses it too; one the read
// lists again is no longer missed. An entry already retired by this voice is
// neither re-retired nor missed.
func Decide(live []atlas.Claim, read Read, status Status, now time.Time) Plan {
	now = now.UTC()
	voice := status.Kind
	held := group(live)
	seen := group(read.Claims)
	var records []EntryRead
	for _, entry := range sortedRefs(seen) {
		want, have := seen[entry], held[entry]
		var rec EntryRead
		for _, slot := range sortedSlots(want) {
			c := want[slot]
			if prior, ok := have[slot]; !ok || !same(prior, c) {
				rec.Claims = append(rec.Claims, c)
			}
		}
		for _, slot := range sortedSlots(have) {
			if _, ok := want[slot]; !ok {
				rec.Withdrawn = append(rec.Withdrawn, slot)
			}
		}
		if len(rec.Claims) > 0 || len(rec.Withdrawn) > 0 {
			rec.Entry = entry
			records = append(records, rec)
		}
	}
	since := status.missedSince()
	var missing []missed
	retired := 0
	for _, entry := range sortedRefs(held) {
		if _, listed := seen[entry]; listed {
			continue
		}
		have := held[entry]
		if alreadyRetired(have) {
			continue
		}
		id := entry.ID().String()
		first, ok := since[id]
		if !ok {
			missing = append(missing, missed{id: id, since: now})
			continue
		}
		if now.Sub(first) < RetireAfter {
			missing = append(missing, missed{id: id, since: first})
			continue
		}
		rec := EntryRead{Entry: entry, Retires: true}
		for _, slot := range sortedSlots(have) {
			prior := have[slot]
			if prior.Facet.Kind == atlas.FacetExists {
				gone := prior
				gone.Denied, gone.ObservedAt, gone.Position = true, now, 0
				rec.Claims = append(rec.Claims, gone)
				continue
			}
			rec.Withdrawn = append(rec.Withdrawn, slot)
		}
		if len(rec.Claims) == 0 {
			// A voice holding facts on an entry it never said exists: its
			// retirement is still a denial of the entry, built fresh.
			if gone, err := atlas.Exists(entry, atlas.IntegrationSource(string(voice)), now); err == nil {
				gone.Denied = true
				rec.Claims = append(rec.Claims, gone)
			}
		}
		records = append(records, rec)
		retired++
	}
	slices.SortFunc(missing, func(a, b missed) int {
		return cmp.Or(a.since.Compare(b.since), cmp.Compare(a.id, b.id))
	})
	overflow := 0
	if len(missing) > MaxMissing {
		overflow = len(missing) - MaxMissing
		missing = missing[:MaxMissing]
	}
	status.Missing, status.MissingOverflow = groupMissed(missing), overflow
	status.Entries, status.Calls, status.Withheld = len(seen), read.Calls, len(read.Withheld)
	status.Digest = digest(read.Claims)
	status.Recorded, status.Retired = len(records), retired
	return Plan{Records: records, Status: status}
}

// missed is one entry a read missed, since when.
type missed struct {
	id    string
	since time.Time
}

// groupMissed is the misses grouped by their first instant, in that order.
func groupMissed(misses []missed) []Missing {
	var out []Missing
	for _, m := range misses {
		if n := len(out); n > 0 && out[n-1].Since.Equal(m.since) {
			out[n-1].Entries = append(out[n-1].Entries, m.id)
			continue
		}
		out = append(out, Missing{Since: m.since, Entries: []string{m.id}})
	}
	return out
}

// alreadyRetired reports a voice whose only word on an entry is its denial.
func alreadyRetired(have map[string]atlas.Claim) bool {
	if len(have) != 1 {
		return false
	}
	for _, c := range have {
		return c.Facet.Kind == atlas.FacetExists && c.Denied
	}
	return false
}

// same reports two claims on one slot that say the same thing.
func same(a, b atlas.Claim) bool {
	return a.Denied == b.Denied && a.Value.Equal(b.Value) && a.Facet.Target == b.Facet.Target
}

// group is the claims by entry, then by slot: a later claim on a slot the
// group already holds is dropped, as the builder drops it.
func group(claims []atlas.Claim) map[atlas.Ref]map[string]atlas.Claim {
	out := map[atlas.Ref]map[string]atlas.Claim{}
	for _, c := range claims {
		slots := out[c.Subject]
		if slots == nil {
			slots = map[string]atlas.Claim{}
			out[c.Subject] = slots
		}
		if _, ok := slots[c.Slot()]; !ok {
			slots[c.Slot()] = c
		}
	}
	return out
}

func sortedRefs(m map[atlas.Ref]map[string]atlas.Claim) []atlas.Ref {
	out := make([]atlas.Ref, 0, len(m))
	for r := range m {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b atlas.Ref) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key))
	})
	return out
}

func sortedSlots(m map[string]atlas.Claim) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// digest is what a read hashed to: every claim's entry, slot, value and
// denial, in a fixed order, so two reads that found the same world hash
// alike whatever order the vendor listed it in.
func digest(claims []atlas.Claim) string {
	lines := make([]string, 0, len(claims))
	for _, c := range claims {
		denied := "0"
		if c.Denied {
			denied = "1"
		}
		lines = append(lines, strings.Join([]string{c.Subject.String(), c.Slot(), string(c.Value.Type),
			c.Value.Text, denied}, "\x1f"))
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
