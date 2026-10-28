package code

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW MANY BYTES A ROW ENCODES TO, computed rather than encoded.
//
// A page is held to MaxPageBytes of encoded rows, and the first way of knowing
// a row's size was to json.Marshal it and throw the encoding away: 0.78–0.83
// µs and three allocations a ref, on every row of every snapshot page on the
// serving node — about an eighth of the 5.7–6.4 µs a page spends a ref — and
// then the answer is encoded again on the estate wire, or not at all when it
// is served in process. Computed, it is 0.08–0.09 µs and none
// (BenchmarkServeASnapshotPage, doc.go's measured section). A push row was
// worse: its commands are read as their stored JSON, decoded, and were
// encoded again only to be measured.
//
// So each row type sums its own encoding: a fixed overhead for its keys and
// punctuation, and each value's encoded length by encoding/json's rules — a
// string's escapes, a number's digits, a time's RFC 3339 spelling, an omitted
// field omitted. EXACT rather than a bound, because a bound that is too high
// cuts pages short and one too low lets a page past its budget; the
// equality with json.Marshal is what rowbytes_test.go holds, over every field
// of every type filled at random, so a field added to a row without its term
// here fails that test rather than a reply ceiling.

// stringBytes is the length of s encoded as a JSON string by the
// encoding/json this module builds with, quotes included: `"` and `\` and the
// five control characters with short escapes take two bytes; every other
// control character and `<`, `>` and `&` (escaped for HTML) take six, as do
// U+2028 and U+2029 (escaped for JavaScript); each byte that is not UTF-8 is
// written as U+FFFD itself, three; everything else is itself. Which of those
// an encoder escapes, and how, is ITS choice — the encoder behind
// json.Marshal writes a byte of invalid UTF-8 as three bytes where the one
// before it wrote six — so FuzzStringBytes holds this to the encoder rather
// than to a reading of it.
func stringBytes(s string) int {
	n := 2
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			n += int(asciiBytes[b])
			i++
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case c == utf8.RuneError && size == 1:
			n += utf8.RuneLen(utf8.RuneError)
		case c == ' ' || c == ' ':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}

// asciiBytes is what each ASCII byte encodes to, a table rather than a switch
// because a ref is almost all ASCII and this is the loop every row's size
// spends its time in.
var asciiBytes = func() (t [utf8.RuneSelf]uint8) {
	for b := range t {
		switch {
		case b == '"' || b == '\\' || b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
			t[b] = 2
		case b < 0x20 || b == '<' || b == '>' || b == '&':
			t[b] = 6
		default:
			t[b] = 1
		}
	}
	return t
}()

// intBytes is the length of n written in decimal.
func intBytes(n int64) int {
	var buf [20]byte
	return len(strconv.AppendInt(buf[:0], n, 10))
}

// uintBytes is the length of n written in decimal.
func uintBytes(n uint64) int {
	var buf [20]byte
	return len(strconv.AppendUint(buf[:0], n, 10))
}

// timeBytes is the length of t as encoding/json writes it: RFC 3339 with the
// nanoseconds' trailing zeros trimmed, quoted.
func timeBytes(t time.Time) int {
	var buf [64]byte
	return len(t.AppendFormat(buf[:0], time.RFC3339Nano)) + 2
}

// trueBytes is the length of a JSON true: every boolean a row carries is
// omitempty, so the one value one is ever written with.
const trueBytes = len("true")

// field is the length of one member `"key":` plus its value of length v,
// with the comma that separates it from the member before when it is not the
// first.
func field(key string, v int, first bool) int {
	n := len(key) + 3 + v
	if !first {
		n++
	}
	return n
}

// positionBytes is the length of a statelog.Position.
func positionBytes(p statelog.Position) int {
	return 2 + field("stream", stringBytes(p.Stream), true) +
		field("generation", uintBytes(uint64(p.Generation)), false) +
		field("seq", uintBytes(p.Seq), false)
}

// stringsBytes is the length of a list of strings.
func stringsBytes(ss []string) int {
	n := 2
	for i, s := range ss {
		if i > 0 {
			n++
		}
		n += stringBytes(s)
	}
	return n
}

// encodedBytes is the length of the ref's JSON encoding.
func (r RefRow) encodedBytes() int {
	n := 2 + field("ref", stringBytes(r.Ref), true)
	if r.OID != "" {
		n += field("oid", stringBytes(r.OID), false)
	}
	return n + field("position", positionBytes(r.Position), false)
}

// encodedBytes is the length of the principal's JSON encoding.
func (p Principal) encodedBytes() int {
	n := 2 + field("kind", stringBytes(string(p.Kind)), true)
	if p.OperatorID != "" {
		n += field("operator_id", stringBytes(p.OperatorID), false)
	}
	if p.Seat != "" {
		n += field("seat", stringBytes(p.Seat), false)
	}
	if !p.Run.IsZero() {
		n += field("run", p.Run.encodedBytes(), false)
	}
	return n
}

// encodedBytes is the length of the run's JSON encoding.
func (r RunProvenance) encodedBytes() int {
	n := 2 + field("turn_id", stringBytes(r.TurnID), true)
	if r.WorkBackend != "" {
		n += field("work_backend", stringBytes(r.WorkBackend), false)
	}
	if r.WorkKey != "" {
		n += field("work_key", stringBytes(r.WorkKey), false)
	}
	if r.Model != "" {
		n += field("model", stringBytes(r.Model), false)
	}
	return n
}

// encodedBytes is the length of the move's JSON encoding.
func (m RefMove) encodedBytes() int {
	n := 2 + field("ref", stringBytes(m.Ref), true) + field("old", stringBytes(m.Old), false) +
		field("new", stringBytes(m.New), false)
	if m.Forced {
		n += field("forced", trueBytes, false)
	}
	return n
}

// encodedBytes is the length of the introducing push's JSON encoding: its
// PushedBy's members flattened beside its own, as encoding/json writes an
// embedded struct.
func (row IntroducedRow) encodedBytes() int {
	n := 2 + field("push_id", stringBytes(row.PushID), true) +
		field("position", positionBytes(row.Position), false) +
		field("at", timeBytes(row.At), false) +
		field("principal", row.Principal.encodedBytes(), false) +
		field("via", stringBytes(string(row.Via)), false)
	if len(row.Moves) > 0 {
		list := 2 + len(row.Moves) - 1
		for _, m := range row.Moves {
			list += m.encodedBytes()
		}
		n += field("moves", list, false)
	}
	if len(row.Introduced) > 0 {
		n += field("introduced", stringsBytes(row.Introduced), false)
	}
	if row.Count != 0 {
		n += field("count", intBytes(int64(row.Count)), false)
	}
	return n
}

// encodedBytes is the length of the pack's JSON encoding. The object is a
// key's canonical spelling, thirty-six bytes, quoted.
func (r PackRow) encodedBytes() int {
	return 2 + field("pack_id", stringBytes(r.PackID), true) +
		field("part", stringBytes(r.Part), false) +
		field("object", len(r.Object.String())+2, false) +
		field("hash", stringBytes(string(r.Hash)), false) +
		field("size", intBytes(r.Size), false) +
		field("index_bytes", intBytes(r.IndexBytes), false) +
		field("reverse_bytes", intBytes(r.ReverseBytes), false) +
		field("git_checksum", stringBytes(r.GitChecksum), false) +
		field("objects", intBytes(r.Objects), false) +
		field("kind", stringBytes(string(r.Kind)), false) +
		field("state", stringBytes(string(r.State)), false) +
		field("added", positionBytes(r.Added), false) +
		field("content", positionBytes(r.Content), false)
}

// encodedBytes is the length of the candidate's JSON encoding.
func (c CompactionCandidate) encodedBytes() int {
	return 2 + field("id", stringBytes(c.ID), true) +
		field("name", stringBytes(c.Name), false) +
		field("live_packs", intBytes(c.LivePacks), false) +
		field("live_objects", intBytes(c.LiveObjects), false) +
		field("version", positionBytes(c.Version), false)
}

// encodedBytes is the length of the candidate's JSON encoding.
func (c PublicationCandidate) encodedBytes() int {
	n := 2 + field("id", stringBytes(c.ID), true) +
		field("name", stringBytes(c.Name), false) +
		field("live_packs", intBytes(c.LivePacks), false) +
		field("published_packs", intBytes(c.PublishedPacks), false) +
		field("live_objects", intBytes(c.LiveObjects), false) +
		field("published_objects", intBytes(c.PublishedObjects), false)
	if !c.LastPushAt.IsZero() {
		n += field("last_push_at", timeBytes(c.LastPushAt), false)
	}
	if !c.PublishedAt.IsZero() {
		n += field("published_at", timeBytes(c.PublishedAt), false)
	}
	return n + field("publish_took", intBytes(int64(c.PublishTook)), false) +
		field("version", positionBytes(c.Version), false)
}

// encodedBytes is the length of the candidate's JSON encoding.
func (c ExpungeCandidate) encodedBytes() int {
	n := 2 + field("id", stringBytes(c.ID), true) +
		field("name", stringBytes(c.Name), false)
	if c.Open != "" {
		n += field("open", stringBytes(c.Open), false)
	}
	if c.Request != "" {
		n += field("request", stringBytes(c.Request), false)
	}
	if !c.Requested.IsZero() {
		n += field("requested", positionBytes(c.Requested), false)
	}
	if !c.ExpungedAt.IsZero() {
		n += field("expunged_at", timeBytes(c.ExpungedAt), false)
	}
	return n + field("live_packs", intBytes(c.LivePacks), false) +
		field("live_bytes", intBytes(c.LiveBytes), false) +
		field("version", positionBytes(c.Version), false)
}

// encodedBytes is the length of the layer row's JSON encoding: its layer
// whole, its parts and packs among them.
func (r LayerRow) encodedBytes() int {
	return 2 + field("seq", intBytes(int64(r.Seq)), true) +
		field("layer", r.Layer.encodedBytes(), false) +
		field("added", positionBytes(r.Added), false)
}

// encodedBytes is the length of the layer's JSON encoding. Packs and Parts are
// never omitted, so a nil one is written null.
func (l LayerRef) encodedBytes() int {
	n := 2 + field("layer_id", stringBytes(l.LayerID), true)
	if l.BaseID != "" {
		n += field("base_id", stringBytes(l.BaseID), false)
	}
	if len(l.Replaces) > 0 {
		n += field("replaces", stringsBytes(l.Replaces), false)
	}
	if l.GraphID != "" {
		n += field("graph_id", stringBytes(l.GraphID), false)
	}
	packs := nullBytes
	if l.Packs != nil {
		packs = stringsBytes(l.Packs)
	}
	parts := nullBytes
	if l.Parts != nil {
		parts = 2 + max(len(l.Parts)-1, 0)
		for _, p := range l.Parts {
			parts += p.encodedBytes()
		}
	}
	return n + field("packs", packs, false) + field("parts", parts, false) +
		field("midx_bytes", intBytes(l.MidxBytes), false) +
		field("bitmap_bytes", intBytes(l.BitmapBytes), false) +
		field("graph_bytes", intBytes(l.GraphBytes), false) +
		field("objects", intBytes(l.Objects), false) +
		field("commits", intBytes(l.Commits), false) +
		field("git", stringBytes(l.Git), false) +
		field("took_ms", intBytes(l.TookMs), false)
}

// encodedBytes is the length of the part's JSON encoding. The object is a
// key's canonical spelling, thirty-six bytes, quoted.
func (p LayerPart) encodedBytes() int {
	return 2 + field("object", len(p.Object.String())+2, true) +
		field("hash", stringBytes(string(p.Hash)), false) +
		field("size", intBytes(p.Size), false)
}

// nullBytes is the length of a JSON null: what a nil slice no omitempty
// leaves out is written as.
const nullBytes = len("null")

// encodedBytes is the length of the retirement's JSON encoding.
func (r RetiredRow) encodedBytes() int {
	return 2 + field("pack_id", stringBytes(r.PackID), true) +
		field("part", stringBytes(r.Part), false) +
		field("git_checksum", stringBytes(r.GitChecksum), false) +
		field("retired_at", timeBytes(r.RetiredAt), false) +
		field("position", positionBytes(r.Position), false)
}

// encodedBytes is the length of the protection's JSON encoding.
func (p Protection) encodedBytes() int {
	n := 2 + field("pattern", stringBytes(p.Pattern), true)
	if p.AllowForce {
		n += field("allow_force", trueBytes, false)
	}
	if p.AllowDelete {
		n += field("allow_delete", trueBytes, false)
	}
	return n + field("direct_push", stringBytes(string(p.DirectPush)), false)
}

// members is the length of the repository's own members, without the braces
// that close them — what a RepoSummary flattens beside its own.
func (r RepoRef) members() int {
	n := field("id", stringBytes(r.ID), true) +
		field("name", stringBytes(r.Name), false) +
		field("status", stringBytes(string(r.Status)), false) +
		field("managed_by", stringBytes(string(r.ManagedBy)), false) +
		field("owner", 2+field("kind", stringBytes(string(r.Owner.Kind)), true)+
			field("ref", stringBytes(r.Owner.Ref), false), false) +
		field("visibility", stringBytes(string(r.Visibility)), false) +
		field("default_branch", stringBytes(r.DefaultBranch), false)
	if r.Description != "" {
		n += field("description", stringBytes(r.Description), false)
	}
	if len(r.Protections) > 0 {
		list := 2 + len(r.Protections) - 1
		for _, p := range r.Protections {
			list += p.encodedBytes()
		}
		n += field("protections", list, false)
	}
	if !r.DeletedAt.IsZero() {
		n += field("deleted_at", timeBytes(r.DeletedAt), false)
	}
	n += field("refs", intBytes(r.Refs), false) +
		field("live_packs", intBytes(r.LivePacks), false) +
		field("live_bytes", intBytes(r.LiveBytes), false) +
		field("live_objects", intBytes(r.LiveObjects), false)
	if r.DropEpoch != 0 {
		n += field("drop_epoch", intBytes(r.DropEpoch), false)
	}
	return n + field("position", positionBytes(r.Position), false)
}

// encodedBytes is the length of the listing row's JSON encoding.
func (s RepoSummary) encodedBytes() int {
	n := 2 + s.RepoRef.members()
	if s.Access != "" {
		n += field("access", stringBytes(s.Access), false)
	}
	if s.Head != "" {
		n += field("head", stringBytes(s.Head), false)
	}
	n += field("pushes", intBytes(s.Pushes), false)
	if !s.LastPushAt.IsZero() {
		n += field("last_push_at", timeBytes(s.LastPushAt), false)
	}
	return n
}

// encodedBytes is the length of the command's JSON encoding.
func (c Command) encodedBytes() int {
	n := 2 + field("ref", stringBytes(c.Ref), true) + field("old", stringBytes(c.Old), false) +
		field("new", stringBytes(c.New), false)
	if c.Forced {
		n += field("forced", trueBytes, false)
	}
	n += field("result", stringBytes(string(c.Result)), false)
	if c.Reason != "" {
		n += field("reason", stringBytes(string(c.Reason)), false)
	}
	if c.Detail != "" {
		n += field("detail", stringBytes(c.Detail), false)
	}
	return n
}

// commandsBytes is the length of the row's commands encoded as the list they
// are, for a row whose commands are not the column they were read from: a
// row read in brief, whose list may be cut.
func (row PushRow) commandsBytes() int {
	if row.Commands == nil {
		return len("null")
	}
	n := 2
	for i, c := range row.Commands {
		if i > 0 {
			n++
		}
		n += c.encodedBytes()
	}
	return n
}

// encodedBytes is the length of the counts' JSON encoding: every count is
// omitted at zero.
func (n CommandCounts) encodedBytes() int {
	size, first := 2, true
	for _, c := range []struct {
		key   string
		count int
	}{
		{"created", n.Created}, {"moved", n.Moved}, {"forced", n.Forced}, {"deleted", n.Deleted},
		{"unchanged", n.Unchanged}, {"refused", n.Refused},
	} {
		if c.count == 0 {
			continue
		}
		size += field(c.key, intBytes(int64(c.count)), first)
		first = false
	}
	return size
}

// encodedBytes is the length of the push row's JSON encoding, given the
// length of its commands' encoding — which a row read from the store has
// already, as the column it was decoded from: the stored commands ARE
// encoding/json's encoding of the same list, so it is never encoded again
// to be measured.
func (row PushRow) encodedBytes(commands int) int {
	n := 2 + field("push_id", stringBytes(row.PushID), true) +
		field("repo_id", stringBytes(row.RepoID), false) +
		field("position", positionBytes(row.Position), false) +
		field("at", timeBytes(row.At), false) +
		field("actor", stringBytes(row.Actor), false) +
		field("actor_kind", stringBytes(row.ActorKind), false)
	if row.ActorSeat != "" {
		n += field("actor_seat", stringBytes(row.ActorSeat), false)
	}
	n += field("via", stringBytes(string(row.Via)), false)
	if !row.Run.IsZero() {
		n += field("run", row.Run.encodedBytes(), false)
	}
	if row.Node != "" {
		n += field("node", stringBytes(row.Node), false)
	}
	if row.Atomic {
		n += field("atomic", trueBytes, false)
	}
	n += field("commands", commands, false)
	if row.Omitted != (CommandCounts{}) {
		n += field("omitted", row.Omitted.encodedBytes(), false)
	}
	if row.PackID != "" {
		n += field("pack_id", stringBytes(row.PackID), false)
	}
	if row.PackBytes != 0 {
		n += field("pack_bytes", intBytes(row.PackBytes), false)
	}
	if len(row.Introduced) > 0 {
		n += field("introduced", stringsBytes(row.Introduced), false)
	}
	if row.IntroducedCount != 0 {
		n += field("introduced_count", intBytes(int64(row.IntroducedCount)), false)
	}
	return n + field("received_at", timeBytes(row.ReceivedAt), false)
}
