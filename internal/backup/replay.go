package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// The window: what the records a restore replays past the copy name.
//
// A restore brings the store copy back at position P on each declaring log and
// replays the log from there to the snapshot's last sequence L — the snapshot
// is point-in-time at the sequence its answer states, however much is appended
// while it transfers — and a build that can read a record the copy's applier
// deferred applies that too. So the rows a restore ends with name the copy's
// objects OVERLAID BY whatever those records write, and the backup carries
// both: the copy's before the streams, these after them. See the package doc's
// "The objects follow the log" for why this and not a read of the live rows.

// windowBufferBytes bounds each pull the window's reader makes, and so how much
// of a log it holds ahead of what it has read: [statelog.MaxDeliveryBytes], the
// largest record the transport carries WITH what the broker counts beside it
// when it delivers one.
//
// NOT THE TRANSPORT'S CEILING ALONE (queue.MaxPayloadBytes). The broker admits
// a publish whose headers and payload reach that ceiling, and then sizes its
// DELIVERY against a pull's byte bound as the subject, the acknowledgement
// subject, the headers and the payload together — so a record at the ceiling
// never fits a pull bounded by the ceiling: every pull is answered "message
// size exceeds max bytes" with nothing delivered, the client pulls again with
// the same bound, and the backup is refused when the wait runs out
// (TestABackupReadsPastARecordAtTheTransportCeiling, on the pinned broker). The
// framework's own pull is sized past the same floor for the same reason
// (statelog's FetchBytes); this one holds one maximal record, since the window
// is read one record at a time.
//
// NOR A LOG'S OWN LARGEST APPEND ([statelog.StreamSpec.MaxAppendBytes]): the
// window names each declaring log by its stream alone, and what bounds the
// records on a log the backup reads is the broker that admitted them — a
// stream made under another build's declaration, or by an operator's restore,
// holds whatever the transport carried.
const windowBufferBytes = statelog.MaxDeliveryBytes

// window is what the records past the copy name, keyed for the two questions
// the pass asks of it: which objects it owes beyond the copy's, and whether a
// row's last record left it naming a given object.
//
// BOUNDED BY THE BACKUP'S OWN WINDOW, never by the inventory: one entry per row
// a record written during the copy, the object pass and the snapshots touched,
// and one per object such a record named — a few MiB at a busy company's
// rate, which is what a backup that ran for hours holds.
type window struct {
	// objects is every object a record names, with the row of the first
	// record naming it.
	objects map[objstore.Key]reference
	// last is each row's LAST record: the object it left the row naming,
	// or none.
	last map[ownerKey]lastWrite
}

// ownerKey is one row of one declared table.
type ownerKey struct{ table, namedBy string }

// lastWrite is what the last record touching a row left it naming.
type lastWrite struct {
	object objstore.Object
	names  bool
}

func newWindow() window {
	return window{objects: map[objstore.Key]reference{}, last: map[ownerKey]lastWrite{}}
}

// note folds one record's references into the window, in the order a restore
// applies them.
func (w window) note(t objstore.ReferenceTable, refs []objstore.RecordReference) error {
	for _, r := range refs {
		w.last[ownerKey{t.Table, r.NamedBy}] = lastWrite{object: r.Object, names: r.Names}
		if !r.Names {
			continue
		}
		ref := reference{Object: r.Object, NamedBy: r.NamedBy, Table: t.Table}
		if prior, held := w.objects[r.Object.Key]; held {
			if prior.Object != r.Object {
				return contradiction(prior, ref)
			}
			continue
		}
		w.objects[r.Object.Key] = ref
	}
	return nil
}

// owesCopied reports whether a restore names ref, an object the COPY names: it
// does unless a record past the copy left ref's row naming another object or
// none — a file replaced or removed while the backup ran.
func (w window) owesCopied(ref reference) bool {
	lw, touched := w.last[ownerKey{ref.Table, ref.NamedBy}]
	return !touched || (lw.names && lw.object.Key == ref.Object.Key)
}

// owesReplayed reports whether a restore names ref, an object only a record
// past the copy names: it does where the last record of its row left the row
// naming it, and not where a later one replaced it again.
func (w window) owesReplayed(ref reference) bool {
	lw, touched := w.last[ownerKey{ref.Table, ref.NamedBy}]
	return touched && lw.names && lw.object.Key == ref.Object.Key
}

// readWindow reads what every record a restore replays past the copy names
// into the pass's window — the records the copy's applier deferred, and, where
// the streams were snapshotted (nc non-nil), each declaring log from the copy's
// position through the snapshot's last sequence.
//
// EVERY RECORD, APPLIED OR NOT: whichever records a restore's applier drops (an
// evicted writer's, a redelivery its guard skips), what is read here is a
// superset of what the restored rows name, and the backup never has to decide
// which records apply. A record this build cannot read that may write a
// declared table refuses the backup ([objstore.ErrRecordUnreadable]): it may
// name an object the restore needs, and nobody can tell which. And EVERY
// RECORD IS A SIGNED FRAME, opened and verified before it is read
// ([objectPass.noteRecord]).
func (p *objectPass) readWindow(ctx context.Context, nc *nats.Conn, api jsapi.API,
	m Manifest, cursors map[string]statelog.FileCursor) error {

	if len(p.required) == 0 {
		return nil
	}
	logs, err := declaringLogs(p.required, p.verifiers)
	if err != nil {
		return err
	}
	if err = p.readDeferred(ctx, logs); err != nil {
		return err
	}
	if nc == nil {
		// A NODE ON AN EXTERNAL CLUSTER snapshots no stream, so there is
		// no last sequence to read to: its backup carries what its copy
		// names, and the cluster's own backup is taken at another moment
		// (docs/guides/backup.md).
		return nil
	}
	js, err := api.Client(nc)
	if err != nil {
		return fmt.Errorf("backup: reach the JetStream API: %w", err)
	}
	for _, l := range logs {
		if err := p.readLog(ctx, js, l, m, cursors); err != nil {
			return err
		}
	}
	return nil
}

// declaringLog is one log a Required table is written by, every Required table
// it writes, and the verifier its records' frames are opened with.
type declaringLog struct {
	stream, deferred, domain string
	tables                   []objstore.ReferenceTable
	verifier                 *statelog.Verifier
}

// verifiers is a verifier for the domain of every Required table among
// tables, keyed by domain, each opening that domain's frames under ring.
//
// PER DOMAIN, as the applier's are: a record's signature binds the name of the
// domain whose log it was appended to, so one verifier for every log would
// open none of them (statelog's deriveRecordKey).
func verifiers(tables []objstore.ReferenceTable, ring statelog.Keyring) (map[string]*statelog.Verifier, error) {
	out := map[string]*statelog.Verifier{}
	for _, t := range tables {
		if t.Standing != objstore.Required {
			continue
		}
		if _, made := out[t.Domain]; made {
			continue
		}
		v, err := statelog.NewVerifier(t.Domain, ring)
		if err != nil {
			return nil, err
		}
		out[t.Domain] = v
	}
	return out, nil
}

// declaringLogs groups the Required tables by the log that writes them, sorted
// by stream so a refusal names the same log every time, each with the verifier
// of its domain from verified. Each was validated to name its log
// ([objstore.ReferenceTable.ValidateLog]); two tables of one log naming two
// deferred-record tables, or two domains, are declarations that disagree about
// one log, and are refused rather than read twice.
func declaringLogs(required []objstore.ReferenceTable,
	verified map[string]*statelog.Verifier) ([]declaringLog, error) {

	byStream := map[string]*declaringLog{}
	var out []*declaringLog
	for _, t := range required {
		if err := t.ValidateLog(); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		l, seen := byStream[t.Log]
		if !seen {
			v, held := verified[t.Domain]
			if !held {
				return nil, fmt.Errorf("backup: no verifier for %s, whose log %s a "+
					"restore replays — its records cannot be read for the files they name",
					t.Domain, t.Log)
			}
			l = &declaringLog{stream: t.Log, deferred: t.Deferred, domain: t.Domain, verifier: v}
			byStream[t.Log] = l
			out = append(out, l)
		}
		if l.deferred != t.Deferred {
			return nil, fmt.Errorf("backup: %s and %s are both written by %s and name its "+
				"deferred records %s and %s — one log's domain defers into one table",
				l.tables[0].Table, t.Table, t.Log, l.deferred, t.Deferred)
		}
		if l.domain != t.Domain {
			return nil, fmt.Errorf("backup: %s and %s are both written by %s and name its "+
				"domain %s and %s — one log is one domain's, and its records are signed "+
				"under that domain's name", l.tables[0].Table, t.Table, t.Log, l.domain, t.Domain)
		}
		l.tables = append(l.tables, t)
	}
	slices.SortFunc(out, func(a, b *declaringLog) int {
		switch {
		case a.stream < b.stream:
			return -1
		case a.stream > b.stream:
			return 1
		}
		return 0
	})
	logs := make([]declaringLog, len(out))
	for i, l := range out {
		logs[i] = *l
	}
	return logs, nil
}

// noteRecord folds one record of l — framed, the bytes the broker stores —
// into the window through every table l writes, naming where it came from in a
// refusal.
//
// THE FRAME IS OPENED AND VERIFIED FIRST, as statelog's verified log opens one
// for every reader that decodes a log straight off it (statelog.VerifiedLog):
// each declaration's Records decodes a record's BODY, and handed the frame it
// read every signed record as one no build reads. Strict, for the verified
// log's reason — what the backup decides from a record is which files it
// carries — so a record under a key this node does not hold refuses the
// backup ([statelog.ErrRecordUnverified]) rather than being read for files on
// trust: its node holds a keyring short of the fleet's, whose applier is
// holding the same record back, and the remedy is the key. A frame the
// keyring does not open at all is not this fleet's ([statelog.ErrRecordTampered])
// and is refused for the same reason the applier refuses it.
func (p *objectPass) noteRecord(l declaringLog, where string, framed []byte) error {
	payload, verdict := l.verifier.Open(framed)
	switch verdict {
	case statelog.Verified:
	case statelog.KeyUnknown:
		return fmt.Errorf("backup: %s: %w (this node holds %s) — a restore replays it "+
			"and it may name a file this backup would not carry; add the key it was "+
			"signed under to secrets.keys and restart this node, or take the backup "+
			"from a node that holds it", where, statelog.ErrRecordUnverified,
			words.And(l.verifier.KeyIDs()))
	default:
		return fmt.Errorf("backup: %s: %w — the log a restore replays holds bytes "+
			"no node of this fleet signed", where, statelog.ErrRecordTampered)
	}
	for _, t := range l.tables {
		refs, err := t.Records(payload)
		if err != nil {
			if errors.Is(err, objstore.ErrRecordUnreadable) {
				return fmt.Errorf("backup: %s: %w — a restore replays it and it may name a file "+
					"this backup would not carry; take the backup from a node running the build "+
					"that wrote it", where, err)
			}
			return fmt.Errorf("backup: %s cannot be read as the %s it may write: %w",
				where, t.Table, err)
		}
		if err := p.win.note(t, refs); err != nil {
			return err
		}
	}
	return nil
}

// deferredPage is how many deferred records one read of the copy takes:
// [statelog.ReprocessPage], the page the framework walks the same table in.
//
// THE FRAMEWORK'S OWN NUMBER, because the question is the framework's: the
// deferred records of a node that sat out a long rolling upgrade are every
// record of a newer version and every later one whose scope met one, so the
// table grows with the upgrade rather than with anything a backup bounds — and
// the applier that reprocesses it already holds a page of exactly this size,
// of exactly these rows, on this node. A second number here would be a second
// opinion about how much of that backlog a node may hold at once.
const deferredPage = statelog.ReprocessPage

// deferredRecord is one record the copy's applier deferred: where it sits on
// its log, packed, and the bytes the broker stored — its signed frame, which
// is what the framework retains.
type deferredRecord struct {
	position int64
	payload  []byte
}

// readDeferred folds every record the copy's appliers deferred on a declaring
// log into the window, in log order.
//
// A DEFERRED RECORD IS ONE A RESTORE MAY APPLY: a build that reads it applies
// it at its own position, so the file it writes is one the restored rows name
// though the copy's do not. A record this build deferred because it cannot read
// it is a newer build's, which refuses the backup as any such record does.
//
// A PAGE AT A TIME ([deferredPage]), each page read in a read of its own and
// folded once that read has ended — never the whole table in one slice under
// one read, which held a stalled upgrade's whole backlog, payloads and all, and
// one of the copy's connections for as long as it took to scan it. The first
// record that refuses the backup ends the walk.
func (p *objectPass) readDeferred(ctx context.Context, logs []declaringLog) error {
	db, err := store.OpenEstate(ctx, store.EstateReplicated, p.copyPath, store.Options{})
	if err != nil {
		return fmt.Errorf("backup: open the copy to read the records it deferred: %w", err)
	}
	defer func() { _ = db.Close() }()
	for _, l := range logs {
		query, err := l.tables[0].DeferredRecordsAfter(deferredPage)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		var (
			after int64
			page  []deferredRecord
		)
		for {
			if err := db.Read(ctx, func(tx *sql.Tx) error {
				// RESET ON EVERY RUN: a read that is retried runs
				// this again, and a page kept from the run that
				// failed would be folded twice.
				page = page[:0]
				rows, qerr := tx.QueryContext(ctx, query, after)
				if qerr != nil {
					return qerr
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var d deferredRecord
					if serr := rows.Scan(&d.position, &d.payload); serr != nil {
						return serr
					}
					page = append(page, d)
				}
				return rows.Err()
			}); err != nil {
				return fmt.Errorf("backup: read the records the copy deferred on %s: %w", l.stream, err)
			}
			if p.deferredRead != nil {
				p.deferredRead(len(page))
			}
			for _, d := range page {
				at := statelog.Position{Stream: l.stream,
					Generation: uint32(d.position / statelog.GenerationStride),
					Seq:        uint64(d.position % statelog.GenerationStride)}
				where := fmt.Sprintf("the record the copy deferred at %s", at)
				if err := p.noteRecord(l, where, d.payload); err != nil {
					return err
				}
				after = d.position
			}
			if len(page) < deferredPage {
				break
			}
		}
	}
	return nil
}

// readLog folds (P, L] of one declaring log into the window: every record
// after the copy's position through the last sequence the log's snapshot
// captured.
func (p *objectPass) readLog(ctx context.Context, js jetstream.JetStream, l declaringLog,
	m Manifest, cursors map[string]statelog.FileCursor) error {

	artifact, captured := snapshotOf(m, l.stream)
	if !captured {
		// THE LOG DOES NOT EXIST, so nothing replays it.
		return nil
	}
	first, last, err := capturedBounds(artifact)
	if err != nil {
		return err
	}
	from := m.Domains[l.stream].Seq
	if last <= from {
		return nil
	}
	// THE COPY'S POSITION IS A POSITION ON ONE INSTANCE OF THE LOG. Read
	// against a stream recreated since, its sequence numbers name other
	// records, and the window would be somebody else's.
	//
	// THE LOOKUP IS ASKED AS A METADATA READ IS ([jsprovision.ReadBounded]):
	// it is the stream's info, which only the stream's leader answers, and a
	// member that still believes in a leader its group has just lost drops
	// the request without a word — sent once, it waited out the backup's
	// deadline, or the client's five-second default for a backup with none,
	// and refused the backup over a reply nobody would send. A backup with
	// no deadline keeps those five seconds, asked again inside them.
	info, err := jsprovision.ReadBounded(ctx, js.Options().DefaultTimeout,
		func(ctx context.Context) (*jetstream.StreamInfo, error) {
			stream, lookErr := js.Stream(ctx, l.stream)
			if lookErr != nil {
				return nil, lookErr
			}
			return stream.CachedInfo(), nil
		}, nil)
	if err != nil {
		return fmt.Errorf("backup: look up %s to read past the copy: %w", l.stream, err)
	}
	cursor, keyed := cursors[l.stream]
	created, err := statelog.StreamCreatedAt(info)
	if err != nil {
		return fmt.Errorf("backup: identify %s to read past the copy: %w", l.stream, err)
	}
	if statelog.IdentityOf(cursor.StreamCreatedAt, created, keyed) == statelog.StreamRecreated {
		return fmt.Errorf("backup: the copy's checkpoint on %s was taken on an earlier "+
			"instance of the stream (created %s; the live one was created %s), so its "+
			"position is not a position on the log a restore replays — take the backup "+
			"again once this node has re-keyed to it: %w", l.stream,
			cursor.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
			created.UTC().Format(time.RFC3339Nano), statelog.ErrStreamRecreated)
	}
	if !statelog.Replayable(from, first) {
		// THE ASSERTION REFUSES THIS for a log the copy holds a cursor on;
		// this is the one it cannot see, a copy with none on a log already
		// trimmed.
		return fmt.Errorf("%w: %s starts at %d and the copy stands at %d, so what the "+
			"records a restore replays from the copy name cannot be read", ErrLogGap,
			l.stream, first, from)
	}
	records, err := readRange(ctx, js, l.stream, from, last, func(seq uint64, payload []byte) error {
		return p.noteRecord(l, fmt.Sprintf("the record at %s sequence %d", l.stream, seq), payload)
	})
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "backup_log_replayed", "log", l.stream, "from", from, "through", last,
		"records", records, "objects", len(p.win.objects),
		"detail", "every record a restore replays past the store copy was read for the files it names")
	return nil
}

// readRange hands each record of stream from after from through through to
// each, in order, and answers how many it read.
//
// AN ORDERED CONSUMER'S ITERATOR, never a fetch: the library re-creates an
// ordered consumer on every Fetch, a consumer creation per batch, where the
// iterator keeps one and remakes it by itself when it loses its heartbeats or a
// sequence. What proves nothing was skipped either way is the CONTIGUITY CHECK:
// each record must be the sequence after the one before, so a record the log
// no longer holds refuses the backup rather than leaving a file uncounted.
//
// EACH RECORD IS WAITED FOR UP TO snapshotChunkWait — the bound a snapshot's
// next chunk gets — through the wait's own context. A shorter wait asked again
// would never see the iterator's heartbeat check fire (it is rescheduled at
// every wait, and fires at twice the heartbeat), so a consumer whose pull the
// broker dropped would be waited on until the bound rather than remade.
func readRange(ctx context.Context, js jetstream.JetStream, stream string, from, through uint64,
	each func(seq uint64, payload []byte) error) (int, error) {

	cons, err := js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   from + 1,
	})
	if err != nil {
		return 0, fmt.Errorf("backup: read %s past the copy: %w", stream, err)
	}
	iter, err := cons.Messages(jetstream.PullMaxBytes(windowBufferBytes))
	if err != nil {
		return 0, fmt.Errorf("backup: read %s past the copy: %w", stream, err)
	}
	defer iter.Stop()
	read := 0
	for next := from + 1; next <= through; next++ {
		msg, err := nextRecord(ctx, iter)
		switch {
		case ctx.Err() != nil:
			return read, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return read, fmt.Errorf("backup: the broker stopped delivering %s at %d of %d "+
				"(no record for %s)", stream, next, through, snapshotChunkWait)
		case err != nil:
			return read, fmt.Errorf("backup: read %s past the copy: %w", stream, err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			return read, fmt.Errorf("backup: place a record of %s: %w", stream, err)
		}
		if got := meta.Sequence.Stream; got != next {
			return read, fmt.Errorf("%w: %s holds no record %d between the copy and the "+
				"snapshot (the next it holds is %d), so what a restore replays cannot all be read",
				ErrLogGap, stream, next, got)
		}
		if err := each(next, msg.Data()); err != nil {
			return read, err
		}
		read++
	}
	return read, nil
}

// nextRecord waits for the iterator's next record, bounded by snapshotChunkWait.
func nextRecord(ctx context.Context, iter jetstream.MessagesContext) (jetstream.Msg, error) {
	waitCtx, cancel := context.WithTimeout(ctx, snapshotChunkWait)
	defer cancel()
	return iter.Next(jetstream.NextContext(waitCtx))
}

// snapshotOf is the stream artefact of the named stream, if the backup took one.
func snapshotOf(m Manifest, name string) (StreamArtifact, bool) {
	for _, a := range m.Streams {
		if a.Name == name {
			return a, true
		}
	}
	return StreamArtifact{}, false
}

// capturedBounds is the first and last sequence a stream's snapshot captured —
// the server's own state, kept verbatim in the artefact. The last is exactly
// where a restore of the snapshot ends: the snapshot is point-in-time at the
// state its answer states, whatever is appended while it transfers.
func capturedBounds(a StreamArtifact) (first, last uint64, err error) {
	if len(a.State) == 0 {
		return 0, 0, fmt.Errorf("backup: the snapshot of %s carries no state, so what a "+
			"restore of it replays cannot be established", a.Name)
	}
	var state struct {
		FirstSeq uint64 `json:"first_seq"`
		LastSeq  uint64 `json:"last_seq"`
	}
	if err := json.Unmarshal(a.State, &state); err != nil {
		return 0, 0, fmt.Errorf("backup: read %s's captured stream state: %w", a.Name, err)
	}
	return state.FirstSeq, state.LastSeq, nil
}
