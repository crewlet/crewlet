package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DIRECTORY READS, for a caller already authorised to see the directory:
// pages, where read.go's request-path lookups are keyed and deliberately
// narrow (a sign-in may not become a roster).
//
// Pages key on the person's uuid7 id, so id order is creation order and a
// cursor is the previous page's last id. Nothing sorts on a sealed value:
// ciphertext order means nothing to a reader.
//
// Nothing here opens a sealed value: [PersonRow] carries ciphertext and the
// surface opens what it shows, so a read that only needs who may do what
// never holds names and addresses in the clear.

// PersonRow is one directory row as an administrator reads it. It is not
// [Sighting], what a sign-in resolves: folding them would put fields a
// refusal must never disclose on the sign-in path.
type PersonRow struct {
	ID    string
	Kind  iam.Kind
	Stage iam.Stage

	// Login is in the CLEAR, unlike the name and the address: the
	// dashboard prints it on every row, and blinding it would cost an
	// operator their audit trail for nothing.
	Login string

	// NameSealed and EmailSealed are ciphertext under the fleet keyring. A
	// removed person has no row (the tombstone is in `iam_removed`), so a
	// value that will not open is a key this ring lacks: dropped before the
	// value was moved off it, or a restore under another keyring.
	NameSealed  []byte
	EmailSealed []byte

	// Seat is the handle of this person's seat, immutable (ADR-0013). A
	// person always holds one ([Writer.Create], ADR-0032); only a service
	// account acting as itself holds none.
	Seat string

	Grants []iam.Grant

	// Epoch is the person's revocation epoch: a session or token minted
	// below it is over.
	Epoch uint64

	CreatedAt time.Time
	UpdatedAt time.Time

	// Version is the iam position that last wrote this row.
	Version uint64
}

// PeopleQuery is one page of the directory.
type PeopleQuery struct {
	// After is the last id of the previous page, exclusive. Empty starts
	// at the beginning.
	After string

	// Limit is how many rows at most. Zero takes [DefaultPageSize]; above
	// [MaxPageSize] is clamped, not refused: a caller asking for a
	// thousand wants "lots", and a refusal sends them to loop instead.
	Limit int

	// Q narrows on the LOGIN and the SEAT, the only identity values held
	// in the clear: a search over sealed names and addresses would open
	// every person in the company per keystroke.
	Q string

	// Stage narrows to one enrolment stage; empty takes every stage.
	Stage iam.Stage
}

// DefaultPageSize and MaxPageSize bound a directory page, sized by what a
// caller does with a row rather than by the SQL: a surface opens and renders
// every row, and a directory row is the largest answer this estate serves.
// Fifty is a screen of people; past two hundred an operator is better served
// by narrowing (`q=`, `stage=`).
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// pageLimit is one directory page's row count: [DefaultPageSize] when none is
// named, and at most [MaxPageSize].
func pageLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	}
	return limit
}

// PeoplePage is one page and where the next one starts.
type PeoplePage struct {
	People []PersonRow

	// Next is the cursor for the following page, empty at the end.
	Next string

	// At is the position this node had applied when it answered, so a
	// reader can tell a quiet directory from a lagging node.
	At statelog.Position
}

// personRowSelect is a directory row as [scanPerson] reads it: the person and
// the revocation epoch beside it, joined by the person's key.
const personRowSelect = `
	SELECT p.id, p.kind, p.stage, p.login, p.name_sealed, p.email_sealed,
	       p.seat_id, p.document,
	       p.created_at, p.updated_at, p.version,
	       COALESCE(e.epoch, 0)
	FROM iam_people p
	LEFT JOIN iam_revocation_epochs e ON e.person_id = p.id`

// peoplePageStatement is one directory page in id order past q.After,
// narrowed by q's stage and term, limited to ONE MORE than asked: the extra
// row says there is a next page without a second count, and is dropped.
func peoplePageStatement(q PeopleQuery, limit int) (string, []any) {
	query := strings.Builder{}
	query.WriteString(personRowSelect + `
		WHERE p.id > ?`)
	args := []any{q.After}
	if q.Stage != "" {
		query.WriteString(` AND p.stage = ?`)
		args = append(args, string(q.Stage))
	}
	if term := strings.TrimSpace(q.Q); term != "" {
		query.WriteString(` AND (p.login LIKE ? OR p.seat_id LIKE ?)`)
		like := "%" + escapeLike(term) + "%"
		args = append(args, like, like)
	}
	query.WriteString(` ORDER BY p.id LIMIT ?`)
	args = append(args, limit+1)
	return query.String(), args
}

// People answers one page of the directory.
func (r *Reader) People(ctx context.Context, q PeopleQuery) (PeoplePage, error) {
	limit := pageLimit(q.Limit)
	out := PeoplePage{At: r.reached()}
	query, args := peoplePageStatement(q, limit)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("iamdomain: read the directory: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			row, err := scanPerson(rows)
			if err != nil {
				return err
			}
			out.People = append(out.People, row)
		}
		return rows.Err()
	})
	if err != nil {
		return PeoplePage{}, err
	}
	if len(out.People) > limit {
		out.Next = out.People[limit-1].ID
		out.People = out.People[:limit]
	}
	return out, nil
}

// Removed answers which of ids carry a removal tombstone, in the order given.
// Another domain's erasure reconciles against it (ADR-0053's Dashboard
// Assistant conversations): a removal is permanent.
//
// AN ORDINARY READ, never identity work ([store.Identity]): a background pass
// must not spend the connection held back for the lookups requests decide on.
func (r *Reader) Removed(ctx context.Context, ids []string) ([]string, error) {
	out := []string{}
	if len(ids) == 0 {
		return out, nil
	}
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		for chunk := range slices.Chunk(ids, removedChunk) {
			among, args := inList(chunk)
			rows, err := tx.QueryContext(ctx,
				`SELECT person_id FROM iam_removed WHERE person_id IN `+among, args...)
			if err != nil {
				return err
			}
			found := map[string]bool{}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return err
				}
				found[id] = true
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			for _, id := range chunk {
				if found[id] {
					out = append(out, id)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read which persons were removed: %w", err)
	}
	return out, nil
}

// removedChunk is how many ids one statement of [Reader.Removed] asks about:
// five hundred, the object store's references batch: far under the driver's
// bind limit, and one statement never holds the reader long.
const removedChunk = 500

// personRowStatement is one directory row, by the person's key.
const personRowStatement = personRowSelect + `
	WHERE p.id = ?`

// Person answers one directory row, or [ErrNotFound]. The sentinel is
// deliberate, unlike [Reader.PersonByLogin]'s zero value: the guard has
// already authorised the caller, so there is no enumeration to protect, and a
// surface needs 404 apart from 200.
func (r *Reader) Person(ctx context.Context, id string) (PersonRow, error) {
	var out PersonRow
	found := false
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, personRowStatement, id)
		if err != nil {
			return fmt.Errorf("iamdomain: read person %q: %w", id, err)
		}
		defer rows.Close()
		if !rows.Next() {
			return rows.Err()
		}
		if out, err = scanPerson(rows); err != nil {
			return err
		}
		found = true
		return rows.Err()
	})
	switch {
	case err != nil:
		return PersonRow{}, err
	case !found:
		return PersonRow{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return out, nil
}

// scanPerson reads one directory row, opening the document for the halves no
// column carries.
func scanPerson(rows *sql.Rows) (PersonRow, error) {
	var (
		out      PersonRow
		kind     string
		stage    string
		document []byte
		created  int64
		updated  int64
		version  int64
		epoch    int64
	)
	if err := rows.Scan(&out.ID, &kind, &stage, &out.Login, &out.NameSealed,
		&out.EmailSealed, &out.Seat, &document,
		&created, &updated, &version, &epoch); err != nil {
		return PersonRow{}, fmt.Errorf("iamdomain: scan a directory row: %w", err)
	}
	out.Kind = iam.Kind(kind)
	out.Stage = iam.Stage(stage)
	out.Epoch = uint64(epoch)
	person, err := DecodePerson(document)
	if err != nil {
		return PersonRow{}, fmt.Errorf("iamdomain: open person %q: %w",
			out.ID, err)
	}
	out.Grants = person.Grants
	out.CreatedAt = fromMillis(created)
	out.UpdatedAt = fromMillis(updated)
	out.Version = uint64(version)
	return out, nil
}

// escapeLike makes a caller's term a literal in a LIKE pattern: otherwise a
// typed `%` matches the whole directory and `_` any character, and the
// narrowing an operator believes they applied quietly does not apply.
func escapeLike(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(term)
}

// CredentialRow is one credential as the directory reports it.
//
// NO VERIFIER: an argon2id digest or a token hash is offline-attackable, and a
// listing is the surface most likely to be copied into a ticket.
type CredentialRow struct {
	ID        string
	PersonID  string
	Method    CredentialMethod
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time

	// Label is what the mint called it, telling a person's tokens apart.
	Label string

	// Grants are what a machine token was minted carrying: its ceiling,
	// re-cut to its owner's grants on every request. Empty on every other
	// method.
	Grants []iam.Grant

	// Superseded marks a machine token a COUNTER ended: its owner's
	// revocation epoch or the company's session generation moved past its
	// minted value ([credential.CheckToken]). Neither writes `revoked_at`,
	// so the row alone would list those tokens as in use.
	Superseded bool

	// Spent is a reset link its person used — see [Credential.Spent].
	Spent bool
}

// Revoked reports a credential that has been withdrawn, has aged out, or was
// ended by a counter ([CredentialRow.Superseded]).
func (c CredentialRow) Revoked(now time.Time) bool {
	return !c.RevokedAt.IsZero() || c.Superseded ||
		(!c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt))
}

// Credentials lists what one person proves themselves with. The counters are
// read in the rows' snapshot, so a token is reported ended by exactly what
// ended it then ([CredentialRow.Superseded]).
func (r *Reader) Credentials(ctx context.Context, personID string) (
	[]CredentialRow, error) {

	var out []CredentialRow
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		epoch, err := epochOf(ctx, tx, personID)
		if err != nil {
			return err
		}
		generation, err := generationOf(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, person_id, method, created_at, expires_at, revoked_at,
			       document
			FROM iam_credentials WHERE person_id = ? ORDER BY created_at DESC`,
			personID)
		if err != nil {
			return fmt.Errorf("iamdomain: read person %q's credentials: %w",
				personID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row      CredentialRow
				method   string
				created  int64
				expires  int64
				revoked  int64
				document []byte
			)
			if err := rows.Scan(&row.ID, &row.PersonID, &method, &created,
				&expires, &revoked, &document); err != nil {
				return fmt.Errorf("iamdomain: scan a credential: %w", err)
			}
			row.Method = CredentialMethod(method)
			row.CreatedAt = fromMillis(created)
			row.ExpiresAt = fromMillis(expires)
			row.RevokedAt = fromMillis(revoked)
			if held, err := DecodeCredential(document); err == nil {
				row.Label = held.Label
				row.Grants = held.Grants
				row.Spent = held.Spent
				row.Superseded = held.EndedBy(Counters{Epoch: epoch,
					Generation: generation})
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InvitationsQuery is one page of the invitations this estate holds.
type InvitationsQuery struct {
	// After is the previous page's last id, exclusive; Limit is
	// [PeopleQuery.Limit]'s. An invitation's id is a uuid7 at its issue
	// ([Blinder.InvitationID]), so id order is issue order.
	After string
	Limit int

	// All includes the invitations that no longer open (redeemed or
	// expired, held until the sweep). Without it a page holds the OPEN ones
	// alone, judged at Now.
	All bool
	Now time.Time
}

// InvitationPage is one page of invitations and where the next one starts.
type InvitationPage struct {
	Invitations []InvitationRow
	Next        string
	At          statelog.Position
}

// invitationsPageStatement is one page of the invitations, in the order of
// their own key from past q.After, limited to one more than asked for as
// [peoplePageStatement] is.
func invitationsPageStatement(q InvitationsQuery, limit int) (string, []any) {
	query := `SELECT ` + invitationColumns + ` FROM iam_invites WHERE id > ?`
	args := []any{q.After}
	if !q.All {
		// OPEN is [InvitationRow.Spent]'s complement and the predicate a
		// directory decide holds a value against ([openInvitation]), so
		// the listing shows exactly the invitations that hold something.
		query += ` AND ` + openInvitation
		args = append(args, q.Now.UnixMilli())
	}
	query += ` ORDER BY id LIMIT ?`
	return query, append(args, limit+1)
}

// Invitations answers one page of the invitations this estate holds. The row
// carries the verifier, as [Reader.InvitationByID]'s does; a surface renders
// none of it, nor the link, which only the issue that made it shows, once.
func (r *Reader) Invitations(ctx context.Context, q InvitationsQuery) (
	InvitationPage, error) {

	limit := pageLimit(q.Limit)
	out := InvitationPage{At: r.reached()}
	query, args := invitationsPageStatement(q, limit)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("iamdomain: read the invitations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			row, err := scanInvitation(rows.Scan)
			if err != nil {
				return err
			}
			out.Invitations = append(out.Invitations, row)
		}
		return rows.Err()
	})
	if err != nil {
		return InvitationPage{}, err
	}
	if len(out.Invitations) > limit {
		out.Next = out.Invitations[limit-1].ID
		out.Invitations = out.Invitations[:limit]
	}
	return out, nil
}

// SessionRecord is one session as the directory reports it.
//
// NO BEARER, nor anything derived from one: a lineage is in every audit row,
// but anything a session could be resumed from would make this listing worth
// attacking.
type SessionRecord struct {
	Lineage   string
	PersonID  string
	Epoch     uint64
	CreatedAt time.Time
	ExpiresAt time.Time
	EndedAt   time.Time
	EndedWhy  string

	// EnrolmentOnly is a session that may only enrol a second factor
	// ([Session.EnrolmentOnly]), told apart for an administrator.
	EnrolmentOnly bool

	// UserAgent and Remote are the browser that opened the session and the
	// address it opened from ([Session.UserAgent]), so a person can tell
	// their sessions apart; empty where the start named none.
	UserAgent string
	Remote    string

	// Superseded marks a session a COUNTER ended: opened before the
	// company's last invalidation, or below its person's current revocation
	// epoch ([sessionSuperseded]). The invalidation writes no `ended_at`,
	// nor does an epoch move on every session it ends (see
	// [Reader.Sessions]), so the row alone would list those live.
	Superseded bool
}

// Live reports a session nothing has ended: not ended, not past its absolute
// deadline, not superseded. [Reader.SessionStanding] decides a named sign-out
// by it too, so a screen never offers to end a session the engine calls over.
func (s SessionRecord) Live(now time.Time) bool {
	return s.EndedAt.IsZero() && !s.Superseded &&
		(s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt))
}

// sessionSuperseded is whether a counter has ended a session: it opened (at
// start) before the company's last invalidation, or carries a revocation epoch
// below its person's current one.
func sessionSuperseded(start, epoch, invalidated, current uint64) bool {
	return start < invalidated || epoch < current
}

// Sessions lists one person's sessions, newest first, ENDED ONES INCLUDED:
// "this session was revoked, and why" is what an investigation looks for, and
// the row is kept until the sweep for exactly that. The counters are read in
// the rows' snapshot, so a session is reported ended by exactly what ended it
// then ([SessionRecord.Superseded]).
func (r *Reader) Sessions(ctx context.Context, personID string) (
	[]SessionRecord, error) {

	var out []SessionRecord
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		invalidated, err := readInvalidated(ctx, tx)
		if err != nil {
			return err
		}
		current, err := epochOf(ctx, tx, personID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT lineage, person_id, epoch, start_position, created_at,
			       absolute_expires_at, ended_at, ended_reason, document
			FROM iam_sessions WHERE person_id = ?
			ORDER BY created_at DESC`, personID)
		if err != nil {
			return fmt.Errorf("iamdomain: read person %q's sessions: %w",
				personID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row     SessionRecord
				epoch   int64
				start   int64
				created int64
				expires int64
				ended   int64
				doc     []byte
			)
			if err := rows.Scan(&row.Lineage, &row.PersonID, &epoch, &start,
				&created, &expires, &ended, &row.EndedWhy, &doc); err != nil {
				return fmt.Errorf("iamdomain: scan a session: %w", err)
			}
			// A DOCUMENT THIS BUILD CANNOT OPEN lists the row without
			// what only the document holds rather than failing the
			// listing: every other column is the row's own.
			if held, err := DecodeSession(doc); err == nil {
				row.EnrolmentOnly = held.EnrolmentOnly
				row.UserAgent, row.Remote = held.UserAgent, held.Remote
			}
			row.Epoch = uint64(epoch)
			row.Superseded = sessionSuperseded(uint64(start), row.Epoch,
				invalidated, current)
			if row.Superseded && ended == 0 {
				// A COUNTER ENDED IT and the row holds no reason: say
				// which. An epoch move writes its reason on every session
				// it ends ([Applier.bumpEpoch]), so what is left is the
				// company's generation, which writes no row per session,
				// and a session a move ended without saying so on its row
				// (its start landed after that move, or the row predates
				// moves writing reasons), which no later move re-describes.
				switch {
				case uint64(start) < invalidated:
					row.EndedWhy = "ended with every session in the company"
				default:
					row.EndedWhy = "ended with every session they held"
				}
			}
			row.CreatedAt = fromMillis(created)
			row.ExpiresAt = fromMillis(expires)
			row.EndedAt = fromMillis(ended)
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HistoryRow is one entry of the identity estate's own trail.
type HistoryRow struct {
	ID    string
	Class HistoryClass

	// ObjectKind and ObjectID are what the entry is filed under: a record's
	// subject, or for a directory record, whose subject names nobody, the
	// person it is about (`person`) or an invitation's address blind
	// (`email`). A trail column, not an [ObjectKind], so a plain string.
	ObjectKind string
	ObjectID   string
	PersonID   string

	// Login is the login PersonID holds now, or empty for an entry about
	// nobody, or about somebody removed since.
	Login string

	Op        OpKind
	Actor     string
	ActorKind iam.Kind

	// OperatorID is the credential Actor acted through, and empty where
	// the record named none: the node's own writer.
	OperatorID string

	Reason  string
	Summary string
	At      time.Time

	// Version is the log POSITION this entry was derived at, which the read
	// pages on.
	Version uint64
}

// HistoryQuery is one page of the trail. It pages by POSITION, never by time:
// no two nodes' clocks are compared, so a caller holding a timestamp resolves
// it once ([Reader.PositionAt]) and pages from there.
type HistoryQuery struct {
	// Before is the position to page back from, exclusive. Zero starts at
	// the newest entry.
	Before uint64

	// Since is the oldest position to include, inclusive. Zero has no
	// floor.
	Since uint64

	// Person and Op narrow the page. Empty means every one.
	Person string
	Op     OpKind

	Limit int
}

// HistoryPage is one page of the trail and where the next one starts.
type HistoryPage struct {
	Entries []HistoryRow

	// Next is the position to pass as [HistoryQuery.Before] for the
	// following page, zero at the end.
	Next uint64

	// At is what this node had applied when it answered.
	At statelog.Position
}

// historyPageStatement is one page of the trail, newest first, narrowed by q,
// limited to one more than asked as [peoplePageStatement] is.
//
// Each entry joins, by key, the login its person holds now: bare ids tell a
// reader nothing. A removed person's row is gone, so their entries keep only
// the id: the removal erases what identified them.
func historyPageStatement(q HistoryQuery, limit int) (string, []any) {
	query := strings.Builder{}
	query.WriteString(`
		SELECT h.id, h.class, h.object_kind, h.object_id, h.person_id, h.op,
		       h.actor, h.actor_kind, h.operator_id, h.reason, h.summary,
		       h.broker_at, h.version, COALESCE(p.login, '')
		FROM iam_history h LEFT JOIN iam_people p ON p.id = h.person_id
		WHERE 1 = 1`)
	var args []any
	if q.Before > 0 {
		query.WriteString(` AND h.version < ?`)
		args = append(args, int64(q.Before))
	}
	if q.Since > 0 {
		query.WriteString(` AND h.version >= ?`)
		args = append(args, int64(q.Since))
	}
	if q.Person != "" {
		query.WriteString(` AND h.person_id = ?`)
		args = append(args, q.Person)
	}
	if q.Op != "" {
		query.WriteString(` AND h.op = ?`)
		args = append(args, string(q.Op))
	}
	query.WriteString(` ORDER BY h.version DESC LIMIT ?`)
	args = append(args, limit+1)
	return query.String(), args
}

// History answers one page of the identity estate's own trail, NEWEST FIRST:
// an investigation asks "what just happened", which oldest-first would bury
// under years of pages.
func (r *Reader) History(ctx context.Context, q HistoryQuery) (HistoryPage, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultHistoryPage
	case limit > MaxHistoryPage:
		limit = MaxHistoryPage
	}
	out := HistoryPage{At: r.reached()}
	query, args := historyPageStatement(q, limit)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("iamdomain: read the identity trail: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row       HistoryRow
				class     string
				op        string
				actorKind string
				at        int64
				version   int64
			)
			if err := rows.Scan(&row.ID, &class, &row.ObjectKind, &row.ObjectID,
				&row.PersonID, &op, &row.Actor, &actorKind, &row.OperatorID,
				&row.Reason, &row.Summary, &at, &version, &row.Login); err != nil {
				return fmt.Errorf("iamdomain: scan a trail entry: %w", err)
			}
			row.Class = HistoryClass(class)
			row.Op = OpKind(op)
			row.ActorKind = iam.Kind(actorKind)
			row.At = fromMillis(at)
			row.Version = uint64(version)
			out.Entries = append(out.Entries, row)
		}
		return rows.Err()
	})
	if err != nil {
		return HistoryPage{}, err
	}
	if len(out.Entries) > limit {
		out.Next = out.Entries[limit-1].Version
		out.Entries = out.Entries[:limit]
	}
	return out, nil
}

// DefaultHistoryPage and MaxHistoryPage bound a page of the trail. Response
// size bounds both these and the directory's; these are larger because a trail
// entry is a small row joined to one login, where a directory row carries a
// person whole and is opened before it renders.
const (
	DefaultHistoryPage = 100
	MaxHistoryPage     = 1000
)

// positionAtStatement is the newest trail position committed at or before an
// instant: a walk of the trail's position index from its newest row, which
// stops at the first row at or before the instant.
const positionAtStatement = `
	SELECT version FROM iam_history WHERE broker_at <= ?
	ORDER BY version DESC LIMIT 1`

// PositionAt resolves an instant to the newest trail position at or before it,
// once, at the surface, so every later page compares values the applier wrote
// rather than a clock some node read.
func (r *Reader) PositionAt(ctx context.Context, at time.Time) (uint64, error) {
	var version int64
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, positionAtStatement,
			at.UnixMilli()).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			// NOTHING AT OR BEFORE IT IS ZERO, which every page reads
			// as no floor: an instant before the company asks for
			// everything.
			return nil
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("iamdomain: resolve %s to a position: %w",
			at.UTC().Format(time.RFC3339), err)
	}
	return uint64(version), nil
}

// SeatBinding is one person's seat binding: the columns the request path's
// seat table reads (internal/iam/session's PersonRow), the kind, and the
// holder's sealed display name. Its own type, not a [PersonRow] with fields
// left zero, which a later caller would read as a person with no grants.
type SeatBinding struct {
	// Person is the directory id, Login the name the dashboard prints.
	Person, Login string

	// NameSealed is the bound principal's display name, sealed as
	// [PersonRow.NameSealed] (the same column), for a surface naming each
	// seat's holder to everybody: the org chart would otherwise disclose
	// every colleague's sign-in name. The surface opens it on its own
	// keyring, never here; nil where the row carries none.
	NameSealed []byte

	// Kind is a person or a service account, which is what a remedy for a
	// binding names: a person is MOVED to another seat or removed, and a
	// service account may be unbound ([Writer.SetIdentity]).
	Kind iam.Kind

	// Seat is the seat's handle, which is immutable (ADR-0013).
	Seat string

	// Stage is the bound person's stage, from the column.
	Stage iam.Stage
}

// Binding is the row's seat binding.
func (p PersonRow) Binding() SeatBinding {
	return SeatBinding{
		Person: p.ID, Login: p.Login, NameSealed: p.NameSealed, Kind: p.Kind,
		Seat: p.Seat, Stage: p.Stage,
	}
}

// SeatInvitation is one OPEN invitation onto a seat — the person on their way
// to it — as the seat listing and a company write that takes a seat away read
// it: never its link, which is shown once by the issue that made it.
type SeatInvitation struct {
	// Invitation is the invitation's id, which is what cancelling it names.
	Invitation string

	// Seat is the handle of the seat it holds.
	Seat string

	// Sealed is the address it was issued to, sealed as the INVITATION's
	// own ([Sealer.OpenInvitation] opens it with the invitation's id), for
	// a surface entitled to show it.
	Sealed string

	// InvitedBy is who issued it.
	InvitedBy string

	CreatedAt time.Time
	ExpiresAt time.Time
}

// SeatClaims is everybody who holds a seat or is on their way to one: every
// binding and every OPEN invitation ([openInvitation]), read in ONE snapshot.
type SeatClaims struct {
	Bindings    []SeatBinding
	Invitations []SeatInvitation
}

// The two statements [Reader.SeatClaims] reads in its one snapshot: every
// binding, in seat order, and every open invitation, in seat order and the
// newest first on each.
const (
	seatClaimBindingsStatement = `
		SELECT id, kind, login, name_sealed, stage, seat_id
		  FROM iam_people
		 WHERE seat_id != ''
		 ORDER BY seat_id, id`
	openSeatInvitationsStatement = `
		SELECT id, seat_id, email_sealed, invited_by, created_at, expires_at
		  FROM iam_invites
		 WHERE ` + openInvitation + `
		 ORDER BY seat_id, created_at DESC, id`
)

// SeatClaims answers [SeatClaims] at now, the caller's clock, as every expiry
// read here takes it. Both halves come from ONE snapshot: a redemption spends
// its invitation and binds its person in one record, so two snapshots could
// show the seat held by neither (offered to the next administrator) or both.
//
// The bindings walk the people's partial seat index; the open invitations walk
// `iam_invites_open_idx` (0041) and sort, because the seat index would also
// range over redeemed invitations until the sweep collects them a week on.
// Neither decodes a document. An error is the unknown arm and an empty answer
// a real one; [ClaimsBySeat] folds it to one claim per seat.
func (r *Reader) SeatClaims(ctx context.Context, now time.Time) (SeatClaims, error) {
	var out SeatClaims
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		out = SeatClaims{}
		rows, err := tx.QueryContext(ctx, seatClaimBindingsStatement)
		if err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		for rows.Next() {
			var b SeatBinding
			var kind, stage string
			if err = rows.Scan(&b.Person, &kind, &b.Login, &b.NameSealed,
				&stage, &b.Seat); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iamdomain: read a seat binding: %w", err)
			}
			b.Kind, b.Stage = iam.Kind(kind), iam.Stage(stage)
			out.Bindings = append(out.Bindings, b)
		}
		if err = rows.Close(); err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		if err = rows.Err(); err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}

		invites, err := tx.QueryContext(ctx, openSeatInvitationsStatement,
			now.UnixMilli())
		if err != nil {
			return fmt.Errorf("iamdomain: read the open invitations: %w", err)
		}
		for invites.Next() {
			var (
				i                SeatInvitation
				sealed           []byte
				created, expires int64
			)
			if err = invites.Scan(&i.Invitation, &i.Seat, &sealed, &i.InvitedBy,
				&created, &expires); err != nil {
				_ = invites.Close()
				return fmt.Errorf("iamdomain: read an open invitation: %w", err)
			}
			i.Sealed = string(sealed)
			i.CreatedAt, i.ExpiresAt = fromMillis(created), fromMillis(expires)
			out.Invitations = append(out.Invitations, i)
		}
		if err = invites.Close(); err != nil {
			return fmt.Errorf("iamdomain: read the open invitations: %w", err)
		}
		return invites.Err()
	})
	if err != nil {
		return SeatClaims{}, err
	}
	return out, nil
}

// SeatClaim is what one seat is claimed by: its bound principal, the open
// invitation holding it, or both, where an older build left them side by side.
// A surface showing one shows the holder: the invitation's redemption will be
// refused for that seat.
type SeatClaim struct {
	Holder     *SeatBinding
	Invitation *SeatInvitation
}

// ClaimsBySeat folds [SeatClaims] to one [SeatClaim] per seat; a seat nobody
// holds or is invited to is absent.
//
// TWO ROWS BOUND TO ONE SEAT ARE THE UNKNOWN ARM, never a pick: the directory
// decides every binding on one subject, so only a node that retained a record
// moving somebody off the seat and applied a later one binding it (or a
// restore) holds both, and another node must answer. TWO OPEN INVITATIONS ON
// ONE SEAT, the same residue, are not: the newest (the last an administrator
// acted on) is answered and the other stays cancellable by id. An invitation
// routes nobody, and refusing would take down the whole seat listing until
// the older link ages out, a week at most.
func ClaimsBySeat(c SeatClaims) (map[string]SeatClaim, error) {
	out := make(map[string]SeatClaim, len(c.Bindings)+len(c.Invitations))
	for i := range c.Bindings {
		b := &c.Bindings[i]
		claim := out[b.Seat]
		if claim.Holder != nil {
			return nil, fmt.Errorf("%w: this node has two people on seat %s "+
				"(%s and %s). Try again on another node",
				statelog.ErrUnavailable,
				b.Seat, claim.Holder.Person, b.Person)
		}
		claim.Holder = b
		out[b.Seat] = claim
	}
	for i := range c.Invitations {
		inv := &c.Invitations[i]
		claim := out[inv.Seat]
		if claim.Invitation != nil && !newerInvitation(*inv, *claim.Invitation) {
			continue
		}
		claim.Invitation = inv
		out[inv.Seat] = claim
	}
	return out, nil
}

// newerInvitation reports whether a was issued after b, its id breaking a tie
// so two nodes folding one answer pick one invitation.
func newerInvitation(a, b SeatInvitation) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.Invitation > b.Invitation
}
