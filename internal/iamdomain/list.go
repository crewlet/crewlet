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
// Opening a sealed name costs a fleet-secret read per person and the key may
// be gone. Both are the CALLER's to deal with: [PersonRow] carries the
// ciphertext and whether this deployment can still open it, and the surface
// decides how many rows it is willing to pay for. A reader that opened
// eagerly would make every listing a fan-out of coordination reads with no
// bound anybody could see.

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

	// NameSealed and EmailSealed are ciphertext. Shredded reports that
	// this person's key was destroyed by a removal, which is what tells
	// "sealed, and I can open it" from "sealed, and nobody ever will
	// again" without attempting a decrypt and reading the failure as an
	// outage.
	NameSealed  []byte
	EmailSealed []byte
	Shredded    bool

	// Seat is the chart handle this person is bound to, and SeatAt the
	// chart position the bind was decided at.
	Seat   string
	SeatAt uint64

	Grants    []iam.Grant
	Colleague iam.Colleague

	// Epoch is the person's revocation epoch: every session and token
	// they hold carries the value it was minted at, and anything below
	// the current one is over.
	Epoch uint64

	CreatedAt time.Time
	UpdatedAt time.Time

	// Version is the iam position that last wrote this row.
	Version uint64

	// Reserved reports a RESERVATION: an enrolment whose claims landed and
	// whose content record has not — see [Sighting.Reserved]. It carries
	// the claimed columns and nothing a person is: no kind, no stage, no
	// grants, and nothing sealed that the content record would have
	// placed.
	//
	// LISTED RATHER THAN HIDDEN, because it holds an address, a login or
	// a seat nobody else can take, and an administrator whose enrolment
	// was refused as "claimed" has to be able to find what claimed it.
	// Decoded as a person, it failed the whole directory page instead.
	Reserved bool
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
// a row rather than from the SQL. A surface renders a person by OPENING their
// sealed name and address, which is one fleet-secret read each — deliberately
// uncached, because a cache would go on opening values for somebody whose key
// a removal has just destroyed. So a page is a fan-out of that many
// coordination reads: fifty is about one round trip's worth of latency for a
// screen, and two hundred is the point past which an operator is better served
// by narrowing than by waiting.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

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
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	out := PeoplePage{At: r.committed()}
	// ONE MORE THAN ASKED FOR, which is how the cursor is derived without
	// a second count: the extra row is what says there IS a next page, and
	// it is dropped rather than returned.
	query := strings.Builder{}
	query.WriteString(`
		SELECT p.id, p.kind, p.stage, p.login, p.name_sealed, p.email_sealed,
		       p.shredded, p.seat_id, p.chart_position, p.document,
		       p.created_at, p.updated_at, MAX(p.version, p.scoped_through),
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
			       p.shredded, p.seat_id, p.chart_position, p.document,
			       p.created_at, p.updated_at, MAX(p.version, p.scoped_through),
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
		shredded int
		created  int64
		updated  int64
		version  int64
		epoch    int64
	)
	if err := rows.Scan(&out.ID, &kind, &stage, &out.Login, &out.NameSealed,
		&out.EmailSealed, &shredded, &out.Seat, &out.SeatAt, &document,
		&created, &updated, &version, &epoch); err != nil {
		return PersonRow{}, fmt.Errorf("iamdomain: scan a directory row: %w", err)
	}
	out.Kind = iam.Kind(kind)
	out.Stage = iam.Stage(stage)
	out.Shredded = shredded != 0
	out.Epoch = uint64(epoch)
	if reservation(kind) {
		out.Reserved = true
	} else {
		person, err := DecodePerson(document)
		if err != nil {
			return PersonRow{}, fmt.Errorf("iamdomain: open person %q: %w",
				out.ID, err)
		}
		out.Grants = person.Grants
		out.Colleague = person.Colleague
	}
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

	// Grants and Colleague are what a machine token was minted carrying —
	// the ceiling on what it can do, re-cut to its owner's own grants on
	// every request. Empty on every other method.
	Grants    []iam.Grant
	Colleague iam.Colleague
}

// Revoked reports a credential that has been withdrawn or has aged out.
func (c CredentialRow) Revoked(now time.Time) bool {
	return !c.RevokedAt.IsZero() ||
		(!c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt))
}

// Credentials lists what one person proves themselves with.
func (r *Reader) Credentials(ctx context.Context, personID string) (
	[]CredentialRow, error) {

	var out []CredentialRow
	err := r.withTx(ctx, func(tx *sql.Tx) error {
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
				row.Grants, row.Colleague = held.Grants, held.Colleague
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
}

// Live reports a session that has not ended and has not aged out.
func (s SessionRecord) Live(now time.Time) bool {
	return s.EndedAt.IsZero() &&
		(s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt))
}

// Sessions lists one person's sessions, newest first.
//
// ENDED ONES INCLUDED, because "this session was ended by reuse detection" is
// the sentence an investigation is looking for and a listing that showed only
// the live ones could never carry it. The row is kept until the sweep
// collects it for exactly that reason.
func (r *Reader) Sessions(ctx context.Context, personID string) (
	[]SessionRecord, error) {

	var out []SessionRecord
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT lineage, person_id, epoch, created_at,
			       absolute_expires_at, ended_at, ended_reason
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
				created int64
				expires int64
				ended   int64
			)
			if err := rows.Scan(&row.Lineage, &row.PersonID, &epoch, &created,
				&expires, &ended, &row.EndedWhy); err != nil {
				return fmt.Errorf("iamdomain: scan a session: %w", err)
			}
			row.Epoch = uint64(epoch)
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
	ID         string
	Class      HistoryClass
	ObjectKind ObjectKind
	ObjectID   string
	PersonID   string
	Op         OpKind
	Actor      string
	ActorKind  iam.Kind
	Reason     string
	Summary    string
	At         time.Time

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
	query.WriteString(`
		SELECT id, class, object_kind, object_id, person_id, op, actor,
		       actor_kind, reason, summary, broker_at, version
		FROM iam_history WHERE 1 = 1`)
	var args []any
	if q.Before > 0 {
		query.WriteString(` AND version < ?`)
		args = append(args, int64(q.Before))
	}
	if q.Since > 0 {
		query.WriteString(` AND version >= ?`)
		args = append(args, int64(q.Since))
	}
	if q.Person != "" {
		query.WriteString(` AND person_id = ?`)
		args = append(args, q.Person)
	}
	if q.Op != "" {
		query.WriteString(` AND op = ?`)
		args = append(args, string(q.Op))
	}
	query.WriteString(` ORDER BY version DESC LIMIT ?`)
	args = append(args, limit+1)

	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query.String(), args...)
		if err != nil {
			return fmt.Errorf("iamdomain: read the identity trail: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row        HistoryRow
				class      string
				objectKind string
				op         string
				actorKind  string
				at         int64
				version    int64
			)
			if err := rows.Scan(&row.ID, &class, &objectKind, &row.ObjectID,
				&row.PersonID, &op, &row.Actor, &actorKind, &row.Reason,
				&row.Summary, &at, &version); err != nil {
				return fmt.Errorf("iamdomain: scan a trail entry: %w", err)
			}
			row.Class = HistoryClass(class)
			row.ObjectKind = ObjectKind(objectKind)
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
// reason: a trail entry is a ROW, opened from nothing and joined to nothing,
// while a directory row costs a fleet-secret read per person before anybody
// can render it. What bounds this one is the response size rather than a
// fan-out.
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

// BootstrapCode is one outstanding way into a company that has nobody in it.
type BootstrapCode struct {
	ID        string
	MintedBy  string
	ExpiresAt time.Time

	// MintedAt is the broker's instant for the record that minted it —
	// identical on every node, which is what lets the person a redemption
	// creates be derived from it ([BootstrappedPersonID]).
	MintedAt time.Time
}

// OutstandingBootstrapCodes are the codes that are neither spent nor aged out.
//
// WHAT RE-ISSUING HAS TO WITHDRAW. A live code is a way to become the
// company's first administrator with no credential at all, so an operator who
// re-issues because they lost the file must not leave the original working in
// somebody's terminal.
//
// THE CLOCK IS THE CALLER'S, which is safe in the one direction that matters:
// a writer whose clock is behind withdraws a code that had already aged out,
// which costs one extra record and changes nothing.
func (r *Reader) OutstandingBootstrapCodes(ctx context.Context, now time.Time) (
	[]BootstrapCode, error) {

	var out []BootstrapCode
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, minted_by, expires_at, created_at FROM iam_bootstrap_codes
			WHERE redeemed_at = 0 AND expires_at > ?
			ORDER BY created_at`, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("iamdomain: read the outstanding bootstrap "+
				"codes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row              BootstrapCode
				expires, created int64
			)
			if err := rows.Scan(&row.ID, &row.MintedBy, &expires,
				&created); err != nil {
				return fmt.Errorf("iamdomain: scan a bootstrap code: %w", err)
			}
			row.ExpiresAt = fromMillis(expires)
			row.MintedAt = fromMillis(created)
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
