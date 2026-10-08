package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DIRECTORY READS: what an administrator, an auditor and a person looking
// at their own row actually ask for.
//
// read.go holds the lookups the REQUEST PATH takes — one person, one session,
// resolved before anything can be decided — and they are deliberately narrow:
// a sign-in may not become a roster. These are the opposite kind of read. They
// are asked by somebody who has already been authorised to see the directory,
// they are pages rather than keyed lookups, and what they answer is "who can
// reach this company, and how".
//
// # Everything here pages on a key the applier writes
//
// A person's id is a uuid7, so ordering by it IS creation order and a cursor
// is the last id of the previous page. Nothing sorts on a value this domain
// seals — a name and an address are ciphertext in every row, so ordering by
// one would be ordering by the ciphertext, which is a stable order that means
// nothing to a reader.
//
// # And nothing here opens anything
//
// Opening a sealed name is the CALLER's to do: [PersonRow] carries the
// ciphertext, and the surface opens what it is about to show somebody. A
// reader that opened eagerly would put every name and address in the clear in
// the memory of every read that only needed to know who may do what.

// PersonRow is one directory row as an administrator reads it.
//
// ITS OWN TYPE beside [Sighting], which is what a SIGN-IN resolves: that one
// is bounded by what a refusal may disclose, and this one is the answer to
// somebody already entitled to the whole directory. Folding them would mean
// the sign-in path carrying fields it must never read.
type PersonRow struct {
	ID    string
	Kind  iam.Kind
	Stage iam.Stage

	// Login is in the CLEAR, unlike the name and the address. Blinding a
	// value the dashboard prints on every row would cost an operator
	// their own audit trail for nothing.
	Login string

	// NameSealed and EmailSealed are ciphertext, under the fleet keyring.
	// There is no row for somebody removed — a removal deletes it and
	// leaves the tombstone in `iam_removed` — so a value that will not open
	// is a key this node's ring does not hold: dropped before the value
	// was moved off it, or a restore under a different keyring.
	NameSealed  []byte
	EmailSealed []byte

	// Seat is the handle of the seat this person is bound to, which is
	// immutable (ADR-0013). A person always holds one ([Writer.Create],
	// ADR-0026); empty on a person only where they were recorded before
	// every person did ([PersonRow.Seatless]), and on a service account
	// that acts as itself.
	Seat string

	Grants []iam.Grant

	// Epoch is the person's revocation epoch: every session and token
	// they hold carries the value it was minted at, and anything below
	// the current one is over.
	Epoch uint64

	CreatedAt time.Time
	UpdatedAt time.Time

	// Version is the iam position that last wrote this row.
	Version uint64
}

// Seatless reports a PERSON who holds no seat: one recorded before every person
// held a human seat (ADR-0026), which nothing this build writes can produce.
// A service account is never seatless in this sense — its seat is optional by
// design — so the predicate is the one rule a report about the former asks,
// stated where the row is rather than at each of its readers.
func (p PersonRow) Seatless() bool {
	return p.Kind == iam.KindPerson && p.Seat == ""
}

// PeopleQuery is one page of the directory.
type PeopleQuery struct {
	// After is the last id of the previous page, exclusive. Empty starts
	// at the beginning.
	After string

	// Limit is how many rows at most. Zero takes [DefaultPageSize] and
	// anything above [MaxPageSize] is clamped rather than refused — a
	// caller asking for a thousand wants "lots", and a refusal there
	// sends them to write a loop that asks for a thousand pages of one.
	Limit int

	// Q narrows on the LOGIN and the SEAT, which are the only two
	// identity values this estate holds in the clear. A name and an
	// address are sealed in every row, so a search over them would have
	// to open every person in the company to compare one — which is a
	// fan-out of coordination reads an operator typing in a box would
	// trigger per keystroke.
	Q string

	// Stage narrows to one enrolment stage: the people who may act, or
	// the abandoned enrolments. Empty takes every stage.
	Stage iam.Stage
}

// DefaultPageSize and MaxPageSize bound a directory page.
//
// FIFTY AND TWO HUNDRED, and the numbers come from what the caller does with
// a row rather than from the SQL: a surface renders every row it is given,
// opening its name and address, and a directory row is the largest answer
// this estate serves — a name, an address, a seat, a grant list, a stage and
// an epoch. Fifty is a screen of people; two hundred is the point past which
// an operator is better served by narrowing (`q=`, `stage=`) than by paging a
// response the size of the whole company.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// pageLimit is how many rows one directory page holds: [DefaultPageSize] for a
// caller that named none, and at most [MaxPageSize] — clamped, never refused.
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

// People answers one page of the directory.
func (r *Reader) People(ctx context.Context, q PeopleQuery) (PeoplePage, error) {
	limit := pageLimit(q.Limit)
	out := PeoplePage{At: r.committed()}
	// ONE MORE THAN ASKED FOR, which is how the cursor is derived without
	// a second count: the extra row is what says there IS a next page, and
	// it is dropped rather than returned.
	query := strings.Builder{}
	query.WriteString(`
		SELECT p.id, p.kind, p.stage, p.login, p.name_sealed, p.email_sealed,
		       p.seat_id, p.document,
		       p.created_at, p.updated_at, p.version,
		       COALESCE(e.epoch, 0)
		FROM iam_people p
		LEFT JOIN iam_revocation_epochs e ON e.person_id = p.id
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

	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query.String(), args...)
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

// Person answers one directory row, or [ErrNotFound].
//
// THE SENTINEL IS DELIBERATE HERE, unlike [Reader.PersonByLogin]'s zero
// value: this read runs for a caller the guard has already authorised, so
// there is no enumeration to protect — and a surface needs 404 and 200 to be
// different answers.
func (r *Reader) Person(ctx context.Context, id string) (PersonRow, error) {
	var out PersonRow
	found := false
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.id, p.kind, p.stage, p.login, p.name_sealed, p.email_sealed,
			       p.seat_id, p.document,
			       p.created_at, p.updated_at, p.version,
			       COALESCE(e.epoch, 0)
			FROM iam_people p
			LEFT JOIN iam_revocation_epochs e ON e.person_id = p.id
			WHERE p.id = ?`, id)
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

// escapeLike makes a caller's term a literal in a LIKE pattern.
//
// A SEARCH BOX IS NOT A PATTERN LANGUAGE. Without this a `%` typed into it
// matches the whole directory and a `_` matches any character, so a narrowing
// an operator believes they applied quietly does not apply.
func escapeLike(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(term)
}

// CredentialRow is one credential as the directory reports it.
//
// NO VERIFIER. What a row carries here is what somebody DECIDES with — which
// method, when it was minted, when it expires, whether it was revoked — and
// the verifier is the one field that must never leave the estate: an argon2id
// digest and a token hash are both offline-attackable, and a listing is the
// surface most likely to be copied into a ticket.
type CredentialRow struct {
	ID        string
	PersonID  string
	Method    CredentialMethod
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time

	// Label is what the mint called it, for a person who holds four
	// tokens and needs to know which is which.
	Label string

	// Grants are what a machine token was minted carrying — the ceiling on
	// what it can do, re-cut to its owner's own grants on every request.
	// Empty on every other method.
	Grants []iam.Grant

	// Superseded marks a machine token a COUNTER ended rather than its own
	// row: its owner's revocation epoch, or the company's session
	// generation, moved past the value it was minted at — the comparison
	// [credential.CheckToken] refuses it on. Neither writes `revoked_at`: a
	// password change, "sign out everywhere" and an administrator ending
	// somebody's sessions all move the epoch, so a listing that read the
	// row alone reported every token they ended as in use.
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

// Credentials lists what one person proves themselves with.
//
// THE COUNTERS ARE READ IN THE SNAPSHOT THE ROWS ARE, so a token is reported
// ended by exactly what ended it at that instant ([CredentialRow.Superseded]).
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
	// After is the last id of the previous page, exclusive; Limit is
	// [PeopleQuery.Limit]'s. An invitation's id is a uuid7 at its issue's
	// instant ([Blinder.InvitationID]), so id order is issue order.
	After string
	Limit int

	// All includes the invitations that no longer open — redeemed, or
	// past their expiry — which the estate holds until the sweep collects
	// them. Without it a page is the OPEN ones alone, judged at Now.
	All bool
	Now time.Time
}

// InvitationPage is one page of invitations and where the next one starts.
type InvitationPage struct {
	Invitations []InvitationRow
	Next        string
	At          statelog.Position
}

// Invitations answers one page of the invitations this estate holds.
//
// THE ROW CARRIES THE VERIFIER, as [Reader.InvitationByID]'s does, and a
// surface renders none of it: what a listing shows is who was invited, to
// what, by whom and until when — never the link, which is shown once by the
// issue that made it.
func (r *Reader) Invitations(ctx context.Context, q InvitationsQuery) (
	InvitationPage, error) {

	limit := pageLimit(q.Limit)
	out := InvitationPage{At: r.committed()}
	query := `SELECT ` + invitationColumns + ` FROM iam_invites WHERE id > ?`
	args := []any{q.After}
	if !q.All {
		// OPEN IS [InvitationRow.Spent]'s complement — not redeemed, a
		// deadline still ahead, a seat named — and the very predicate a
		// directory decide holds a value against ([openInvitation]), so
		// the listing shows exactly the invitations that hold something.
		query += ` AND ` + openInvitation
		args = append(args, q.Now.UnixMilli())
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit+1)
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
// NO BEARER AND NOTHING DERIVED FROM ONE. A lineage is not a secret — it is
// in every audit row — but the cookie is, and a listing that carried anything
// a session could be resumed from would make this surface worth attacking.
type SessionRecord struct {
	Lineage   string
	PersonID  string
	Epoch     uint64
	CreatedAt time.Time
	ExpiresAt time.Time
	EndedAt   time.Time
	EndedWhy  string

	// EnrolmentOnly is a session that may only enrol a second factor — see
	// [Session.EnrolmentOnly] — which an administrator reading somebody's
	// sessions needs told apart from a whole one.
	EnrolmentOnly bool

	// Superseded marks a session a COUNTER ended: opened before the
	// company's last invalidation, or at a revocation epoch its person has
	// since moved past ([sessionSuperseded]). The company's invalidation
	// writes no `ended_at`, and neither does an epoch move on a session
	// whose start landed after it or that an estate held before moves wrote
	// their reasons, so a listing that read the row alone reported those
	// live.
	Superseded bool
}

// Live reports a session nothing has ended: not ended, not past its absolute
// deadline, and not superseded.
//
// THE ONE READING: the listing reports it and [Reader.SessionStanding] decides
// a named sign-out by it, so a screen never offers to end a session the engine
// would answer was already over.
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

// Sessions lists one person's sessions, newest first.
//
// ENDED ONES INCLUDED, because "this session was revoked, and why" is the
// sentence an investigation is looking for and a listing that showed only the
// live ones could never carry it. The row is kept until the sweep
// collects it for exactly that reason.
//
// THE COUNTERS ARE READ IN THE SNAPSHOT THE ROWS ARE, so a session is
// reported ended by exactly what ended it at that instant
// ([SessionRecord.Superseded]).
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
			// A DOCUMENT THIS BUILD CANNOT OPEN is listed without the
			// flag rather than failing the listing: every other column
			// is the row's own, and this is the one fact that lives only
			// in the document.
			if held, err := DecodeSession(doc); err == nil {
				row.EnrolmentOnly = held.EnrolmentOnly
			}
			row.Epoch = uint64(epoch)
			row.Superseded = sessionSuperseded(uint64(start), row.Epoch,
				invalidated, current)
			if row.Superseded && ended == 0 {
				// A COUNTER ENDED IT and the row holds no reason: say
				// which, or the listing shows an ended session with no
				// account of why — beside the deadline it never reached.
				// An epoch move writes its own reason on every session it
				// ends ([Applier.bumpEpoch]), so what is left here is the
				// company's generation, which ends everybody's at once and
				// writes no row per session, and a session a move had
				// already ended without saying so on its row — its start
				// landed after that move, or the row predates moves writing
				// their reasons — which no later move re-describes.
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
	// subject, or — for a directory record, whose subject names nobody — the
	// person it is about (`person`) or an invitation's address blind
	// (`email`). A trail column rather than a subject kind, so a plain
	// string: `email` is no [ObjectKind] any more.
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

	// Version is the log POSITION this entry was derived at, which is
	// what the read pages on.
	Version uint64
}

// HistoryQuery is one page of the trail.
//
// IT PAGES BY POSITION AND NEVER BY TIME, because two nodes' clocks are
// compared nowhere in this engine: a caller holding a timestamp resolves it
// to a position once, against the rows' own instants, and pages from there.
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

// History answers one page of the identity estate's own trail.
//
// NEWEST FIRST, which is what somebody investigating actually reads: the
// question is almost always "what just happened", and a reader that started
// at the beginning would page through years to reach it.
func (r *Reader) History(ctx context.Context, q HistoryQuery) (HistoryPage, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultHistoryPage
	case limit > MaxHistoryPage:
		limit = MaxHistoryPage
	}
	out := HistoryPage{At: r.committed()}
	query := strings.Builder{}
	// THE LOGIN OF WHOEVER AN ENTRY IS ABOUT, joined from their row as it
	// stands: an entry names them by id, and a trail of id prefixes said
	// whose session opened and whose row changed to nobody reading it. A
	// removed person's row is gone, so their entries name nobody — the
	// removal erases what identified them, and the trail keeps the id.
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

	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query.String(), args...)
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

// DefaultHistoryPage and MaxHistoryPage bound a page of the trail.
//
// A HUNDRED AND A THOUSAND, and they are larger than the directory's for one
// reason: a trail entry is a small ROW, opened from nothing and joined to one
// login, while a directory row carries a person whole and is opened before
// anybody can render it. What bounds both is the response size.
const (
	DefaultHistoryPage = 100
	MaxHistoryPage     = 1000
)

// PositionAt resolves an instant to the newest trail position at or before it.
//
// ONCE, AT THE SURFACE, which is what lets everything else page by POSITION:
// a caller holding a timestamp gets a number, and every page after that is
// compared against values the applier wrote rather than against a clock some
// node read for itself.
func (r *Reader) PositionAt(ctx context.Context, at time.Time) (uint64, error) {
	var version int64
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT version FROM iam_history WHERE broker_at <= ?
			ORDER BY version DESC LIMIT 1`, at.UnixMilli()).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			// NOTHING AT OR BEFORE IT IS ZERO, which is the floor
			// every page already reads as "no floor" — an instant
			// before this company existed asks for everything, which
			// is exactly what it means.
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

// SeatBinding is one person's seat binding — the columns the request path's
// seat table reads (internal/iam/session's PersonRow), the kind beside them,
// and nothing sealed.
//
// ITS OWN TYPE rather than a [PersonRow] with most fields left zero: a row
// type whose document, grants and sealed values were silently absent would be
// one a later caller read as a person with no grants.
type SeatBinding struct {
	// Person is the directory id, Login the name the dashboard prints.
	Person, Login string

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
		Person: p.ID, Login: p.Login, Kind: p.Kind, Seat: p.Seat, Stage: p.Stage,
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

// SeatClaims answers [SeatClaims] at now — the instant an invitation is open
// at, the caller's clock as every reader of an expiry here takes it.
//
// # One snapshot, because the two halves move together
//
// A redemption spends its invitation and binds its person in ONE record, so
// read in two snapshots a redemption landing between them showed the seat as
// held by neither — vacant, and offered to the next administrator to fill —
// or by both. One read of both tables is the only shape in which every
// answer is a state the directory was actually in.
//
// # What it costs
//
// The bindings are a walk of the people's partial seat index, bound rows only,
// in seat order; the open invitations a walk of the invitations' OPEN index
// (0031's `iam_invites_open_idx`, unredeemed rows only) and a sort of what it
// finds. That index rather than the seat's: a range of the seat index above
// the empty seat is every invitation that ever named a seat, the redeemed ones
// included until the sweep collects them a week on, where the open index is
// only the invitations still outstanding — the set this answers. Neither
// decodes a document.
//
// Three-valued like everything here: an error is the unknown arm, and an
// empty answer is a real one ("nobody holds or is invited to any seat").
// [ClaimsBySeat] folds it to one claim per seat.
func (r *Reader) SeatClaims(ctx context.Context, now time.Time) (SeatClaims, error) {
	var out SeatClaims
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		out = SeatClaims{}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, kind, login, stage, seat_id
			  FROM iam_people
			 WHERE seat_id != ''
			 ORDER BY seat_id, id`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		for rows.Next() {
			var b SeatBinding
			var kind, stage string
			if err = rows.Scan(&b.Person, &kind, &b.Login, &stage,
				&b.Seat); err != nil {
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

		invites, err := tx.QueryContext(ctx, `
			SELECT id, seat_id, email_sealed, invited_by, created_at, expires_at
			  FROM iam_invites
			 WHERE `+openInvitation+`
			 ORDER BY seat_id, created_at DESC, id`, now.UnixMilli())
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

// SeatClaim is what one seat is claimed by: the principal bound to it, the
// open invitation holding it, or — only where a build before invitations held
// their seats left the two side by side — both. A surface rendering one answer
// takes the holder: the invitation's redemption will be refused for the seat
// they hold.
type SeatClaim struct {
	Holder     *SeatBinding
	Invitation *SeatInvitation
}

// ClaimsBySeat folds [SeatClaims] to one [SeatClaim] per seat; a seat nobody
// holds or is invited to is absent.
//
// TWO ROWS BOUND TO ONE SEAT ARE THE UNKNOWN ARM, never a pick. The directory
// decides every binding on one subject in one snapshot, so ordinary traffic
// never produces two; a node that retained the record moving somebody off a
// seat, and applied a later one binding it to somebody else, holds both until
// it reprocesses the first — as a restore can. That node cannot say who holds
// the seat, so another node answers.
//
// TWO OPEN INVITATIONS ON ONE SEAT ARE NOT. Invitations issued before every
// invitation held its seat may name one seat twice, and both stay open until
// they age out — a week at most; the newest is the one an administrator acted
// on last and the one this answers, and the other stays cancellable by its id
// from the invitation listing. Refusing the whole answer over them would take
// the seat listing down for every seat, for a week, over a residue nothing
// this build writes.
func ClaimsBySeat(c SeatClaims) (map[string]SeatClaim, error) {
	out := make(map[string]SeatClaim, len(c.Bindings)+len(c.Invitations))
	for i := range c.Bindings {
		b := &c.Bindings[i]
		claim := out[b.Seat]
		if claim.Holder != nil {
			return nil, fmt.Errorf("%w: this node holds two rows bound to seat "+
				"%s (%s and %s), which only a record it retained or a restore "+
				"leaves behind — another node can answer", statelog.ErrUnavailable,
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
