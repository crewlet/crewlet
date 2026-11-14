package collect

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/textcut"
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

	// Unread is every part of the vendor the last complete read was refused,
	// with what to grant: the entries of such a part are as an earlier read
	// left them.
	// UnreadOverflow is how many more parts were refused than [MaxUnread]
	// findings name.
	Unread         []Finding `json:"unread,omitempty"`
	UnreadOverflow int       `json:"unread_overflow,omitempty"`

	// Scopes is every part of the world the last complete read visited that
	// keeps a standing of its own, as each server of the SSH source does:
	// when it was last read whole, and how many of its parts the last
	// complete read was refused.
	Scopes []Scope `json:"scopes,omitempty"`

	// Missing is every entry the voice holds that complete reads have
	// stopped seeing, grouped by the instant of the first read that missed
	// it; MissingOverflow how many more a deletion past [MaxMissing] holds
	// back for a later read.
	Missing         []Missing `json:"missing,omitempty"`
	MissingOverflow int       `json:"missing_overflow,omitempty"`
}

// Finding is one part of a vendor a source may not read, and what to grant.
// Scope names the scope the part belongs to ([Scope]), where it has one.
type Finding struct {
	Part   string `json:"part"`
	Scope  string `json:"scope,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Scope is one part of a source's world with a standing of its own: one
// server of the SSH source.
type Scope struct {
	ID string `json:"id"`

	// LastReadAt is the last complete read that read every part of the
	// scope, zero where none has yet; Unread how many of its parts the last
	// complete read was refused.
	LastReadAt time.Time `json:"last_read_at,omitzero"`
	Unread     int       `json:"unread,omitempty"`
}

// MaxScopes bounds the scopes a standing carries. A standing is one
// coordination record: 2,048 scopes with a 64-byte id each encode to about
// 250 KiB, and a company declares its servers by hand, far below that.
const MaxScopes = 2048

// MaxUnread bounds the findings a standing carries, and MaxFindingLength the
// text of each: a source reads a handful of listings, and the standing is one
// coordination record.
const (
	MaxUnread        = 16
	MaxFindingLength = 512
)

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

// RefreshFloor is the least time between two reads of one source a person
// can ask for: the floor of the refresh interval, so "Refresh now" reads a
// vendor no more often than the smallest cadence a company may set.
const RefreshFloor = config.MinAtlasRefreshInterval

// Request is a person's ask that a source be read now, waiting for the duty.
type Request struct {
	RequestedAt time.Time `json:"requested_at"`
	// By is the handle or login of whoever asked.
	By string `json:"by,omitempty"`
}

// MaxRequestByLength bounds who a request names: one handle or login.
const MaxRequestByLength = 128

// Waits reports whether the request asks for a read the standing has not
// made yet: one made at or after the ask has answered it.
func (r Request) Waits(s Status) bool {
	return !r.RequestedAt.IsZero() && r.RequestedAt.After(s.LastAttemptAt)
}

// RefreshableAt is the first instant a person's ask for a read is honoured:
// [RefreshFloor] after the last attempt, and at once for a source never read.
func (s Status) RefreshableAt() time.Time {
	if s.LastAttemptAt.IsZero() {
		return time.Time{}
	}
	return s.LastAttemptAt.Add(RefreshFloor)
}

// NextReadAt is when the source is next read on its own, given the company's
// refresh interval; zero for a source never read, which is due now.
func (s Status) NextReadAt(interval time.Duration) time.Time {
	if s.LastAttemptAt.IsZero() {
		return time.Time{}
	}
	wait := interval
	if !s.Completed {
		wait = min(interval, RetryAfter)
	}
	return s.LastAttemptAt.Add(wait)
}

// Due reports whether the source is due a read at now, given the company's
// refresh interval: never read, read a whole interval ago, or read less than
// whole [RetryAfter] ago. Judged against the interval in force rather than
// one recorded, so a shortened interval applies at once.
func (s Status) Due(now time.Time, interval time.Duration) bool {
	return !now.Before(s.NextReadAt(interval))
}

// DueFor reports whether the source is due a read at now: on its own
// cadence ([Status.Due]), or because a person asked ([Request]) and
// [RefreshFloor] has passed since the last attempt.
func (s Status) DueFor(now time.Time, interval time.Duration, asked Request) bool {
	if s.Due(now, interval) {
		return true
	}
	return asked.Waits(s) && !now.Before(s.RefreshableAt())
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
//
// A claim from a part the read was refused ([Read.Unread]) is carried as the
// log holds it: it is neither withdrawn nor missed, and a miss it already had
// is kept, never advanced. An entry the voice never said exists is released
// rather than retired: its slots are withdrawn, and nothing is denied.
func Decide(live []atlas.Claim, read Read, status Status, now time.Time) Plan {
	now = now.UTC()
	held := group(live)
	seen := group(read.Claims)
	carried := carry(held, seen, read.Unread)
	records := moved(held, seen)
	since := status.missedSince()
	var missing []missed
	retired := 0
	for _, entry := range sortedRefs(held) {
		have := held[entry]
		id := entry.ID().String()
		first, ok := since[id]
		if _, listed := seen[entry]; listed {
			// A MISS IS KEPT, never advanced, for an entry only a refused
			// part holds: the refusal is no read of it either way.
			if carried[entry] && ok && !alreadyRetired(have) {
				missing = append(missing, missed{id: id, since: first})
			}
			continue
		}
		if alreadyRetired(have) {
			continue
		}
		if !ok {
			missing = append(missing, missed{id: id, since: now})
			continue
		}
		if now.Sub(first) < RetireAfter {
			missing = append(missing, missed{id: id, since: first})
			continue
		}
		rec := EntryRead{Entry: entry}
		for _, slot := range sortedSlots(have) {
			prior := have[slot]
			if prior.Facet.Kind == atlas.FacetExists {
				gone := prior
				gone.Denied, gone.ObservedAt, gone.Position = true, now, 0
				rec.Claims = append(rec.Claims, gone)
				rec.Retires = true
				continue
			}
			rec.Withdrawn = append(rec.Withdrawn, slot)
		}
		// A VOICE THAT NEVER SAID THE ENTRY EXISTS denies nothing: what it
		// said about the entry is withdrawn, and whoever holds the entry
		// still does. A denial here would retire another source's entry.
		records = append(records, rec)
		if rec.Retires {
			retired++
		}
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
	status.Unread, status.UnreadOverflow = findings(refused(read.Unread, held))
	status.Scopes = scopes(status.Scopes, read, now)
	status.Recorded, status.Retired = len(records), retired
	return Plan{Records: records, Status: status}
}

// moved is one record per entry of seen whose claims differ from the ones
// held on it: the claims that are new or changed, and the held slots seen no
// longer holds. An entry seen does not list is not its concern.
func moved(held, seen map[atlas.Ref]map[string]atlas.Claim) []EntryRead {
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
	return records
}

// Replace is the records that make a voice's live claims exactly want, AT
// ONCE: what is new or changed is claimed, and every other slot the voice
// holds is withdrawn, an entry want no longer lists released whole. For a
// voice whose reading is exact at the instant it is made, as a repository's
// files at one commit are: a vendor's listing is not, and takes [Decide],
// whose retirement waits for two complete misses. Nothing is denied: the
// voice stops saying it, and whoever else holds the entry still does. PURE.
func Replace(live, want []atlas.Claim) []EntryRead {
	held, seen := group(live), group(want)
	records := moved(held, seen)
	for _, entry := range sortedRefs(held) {
		if _, listed := seen[entry]; listed {
			continue
		}
		records = append(records, EntryRead{Entry: entry, Withdrawn: sortedSlots(held[entry])})
	}
	return records
}

// carry adds to seen every claim the voice holds that a refused part covers,
// as the log holds it, and answers the entries only such claims list.
func carry(held, seen map[atlas.Ref]map[string]atlas.Claim, unread []Unread) map[atlas.Ref]bool {
	carried := map[atlas.Ref]bool{}
	for entry, slots := range held {
		for slot, c := range slots {
			if !slices.ContainsFunc(unread, func(u Unread) bool { return u.Holds != nil && u.Holds(c) }) {
				continue
			}
			if seen[entry] == nil {
				seen[entry] = map[string]atlas.Claim{}
				carried[entry] = true
			}
			if _, ok := seen[entry][slot]; !ok {
				seen[entry][slot] = c
			}
		}
	}
	return carried
}

// refused is the unread parts a standing names: every part the credential
// was expected to read, and an optional one ([Unread.Optional]) only where
// the voice holds an entry the part said exists. An optional part no entry
// was ever read from is a permission the company never gave, which is no
// refusal.
func refused(unread []Unread, held map[atlas.Ref]map[string]atlas.Claim) []Unread {
	var out []Unread
	for _, u := range unread {
		if !u.Optional || holdsEntry(u, held) {
			out = append(out, u)
		}
	}
	return out
}

// holdsEntry reports whether the voice holds a live exists claim from the
// part. ONLY AN ENTRY COUNTS: a fact the part shares with another part (what
// an account is used for) would name a part nothing was ever read from.
func holdsEntry(u Unread, held map[atlas.Ref]map[string]atlas.Claim) bool {
	if u.Holds == nil {
		return false
	}
	for _, slots := range held {
		for _, c := range slots {
			if c.Facet.Kind == atlas.FacetExists && !c.Denied && u.Holds(c) {
				return true
			}
		}
	}
	return false
}

// findings is the refused parts as a standing keeps them: bounded, in a fixed
// order, each part once, and how many more there were than it keeps.
func findings(unread []Unread) ([]Finding, int) {
	var out []Finding
	for _, u := range unread {
		if slices.ContainsFunc(out, func(f Finding) bool { return f.Part == u.Part }) {
			continue
		}
		out = append(out, Finding{Part: textcut.Within(u.Part, MaxFindingLength),
			Scope:  textcut.Within(u.Scope, MaxFindingLength),
			Detail: textcut.Within(u.Detail, MaxFindingLength)})
	}
	slices.SortFunc(out, func(a, b Finding) int { return cmp.Compare(a.Part, b.Part) })
	if len(out) > MaxUnread {
		return out[:MaxUnread], len(out) - MaxUnread
	}
	return out, 0
}

// scopes is each scope the read visited, in the order of their ids: read
// whole at now where the read was refused none of its parts, and otherwise
// as last read whole, with how many parts were refused. A scope the read no
// longer visits has no standing. Counted from the read rather than from the
// findings, which are bounded.
func scopes(prior []Scope, read Read, now time.Time) []Scope {
	last := map[string]time.Time{}
	for _, s := range prior {
		last[s.ID] = s.LastReadAt
	}
	refused := map[string]map[string]bool{}
	for _, u := range read.Unread {
		if u.Scope == "" {
			continue
		}
		if refused[u.Scope] == nil {
			refused[u.Scope] = map[string]bool{}
		}
		refused[u.Scope][u.Part] = true
	}
	ids := slices.Clone(read.Scopes)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > MaxScopes {
		ids = ids[:MaxScopes]
	}
	var out []Scope
	for _, id := range ids {
		s := Scope{ID: id, LastReadAt: last[id], Unread: len(refused[id])}
		if s.Unread == 0 {
			s.LastReadAt = now
		}
		out = append(out, s)
	}
	return out
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
