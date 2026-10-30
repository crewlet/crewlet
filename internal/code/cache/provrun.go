package cache

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PROVENANCE RUN: one immutable file of the index of which push introduced
// each commit (provenance.go), SELF-CONTAINED — every push it names, every
// ref those pushes moved, every move and every introduced commit, so a lookup
// reads one run and nothing beside it — and COMPLETE OVER ITS RANGE: every
// push of the repository with a position in (from, through] is in it. Runs
// never overlap, so no push is in two of them and no lookup weighs duplicates.
// Big-endian throughout:
//
//	header   "CRPV" | version u8 (1) | oid bytes u8 (20) | reserved u16
//	         pushes u32 | refs u32 | commits u64 | moves u64 | push bytes u64 | name bytes u64 |
//	         from i64 | through i64                                     (64 bytes)
//	offsets  pushes × u64          — each push record's offset in the push section
//	pushes   push bytes            — the records back to back, in (position, push id) order:
//	         push id [16] | position i64 | at unix-µs i64 | via u8 | principal kind u8 |
//	         fields (tag u8 | uvarint length | bytes), each only when it is not empty:
//	         1 operator id, 2 seat, and a seat's run — 3 turn id, 4 work backend,
//	         5 work key, 6 model, and a coding box's 7 run id, 8 coding agent;
//	         an unknown tag skipped
//	refs     refs × (ref key [8] | name offset u32 | name length u16 | ordinal u8 | reserved u8)
//	                                                          sorted by (ref key, ordinal)
//	names    name bytes            — the refs' names back to back
//	moves    moves × (ref key [8] | ordinal u8 | position i64 | push index u32 | old [20] |
//	         new [20] | flags u8 (1 forced, 2 a deletion))
//	                                                          sorted by (ref key, ordinal, position)
//	commits  commits × (oid [20] | push index u32)            sorted by (oid, push index)
//	trailer  SHA-256 of every byte before it [32]
//
// A REF IS NAMED BY ITS KEY AND ITS ORDINAL. The key is the first eight bytes
// of the SHA-256 of the ref's name: a fixed width, so the moves section sorts
// and seeks by it. Two refs of one run sharing a key — about one chance in a
// billion at code.MaxRefsPerRepo — are two ordinals of it, each with its name
// in the refs section, so a lookup finds its ref's ordinal by NAME and the
// moves of each ref stay contiguous: a collision costs nothing, and never a
// wrong answer. A run that does not hold a ref's name holds none of its moves,
// whatever key another ref of it has.
//
// MOVES OF ONE REF AT ONE POSITION ARE IN THE ORDER THEIR PUSHES LANDED them.
// Group commit lands up to code.MaxPushesPerRecord pushes in one record, each
// decided against the ones before it (code's decide), so two moves of one ref
// in one record chain — the second's old is the first's new — and the chain
// is their order. The push id is no such order: the rows keep none within a
// record.
//
// Every record has a fixed width but a push's, whose start the offsets table
// gives, so a lookup is a binary search over memory the page cache backs once
// the run is mapped (provenance.go), never a read call a probe.
//
// A PUSH'S FIELDS ARE TAGGED, so a field a later build adds — a seat's run
// gained four, and a coding box's two more — moves no version: a reader skips
// a tag it does not know, a run this build writes is read by an older one as
// the push less what it cannot name, and the index is a cache any build
// rebuilds from the rows.

// The format's fixed parts.
const (
	runMagic        = "CRPV"
	runVersion      = 1
	runOIDBytes     = 20
	runHeaderBytes  = 64
	runOffsetBytes  = 8
	runRefBytes     = 16
	runMoveBytes    = 62
	runCommitBytes  = 24
	runKeyBytes     = 8
	runTrailerBytes = sha256.Size
	// runPushFixed is a push record's fixed part: its id, position, instant,
	// way and principal kind.
	runPushFixed = 16 + 8 + 8 + 1 + 1
)

// A move's flags.
const (
	moveForced  = 1 << 0
	moveDeleted = 1 << 1
)

// The push record's fields after its fixed part: the principal's credential
// and seat, and a seat's run (code.RunProvenance) — a coding box's naming the
// run that pushed and its coding agent beside the turn.
const (
	fieldOperatorID  = 1
	fieldSeat        = 2
	fieldTurnID      = 3
	fieldWorkBackend = 4
	fieldWorkKey     = 5
	fieldModel       = 6
	fieldRunID       = 7
	fieldCodingAgent = 8
)

// The codes a push record names its way and its principal's kind by: one
// table each, so a kind or a way a later build adds is a code here — and
// TestEveryPushPrincipalAndWayHasARunCode holds every value of code's closed
// sets to one.
var (
	runViaCodes  = map[code.PushVia]uint8{code.ViaGit: 1, code.ViaEngine: 2, code.ViaMerge: 3}
	runKindCodes = map[code.PrincipalKind]uint8{code.PrincipalOperator: 1, code.PrincipalAgent: 2}
)

// errRunMalformed is a run whose bytes are not the format's: a file this
// build did not write, or one damaged past its trailer's reach.
var errRunMalformed = errors.New("cache: a provenance run that is not one this build writes")

// runHeader is a run's header.
type runHeader struct {
	pushes, refs                         uint32
	commits, moves, pushBytes, nameBytes uint64
	from, through                        int64
}

// runLayout is where each section of a run with header h begins, and how long
// the whole file is.
type runLayout struct {
	offsets, pushes, refs, names, moves, commits, trailer, size int64
}

// layout is h's sections, or false where they overflow what a file can be.
func (h runHeader) layout() (runLayout, bool) {
	const most = 1 << 62
	if h.commits > most/runCommitBytes || h.moves > most/runMoveBytes || h.pushBytes > most ||
		h.nameBytes > most {
		return runLayout{}, false
	}
	var l runLayout
	l.offsets = runHeaderBytes
	l.pushes = l.offsets + int64(h.pushes)*runOffsetBytes
	l.refs = l.pushes + int64(h.pushBytes)
	l.names = l.refs + int64(h.refs)*runRefBytes
	l.moves = l.names + int64(h.nameBytes)
	l.commits = l.moves + int64(h.moves)*runMoveBytes
	l.trailer = l.commits + int64(h.commits)*runCommitBytes
	l.size = l.trailer + runTrailerBytes
	return l, l.size > 0
}

// encode is h's 64 bytes.
func (h runHeader) encode() []byte {
	b := make([]byte, runHeaderBytes)
	copy(b, runMagic)
	b[4], b[5] = runVersion, runOIDBytes
	binary.BigEndian.PutUint32(b[8:], h.pushes)
	binary.BigEndian.PutUint32(b[12:], h.refs)
	binary.BigEndian.PutUint64(b[16:], h.commits)
	binary.BigEndian.PutUint64(b[24:], h.moves)
	binary.BigEndian.PutUint64(b[32:], h.pushBytes)
	binary.BigEndian.PutUint64(b[40:], h.nameBytes)
	binary.BigEndian.PutUint64(b[48:], uint64(h.from))
	binary.BigEndian.PutUint64(b[56:], uint64(h.through))
	return b
}

// parseRunHeader reads the header of data, a whole run, and holds its
// sections to data's length.
func parseRunHeader(data []byte) (runHeader, runLayout, error) {
	if len(data) < runHeaderBytes+runTrailerBytes || string(data[:4]) != runMagic {
		return runHeader{}, runLayout{}, fmt.Errorf("%w: no CRPV header", errRunMalformed)
	}
	if data[4] != runVersion || data[5] != runOIDBytes {
		return runHeader{}, runLayout{}, fmt.Errorf("%w: version %d of %d-byte ids, and this build writes version "+
			"%d of %d", errRunMalformed, data[4], data[5], runVersion, runOIDBytes)
	}
	h := runHeader{
		pushes: binary.BigEndian.Uint32(data[8:]), refs: binary.BigEndian.Uint32(data[12:]),
		commits: binary.BigEndian.Uint64(data[16:]), moves: binary.BigEndian.Uint64(data[24:]),
		pushBytes: binary.BigEndian.Uint64(data[32:]), nameBytes: binary.BigEndian.Uint64(data[40:]),
		from: int64(binary.BigEndian.Uint64(data[48:])), through: int64(binary.BigEndian.Uint64(data[56:])),
	}
	l, ok := h.layout()
	if !ok || l.size != int64(len(data)) || h.from > h.through {
		return runHeader{}, runLayout{}, fmt.Errorf("%w: its sections do not add up to its %d bytes",
			errRunMalformed, len(data))
	}
	return h, l, nil
}

// refKey is the key a run files ref under: the first eight bytes of the
// SHA-256 of its name.
func refKey(ref string) [runKeyBytes]byte {
	sum := sha256.Sum256([]byte(ref))
	var k [runKeyBytes]byte
	copy(k[:], sum[:])
	return k
}

// oidBytes is a whole object id's twenty bytes (the zero id's for a create's
// old and a deletion's new), or false for anything that is not one.
func oidBytes(oid string) ([runOIDBytes]byte, bool) {
	var b [runOIDBytes]byte
	if len(oid) != 2*runOIDBytes {
		return b, false
	}
	if _, err := hex.Decode(b[:], []byte(oid)); err != nil || strings.ToLower(oid) != oid {
		return b, false
	}
	return b, true
}

// runRows is what one run holds, built from push rows (encodeRun) and written
// whole.
type runRows struct {
	from, through int64
	payloads      [][]byte
	refs          []builtRef
	names         []byte
	moves         []builtMove
	commits       []builtCommit
}

type builtRef struct {
	key     [runKeyBytes]byte
	name    string
	ordinal uint8
	off     uint32
}

type builtMove struct {
	key      [runKeyBytes]byte
	ordinal  uint8
	position int64
	push     uint32
	old, new [runOIDBytes]byte
	flags    uint8
	// chain is the move's place among the moves of its ref at its position,
	// in the order their pushes landed them.
	chain int
}

type builtCommit struct {
	oid  [runOIDBytes]byte
	push uint32
}

// buildRun is the run of rows over (from, through]: rows in (position, push
// id) order, each a push of the repository at a position in that range, and
// every push of it there — what a delta's pushes phase or a page of the
// backfill answered, held to that already (validPush). An error is a row
// this build cannot write: a push id that is no UUID, a principal kind or a
// way it has no code for, or a ref name past what a run can hold.
func buildRun(from, through int64, rows []code.IntroducedRow) (runRows, error) {
	return buildRunKeyed(from, through, rows, refKey)
}

// buildRunKeyed is buildRun filing every ref under the key keyOf answers for
// its name: refKey, but for the suite's case of two refs sharing a key, which
// one chance in a billion would otherwise have to arrange.
func buildRunKeyed(from, through int64, rows []code.IntroducedRow, keyOf func(string) [runKeyBytes]byte) (
	runRows, error) {

	r := runRows{from: from, through: through}
	type named struct {
		key  [runKeyBytes]byte
		name string
	}
	byName := map[string]named{}
	for i, row := range rows {
		payload, err := pushPayload(row)
		if err != nil {
			return runRows{}, err
		}
		r.payloads = append(r.payloads, payload)
		for _, m := range row.Moves {
			if _, ok := byName[m.Ref]; !ok {
				byName[m.Ref] = named{key: keyOf(m.Ref), name: m.Ref}
			}
			old, okOld := oidBytes(m.Old)
			nw, okNew := oidBytes(m.New)
			if !okOld || !okNew {
				return runRows{}, fmt.Errorf("cache: push %s moved %s from %q to %q, which are not object ids",
					row.PushID, m.Ref, m.Old, m.New)
			}
			var flags uint8
			if m.Forced {
				flags |= moveForced
			}
			if m.New == code.ZeroOID {
				flags |= moveDeleted
			}
			r.moves = append(r.moves, builtMove{key: byName[m.Ref].key, position: row.Position.Packed(),
				push: uint32(i), old: old, new: nw, flags: flags})
		}
		for _, oid := range row.Introduced {
			b, ok := oidBytes(oid)
			if !ok {
				return runRows{}, fmt.Errorf("cache: push %s introduced %q, which is not an object id", row.PushID, oid)
			}
			r.commits = append(r.commits, builtCommit{oid: b, push: uint32(i)})
		}
	}
	// THE REFS, each its key's ordinal by name.
	names := make([]named, 0, len(byName))
	for _, n := range byName {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b named) int {
		return cmp.Or(bytes.Compare(a.key[:], b.key[:]), strings.Compare(a.name, b.name))
	})
	ordinals := map[string]uint8{}
	for i, n := range names {
		var ord int
		if i > 0 && names[i-1].key == n.key {
			ord = int(ordinals[names[i-1].name]) + 1
		}
		if ord > 255 {
			return runRows{}, fmt.Errorf("cache: %d refs of one run share the key of %s, past the 256 a run "+
				"holds", ord+1, n.name)
		}
		ordinals[n.name] = uint8(ord)
		r.refs = append(r.refs, builtRef{key: n.key, name: n.name, ordinal: uint8(ord), off: uint32(len(r.names))})
		r.names = append(r.names, n.name...)
	}
	// THE MOVES, each ref's in the order its pushes landed them.
	moveRef := 0
	for _, row := range rows {
		for _, m := range row.Moves {
			r.moves[moveRef].ordinal = ordinals[m.Ref]
			moveRef++
		}
	}
	chainMoves(r.moves)
	slices.SortFunc(r.moves, func(a, b builtMove) int {
		return cmp.Or(bytes.Compare(a.key[:], b.key[:]), cmp.Compare(a.ordinal, b.ordinal),
			cmp.Compare(a.position, b.position), cmp.Compare(a.chain, b.chain))
	})
	slices.SortFunc(r.commits, func(a, b builtCommit) int {
		return cmp.Or(bytes.Compare(a.oid[:], b.oid[:]), cmp.Compare(a.push, b.push))
	})
	return r, nil
}

// chainMoves gives every move its place among the moves of its ref at its
// position (builtMove.chain): the order their pushes landed them, which the
// moves' own values spell — each one after the first starts where the one
// before it ended. A group of one is its own chain; a group whose moves do
// not chain — which no decide lands — keeps its pushes' order.
func chainMoves(moves []builtMove) {
	type group struct {
		key      [runKeyBytes]byte
		ordinal  uint8
		position int64
	}
	at := map[group][]int{}
	for i, m := range moves {
		g := group{m.key, m.ordinal, m.position}
		at[g] = append(at[g], i)
	}
	for _, idx := range at {
		if len(idx) == 1 {
			continue
		}
		ends := map[[runOIDBytes]byte]bool{}
		for _, i := range idx {
			ends[moves[i].new] = true
		}
		var first = -1
		for _, i := range idx {
			if !ends[moves[i].old] {
				first = i
				break
			}
		}
		placed := map[int]bool{}
		for n, cur := 0, first; cur >= 0 && n < len(idx); n++ {
			moves[cur].chain, placed[cur] = n, true
			next := -1
			for _, i := range idx {
				if !placed[i] && moves[i].old == moves[cur].new {
					next = i
					break
				}
			}
			cur = next
		}
		if len(placed) != len(idx) {
			// No chain: the pushes' own order, which is all that is left.
			for n, i := range idx {
				moves[i].chain = n
			}
		}
	}
}

// pushPayload is row's push record.
func pushPayload(row code.IntroducedRow) ([]byte, error) {
	id, err := uuid.Parse(row.PushID)
	if err != nil || id.String() != row.PushID {
		return nil, fmt.Errorf("cache: push id %q is not a canonical UUID", row.PushID)
	}
	via, ok := runViaCodes[row.Via]
	if !ok {
		return nil, fmt.Errorf("cache: push %s was built %q, which a run has no code for", row.PushID, row.Via)
	}
	kind, ok := runKindCodes[row.Principal.Kind]
	if !ok {
		return nil, fmt.Errorf("cache: push %s was made by a principal of kind %q, which a run has no code for",
			row.PushID, row.Principal.Kind)
	}
	run := row.Principal.Run
	fields := []struct {
		tag   byte
		value string
	}{
		{fieldOperatorID, row.Principal.OperatorID}, {fieldSeat, row.Principal.Seat},
		{fieldTurnID, run.TurnID}, {fieldWorkBackend, run.WorkBackend}, {fieldWorkKey, run.WorkKey},
		{fieldModel, run.Model}, {fieldRunID, run.RunID}, {fieldCodingAgent, run.CodingAgent},
	}
	size := runPushFixed
	for _, f := range fields {
		if f.value != "" {
			size += 1 + binary.MaxVarintLen64 + len(f.value)
		}
	}
	b := make([]byte, runPushFixed, size)
	copy(b, id[:])
	binary.BigEndian.PutUint64(b[16:], uint64(row.Position.Packed()))
	binary.BigEndian.PutUint64(b[24:], uint64(row.At.UnixMicro()))
	b[32], b[33] = via, kind
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		b = append(b, f.tag)
		b = binary.AppendUvarint(b, uint64(len(f.value)))
		b = append(b, f.value...)
	}
	return b, nil
}

// decodePush is the PushedBy a push record holds.
func decodePush(payload []byte) (code.PushedBy, error) {
	if len(payload) < runPushFixed {
		return code.PushedBy{}, fmt.Errorf("%w: a push record of %d bytes", errRunMalformed, len(payload))
	}
	id, err := uuid.FromBytes(payload[:16])
	if err != nil {
		return code.PushedBy{}, fmt.Errorf("%w: %w", errRunMalformed, err)
	}
	p := code.PushedBy{
		PushID:   id.String(),
		Position: statelog.Unpack(topics.CodeLogStream, int64(binary.BigEndian.Uint64(payload[16:]))),
		At:       time.UnixMicro(int64(binary.BigEndian.Uint64(payload[24:]))).UTC(),
	}
	for v, c := range runViaCodes {
		if c == payload[32] {
			p.Via = v
		}
	}
	for k, c := range runKindCodes {
		if c == payload[33] {
			p.Principal.Kind = k
		}
	}
	if p.Via == "" || p.Principal.Kind == "" {
		return code.PushedBy{}, fmt.Errorf("%w: a push record coded %d and %d", errRunMalformed, payload[32],
			payload[33])
	}
	rest := payload[runPushFixed:]
	for len(rest) > 0 {
		tag := rest[0]
		n, w := binary.Uvarint(rest[1:])
		if w <= 0 || uint64(len(rest)-1-w) < n {
			return code.PushedBy{}, fmt.Errorf("%w: a push record's field runs past it", errRunMalformed)
		}
		value := string(rest[1+w : 1+w+int(n)])
		switch tag {
		case fieldOperatorID:
			p.Principal.OperatorID = value
		case fieldSeat:
			p.Principal.Seat = value
		case fieldTurnID:
			p.Principal.Run.TurnID = value
		case fieldWorkBackend:
			p.Principal.Run.WorkBackend = value
		case fieldWorkKey:
			p.Principal.Run.WorkKey = value
		case fieldModel:
			p.Principal.Run.Model = value
		case fieldRunID:
			p.Principal.Run.RunID = value
		case fieldCodingAgent:
			p.Principal.Run.CodingAgent = value
		}
		rest = rest[1+w+int(n):]
	}
	return p, nil
}

// header is r's header.
func (r runRows) header() runHeader {
	var pushBytes uint64
	for _, p := range r.payloads {
		pushBytes += uint64(len(p))
	}
	return runHeader{pushes: uint32(len(r.payloads)), refs: uint32(len(r.refs)), commits: uint64(len(r.commits)),
		moves: uint64(len(r.moves)), pushBytes: pushBytes, nameBytes: uint64(len(r.names)), from: r.from,
		through: r.through}
}

// write writes r to w as one run, its trailer the SHA-256 of what came
// before, and answers its header.
func (r runRows) write(w io.Writer) (runHeader, error) {
	h := r.header()
	out := newRunWriter(w)
	out.put(h.encode())
	var off uint64
	for _, p := range r.payloads {
		out.u64(off)
		off += uint64(len(p))
	}
	for _, p := range r.payloads {
		out.put(p)
	}
	for _, ref := range r.refs {
		out.ref(ref.key, ref.off, uint16(len(ref.name)), ref.ordinal)
	}
	out.put(r.names)
	for _, m := range r.moves {
		out.move(m.key, m.ordinal, m.position, m.push, m.old, m.new, m.flags)
	}
	for _, c := range r.commits {
		out.commit(c.oid, c.push)
	}
	return h, out.close()
}

// runWriter is a run streamed to dst: buffered, hashed as it goes, its trailer
// the hash.
type runWriter struct {
	dst     io.Writer
	w       *bufio.Writer
	sum     hash.Hash
	scratch [runMoveBytes]byte
	err     error
}

func newRunWriter(dst io.Writer) *runWriter {
	h := sha256.New()
	return &runWriter{dst: dst, w: bufio.NewWriterSize(io.MultiWriter(dst, h), copyBuffer), sum: h}
}

func (o *runWriter) put(b []byte) {
	if o.err == nil {
		_, o.err = o.w.Write(b)
	}
}

func (o *runWriter) u64(v uint64) {
	binary.BigEndian.PutUint64(o.scratch[:8], v)
	o.put(o.scratch[:8])
}

func (o *runWriter) ref(key [runKeyBytes]byte, off uint32, n uint16, ordinal uint8) {
	b := o.scratch[:runRefBytes]
	copy(b, key[:])
	binary.BigEndian.PutUint32(b[8:], off)
	binary.BigEndian.PutUint16(b[12:], n)
	b[14], b[15] = ordinal, 0
	o.put(b)
}

func (o *runWriter) move(key [runKeyBytes]byte, ordinal uint8, position int64, push uint32,
	old, nw [runOIDBytes]byte, flags uint8) {
	b := o.scratch[:runMoveBytes]
	copy(b, key[:])
	b[8] = ordinal
	binary.BigEndian.PutUint64(b[9:], uint64(position))
	binary.BigEndian.PutUint32(b[17:], push)
	copy(b[21:], old[:])
	copy(b[41:], nw[:])
	b[61] = flags
	o.put(b)
}

func (o *runWriter) commit(oid [runOIDBytes]byte, push uint32) {
	b := o.scratch[:runCommitBytes]
	copy(b, oid[:])
	binary.BigEndian.PutUint32(b[20:], push)
	o.put(b)
}

// close flushes what is buffered and writes the trailer — straight to the
// destination, past the hash, since a trailer is no part of what it sums.
func (o *runWriter) close() error {
	if o.err != nil {
		return o.err
	}
	if err := o.w.Flush(); err != nil {
		return err
	}
	_, err := o.dst.Write(o.sum.Sum(nil))
	return err
}

// runFile is a run's bytes as a lookup reads them — a mapped file
// (provenance.go) or, in a case, bytes in memory — with its header and
// sections, and the key a ref's name is filed under (refKey).
type runFile struct {
	data  []byte
	h     runHeader
	l     runLayout
	keyOf func(string) [runKeyBytes]byte
}

// openRunBytes is data, a whole run, as a lookup reads it; its trailer is the
// caller's to have checked (CheckRunTrailer) — this holds its sections to its
// length, so no lookup reads outside it.
func openRunBytes(data []byte) (runFile, error) {
	h, l, err := parseRunHeader(data)
	if err != nil {
		return runFile{}, err
	}
	f := runFile{data: data, h: h, l: l, keyOf: refKey}
	// The offsets table rises within the push section, every record holding
	// at least its fixed part: what pushPayload reads by.
	prev := uint64(0)
	for i := range int64(h.pushes) {
		off := binary.BigEndian.Uint64(data[l.offsets+i*runOffsetBytes:])
		if off < prev || off > h.pushBytes || (i > 0 && off-prev < runPushFixed) {
			return runFile{}, fmt.Errorf("%w: its push offsets do not rise within its push section", errRunMalformed)
		}
		prev = off
	}
	if h.pushes > 0 && h.pushBytes-prev < runPushFixed {
		return runFile{}, fmt.Errorf("%w: its last push record is cut short", errRunMalformed)
	}
	for i := range int64(h.refs) {
		e := data[l.refs+i*runRefBytes:]
		off, n := uint64(binary.BigEndian.Uint32(e[8:])), uint64(binary.BigEndian.Uint16(e[12:]))
		if off+n > h.nameBytes {
			return runFile{}, fmt.Errorf("%w: a ref's name runs past its names", errRunMalformed)
		}
	}
	return f, nil
}

// pushCount is how many pushes f holds.
func (f runFile) pushCount() int { return int(f.h.pushes) }

// push is push record i.
func (f runFile) push(i int) []byte {
	start := binary.BigEndian.Uint64(f.data[f.l.offsets+int64(i)*runOffsetBytes:])
	end := f.h.pushBytes
	if i+1 < int(f.h.pushes) {
		end = binary.BigEndian.Uint64(f.data[f.l.offsets+int64(i+1)*runOffsetBytes:])
	}
	return f.data[f.l.pushes+int64(start) : f.l.pushes+int64(end)]
}

// pushPosition is push i's position, packed.
func (f runFile) pushPosition(i int) int64 { return int64(binary.BigEndian.Uint64(f.push(i)[16:])) }

// firstAbove is the index of the first push at a position above floor: every
// push before it is at or below it.
func (f runFile) firstAbove(floor int64) int {
	n := f.pushCount()
	i, j := 0, n
	for i < j {
		h := int(uint(i+j) >> 1)
		if f.pushPosition(h) <= floor {
			i = h + 1
		} else {
			j = h
		}
	}
	return i
}

// refRec is ref entry i's bytes: its key, its name's offset and length, and
// its ordinal.
func (f runFile) refRec(i int) []byte {
	return f.data[f.l.refs+int64(i)*runRefBytes : f.l.refs+int64(i+1)*runRefBytes]
}

// refName is ref entry i's name, as the run's bytes hold it.
func (f runFile) refName(i int) []byte {
	e := f.refRec(i)
	off, n := int64(binary.BigEndian.Uint32(e[8:])), int64(binary.BigEndian.Uint16(e[12:]))
	return f.data[f.l.names+off : f.l.names+off+n]
}

// refOrdinal is ref's key and its ordinal in f, or false where f holds no
// move of ref: a binary search of the refs by key, reading their bytes in
// place.
func (f runFile) refOrdinal(ref string) ([runKeyBytes]byte, uint8, bool) {
	key := f.keyOf(ref)
	n := int(f.h.refs)
	i, j := 0, n
	for i < j {
		h := int(uint(i+j) >> 1)
		if bytes.Compare(f.refRec(h)[:runKeyBytes], key[:]) < 0 {
			i = h + 1
		} else {
			j = h
		}
	}
	for ; i < n; i++ {
		e := f.refRec(i)
		if !bytes.Equal(e[:runKeyBytes], key[:]) {
			break
		}
		if string(f.refName(i)) == ref {
			return key, e[14], true
		}
	}
	return key, 0, false
}

// moveRec is move record i's bytes.
func (f runFile) moveRec(i int) []byte {
	return f.data[f.l.moves+int64(i)*runMoveBytes : f.l.moves+int64(i+1)*runMoveBytes]
}

// commitRec is commit record i's bytes.
func (f runFile) commitRec(i int) []byte {
	return f.data[f.l.commits+int64(i)*runCommitBytes : f.l.commits+int64(i+1)*runCommitBytes]
}

// moveSpan is the moves of the ref filed under key and ordinal with a
// position in (above, through], as [lo, hi) of f's moves section.
func (f runFile) moveSpan(key [runKeyBytes]byte, ordinal uint8, above, through int64) (int, int) {
	n := int(f.h.moves)
	// The first record not below (key, ordinal, above+1).
	search := func(position int64) int {
		i, j := 0, n
		for i < j {
			h := int(uint(i+j) >> 1)
			rec := f.moveRec(h)
			c := cmp.Or(bytes.Compare(rec[:8], key[:]), cmp.Compare(rec[8], ordinal),
				cmp.Compare(int64(binary.BigEndian.Uint64(rec[9:])), position))
			if c < 0 {
				i = h + 1
			} else {
				j = h
			}
		}
		return i
	}
	lo := search(above + 1)
	hi := lo
	if through >= above+1 {
		hi = search(through + 1)
	}
	return lo, hi
}

// commitPush is the index of the push that introduced oid in f — the newest
// of the run's pushes that list it, a commit a later push brought back after
// every ref that reached it was deleted being listed by both — at a position
// above floor, or false.
func (f runFile) commitPush(oid [runOIDBytes]byte, floor int64) (int, bool) {
	n := int(f.h.commits)
	rec := func(i int) []byte {
		return f.data[f.l.commits+int64(i)*runCommitBytes : f.l.commits+int64(i+1)*runCommitBytes]
	}
	i, j := 0, n
	for i < j {
		h := int(uint(i+j) >> 1)
		if bytes.Compare(rec(h)[:runOIDBytes], oid[:]) < 0 {
			i = h + 1
		} else {
			j = h
		}
	}
	best := -1
	for ; i < n && bytes.Equal(rec(i)[:runOIDBytes], oid[:]); i++ {
		p := int(binary.BigEndian.Uint32(rec(i)[runOIDBytes:]))
		if p < int(f.h.pushes) && f.pushPosition(p) > floor {
			best = p
		}
	}
	return best, best >= 0
}

// mergeRuns writes to w the run of ins — adjacent runs in position order, each
// one's range below the next's, nothing between them but positions with no
// push — without the pushes at or below floor (the index's horizon) and what
// they moved and introduced, and answers its header.
//
// STREAMED, holding nothing a push or a ref: the inputs' pushes are each in
// (position, push id) order over disjoint ranges, so the merged run's are
// theirs one after another and a push's new index is its old one moved by a
// constant; the commits are each sorted, and merge in one pass; and the refs —
// every name a run holds, which a repository whose every push moves a ref of
// its own holds by the hundred thousand — are merged ONE KEY AT A TIME
// (refGroups), every input's refs of the key beside every input's moves of
// it, since both sections are in key order, so all a merge holds of them is
// one key's ordinals: at most 256 an input. Held whole, by name, the refs of
// two runs of 200 000 pushes each moving a ref of its own took 198 MB, and a
// repository at code.MaxRefsPerRepo churning its seat branches every 14 days
// would have taken some 0.7 GB a merge. A merge reads each input in order from
// its mapped file — once to count what it writes, then once a section — and
// writes each record once.
func mergeRuns(w io.Writer, ins []runFile, floor int64) (runHeader, error) {
	if len(ins) < 2 {
		return runHeader{}, fmt.Errorf("cache: a merge of %d runs: a merge reads two at least", len(ins))
	}
	var (
		// skip is the pushes at or below floor each drops: a prefix of
		// each. base is the merged index of each input's first kept push,
		// and start the offset of that push in the input's push section.
		skip  = make([]int, len(ins))
		base  = make([]int, len(ins))
		start = make([]uint64, len(ins))
		h     runHeader
		kept  int
	)
	h.from, h.through = max(ins[0].h.from, floor), ins[len(ins)-1].h.through
	for k, in := range ins {
		skip[k] = in.firstAbove(floor)
		start[k] = in.h.pushBytes
		if skip[k] < in.pushCount() {
			start[k] = binary.BigEndian.Uint64(in.data[in.l.offsets+int64(skip[k])*runOffsetBytes:])
		}
		base[k] = kept
		kept += in.pushCount() - skip[k]
		h.pushBytes += in.h.pushBytes - start[k]
		for i := range int(in.h.commits) {
			if int(binary.BigEndian.Uint32(in.commitRec(i)[runOIDBytes:])) >= skip[k] {
				h.commits++
			}
		}
	}
	switch {
	case kept == 0:
		return runHeader{}, fmt.Errorf("cache: a merge keeping no push: every push of its %d runs is at or below %d",
			len(ins), floor)
	case kept > math.MaxUint32:
		return runHeader{}, fmt.Errorf("cache: a merge of %d pushes, past the %d a run holds", kept,
			uint64(math.MaxUint32))
	}
	h.pushes = uint32(kept)
	g := newRefGroups(ins, floor)
	refs := 0
	for g.next() {
		refs += g.kept
		h.nameBytes += g.keptNameBytes
		h.moves += g.keptMoves
	}
	switch {
	case g.err != nil:
		return runHeader{}, g.err
	case refs > math.MaxUint32 || h.nameBytes > math.MaxUint32:
		return runHeader{}, fmt.Errorf("cache: a merge of %d refs named in %d bytes, past what a run holds", refs,
			h.nameBytes)
	}
	h.refs = uint32(refs)

	out := newRunWriter(w)
	out.put(h.encode())
	var at uint64
	for k, in := range ins {
		for i := skip[k]; i < in.pushCount(); i++ {
			off := binary.BigEndian.Uint64(in.data[in.l.offsets+int64(i)*runOffsetBytes:])
			out.u64(at + off - start[k])
		}
		at += in.h.pushBytes - start[k]
	}
	for k, in := range ins {
		out.put(in.data[in.l.pushes+int64(start[k]) : in.l.pushes+int64(in.h.pushBytes)])
	}
	// THE REFS, then their names: each kept ref of each key in name order,
	// its ordinal the next of its key.
	var nameAt uint32
	for g = newRefGroups(ins, floor); g.next(); {
		for o := range g.kept {
			name := g.name(o)
			out.ref(g.key, nameAt, uint16(len(name)), uint8(o))
			nameAt += uint32(len(name))
		}
	}
	for g = newRefGroups(ins, floor); g.next(); {
		for o := range g.kept {
			out.put(g.name(o))
		}
	}
	// THE MOVES, by (key, ordinal, position): one ref's moves the inputs' in
	// order — the lower positions first — each input's chain order kept.
	for g = newRefGroups(ins, floor); g.next(); {
		for o := range g.kept {
			for k, in := range ins {
				ord := g.inv[k][o]
				if ord < 0 {
					continue
				}
				for i := g.spanLo[k][ord]; i < g.spanHi[k][ord]; i++ {
					rec := in.moveRec(int(i))
					position := int64(binary.BigEndian.Uint64(rec[9:]))
					if position <= floor {
						continue
					}
					var old, nw [runOIDBytes]byte
					copy(old[:], rec[21:41])
					copy(nw[:], rec[41:61])
					push := uint32(base[k] + int(binary.BigEndian.Uint32(rec[17:])) - skip[k])
					out.move(g.key, uint8(o), position, push, old, nw, rec[61])
				}
			}
		}
	}
	if g.err != nil {
		return runHeader{}, g.err
	}
	// THE COMMITS, merged by id: one id's entries the lower input's first,
	// which keeps them in merged push order.
	next := make([]int, len(ins))
	keptCommit := func(k, i int) int {
		for ; i < int(ins[k].h.commits); i++ {
			if int(binary.BigEndian.Uint32(ins[k].commitRec(i)[runOIDBytes:])) >= skip[k] {
				break
			}
		}
		return i
	}
	for k := range ins {
		next[k] = keptCommit(k, 0)
	}
	for {
		best := -1
		for k, in := range ins {
			if next[k] >= int(in.h.commits) {
				continue
			}
			if best < 0 || bytes.Compare(in.commitRec(next[k])[:runOIDBytes],
				ins[best].commitRec(next[best])[:runOIDBytes]) < 0 {
				best = k
			}
		}
		if best < 0 {
			break
		}
		rec := ins[best].commitRec(next[best])
		var oid [runOIDBytes]byte
		copy(oid[:], rec[:runOIDBytes])
		out.commit(oid, uint32(base[best]+int(binary.BigEndian.Uint32(rec[runOIDBytes:]))-skip[best]))
		next[best] = keptCommit(best, next[best]+1)
	}
	return h, out.close()
}

// refGroups is a merge's walk of its inputs' refs ONE KEY AT A TIME, in key
// order (mergeRuns): each input's refs filed under the key and its moves of
// them — both sections keep a key's records together, and in key order — and
// the refs of every input merged by name, a name several inputs hold being
// one ref of the merged run, KEPT where a move of it in any input is above
// the floor, its ordinal the next of its key in name order. What it holds is
// a key's ordinals, at most 256 an input: a few kilobytes, however many refs
// the runs hold. A run whose sections do not agree — a move of a key or an
// ordinal no ref holds, names out of order, a key's refs past 256 — is no run
// this build wrote (errRunMalformed).
type refGroups struct {
	ins   []runFile
	floor int64
	// ref and move are each input's next ref entry and move record.
	ref, move []int
	// key is the group's key; kept, keptNameBytes and keptMoves what it
	// keeps: refs, the bytes of their names, and moves above the floor.
	key           [runKeyBytes]byte
	kept          int
	keptNameBytes uint64
	keptMoves     uint64
	// By each input's ORDINAL in the group: its ref entry (entryOf, -1 for
	// none), whether a move of it is above the floor (above), and its moves'
	// span (spanLo, spanHi). By the merged ref's ordinal: each input's
	// ordinal of its name (inv, -1 for none), and the input and entry its
	// name is read from (src).
	entryOf        [][256]int32
	above          [][256]bool
	spanLo, spanHi [][256]int32
	inv            [][256]int16
	src            [256]refSrc
	// ords is each input's ordinals in the group, in name order, which the
	// next group's start resets; at is each input's place among them as
	// the group's names are merged.
	ords [][]uint8
	at   []int
	err  error
}

// refSrc is where a merged ref's name is read from: an input, and its entry.
type refSrc struct {
	k     int
	entry int32
}

// newRefGroups is a walk of ins's refs, before its first key.
func newRefGroups(ins []runFile, floor int64) *refGroups {
	g := &refGroups{ins: ins, floor: floor, ref: make([]int, len(ins)), move: make([]int, len(ins)),
		entryOf: make([][256]int32, len(ins)), above: make([][256]bool, len(ins)),
		spanLo: make([][256]int32, len(ins)), spanHi: make([][256]int32, len(ins)),
		inv: make([][256]int16, len(ins)), ords: make([][]uint8, len(ins)), at: make([]int, len(ins))}
	for k := range ins {
		for o := range 256 {
			g.entryOf[k][o], g.inv[k][o] = -1, -1
		}
	}
	return g
}

// name is the name of the group's kept ref o, as its input holds it.
func (g *refGroups) name(o int) []byte {
	s := g.src[o]
	return g.ins[s.k].refName(int(s.entry))
}

// malformed stops the walk at a run that is no run this build wrote.
func (g *refGroups) malformed(format string, args ...any) bool {
	g.err = fmt.Errorf("%w: "+format, append([]any{errRunMalformed}, args...)...)
	return false
}

// next moves the walk to its next key, answering false at the end of every
// input's refs, or at a run that is malformed (err).
func (g *refGroups) next() bool {
	if g.err != nil {
		return false
	}
	// The last group's marks, reset where they were set.
	for k := range g.ins {
		for _, o := range g.ords[k] {
			g.entryOf[k][o], g.above[k][o], g.spanLo[k][o], g.spanHi[k][o] = -1, false, 0, 0
		}
		g.ords[k] = g.ords[k][:0]
		for o := range g.kept {
			g.inv[k][o] = -1
		}
	}
	g.kept, g.keptNameBytes, g.keptMoves = 0, 0, 0
	have := false
	for k, in := range g.ins {
		if g.ref[k] < int(in.h.refs) {
			if key := in.refRec(g.ref[k])[:runKeyBytes]; !have || bytes.Compare(key, g.key[:]) < 0 {
				copy(g.key[:], key)
				have = true
			}
		}
	}
	if !have {
		for k, in := range g.ins {
			if g.move[k] < int(in.h.moves) {
				return g.malformed("a move names a ref the run does not hold")
			}
		}
		return false
	}
	for k, in := range g.ins {
		// The input's refs of the key, each ordinal once, in name order.
		lo := g.ref[k]
		hi := lo
		for ; hi < int(in.h.refs); hi++ {
			e := in.refRec(hi)
			if !bytes.Equal(e[:runKeyBytes], g.key[:]) {
				break
			}
			o := e[14]
			switch {
			case g.entryOf[k][o] >= 0:
				return g.malformed("two refs of one key hold ordinal %d", o)
			case hi > lo && bytes.Compare(in.refName(hi-1), in.refName(hi)) >= 0:
				return g.malformed("the refs of one key are out of name order")
			}
			g.entryOf[k][o] = int32(hi)
			g.ords[k] = append(g.ords[k], o)
		}
		g.ref[k] = hi
		// Its moves of the key, each ordinal's together and in position
		// order: their spans, and which are above the floor.
		mi := g.move[k]
		if mi < int(in.h.moves) && bytes.Compare(in.moveRec(mi)[:runKeyBytes], g.key[:]) < 0 {
			return g.malformed("a move names a ref the run does not hold")
		}
		prev := -1
		for ; mi < int(in.h.moves); mi++ {
			rec := in.moveRec(mi)
			if !bytes.Equal(rec[:runKeyBytes], g.key[:]) {
				break
			}
			o := int(rec[runKeyBytes])
			switch {
			case g.entryOf[k][o] < 0:
				return g.malformed("a move names ordinal %d of a key whose refs do not hold it", o)
			case o < prev:
				return g.malformed("the moves of one key are out of ordinal order")
			case o != prev:
				g.spanLo[k][o] = int32(mi)
			}
			g.spanHi[k][o], prev = int32(mi+1), o
			if int64(binary.BigEndian.Uint64(rec[9:])) > g.floor {
				g.above[k][o] = true
				g.keptMoves++
			}
		}
		g.move[k] = mi
	}
	// The refs merged by name: each name once, kept where any input's moves
	// of it are above the floor.
	at := g.at
	for k := range at {
		at[k] = 0
	}
	for {
		least := -1
		var name []byte
		for k, in := range g.ins {
			if at[k] >= len(g.ords[k]) {
				continue
			}
			n := in.refName(int(g.entryOf[k][g.ords[k][at[k]]]))
			if least < 0 || bytes.Compare(n, name) < 0 {
				least, name = k, n
			}
		}
		if least < 0 {
			return true
		}
		keep := false
		for k, in := range g.ins {
			if at[k] < len(g.ords[k]) && bytes.Equal(in.refName(int(g.entryOf[k][g.ords[k][at[k]]])), name) {
				keep = keep || g.above[k][g.ords[k][at[k]]]
			}
		}
		if keep && g.kept == 256 {
			return g.malformed("257 refs of one run share the key of %s, past the 256 a run holds", name)
		}
		for k, in := range g.ins {
			if at[k] >= len(g.ords[k]) {
				continue
			}
			o := g.ords[k][at[k]]
			if !bytes.Equal(in.refName(int(g.entryOf[k][o])), name) {
				continue
			}
			if keep {
				g.inv[k][g.kept] = int16(o)
			}
			at[k]++
		}
		if keep {
			g.src[g.kept] = refSrc{k: least, entry: g.entryOf[least][g.ords[least][at[least]-1]]}
			g.keptNameBytes += uint64(len(name))
			g.kept++
		}
	}
}
