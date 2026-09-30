package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MOVING SOMEBODY'S VALUES ONTO THE KEYRING'S ACTIVE KEY.
//
// A keyring rotation adds a key, makes it active, re-seals what the old key
// sealed, and then drops the old one. Everything the keyring seals in the
// company's secret store is re-sealed in place by `crewlet secrets rekey`;
// what it seals in THIS estate cannot be, because these rows are derived from
// the log and a node that rewrote its own would hold different bytes from its
// peers — and a replay from the floor would write the old ciphertext back. So
// a person's values move the only way anything here changes: a RECORD, one
// per person holding a value under a key other than the active one, which is
// an ordinary content update ([Writer.UpdatePerson]) whose document carries the
// same name, address and seeds sealed afresh. Every node applies it and holds
// the new ciphertext; the old key can then be dropped without a single value
// in any row becoming unreadable.
//
// # What is not moved, and why that is enough
//
// An OUTSTANDING INVITATION's address is not re-sealed: the only record that
// could carry it again is a re-issue, which the address's own claim refuses
// while the invitation is open, and it stops being redeemable when it expires
// anyway. So it is COUNTED ([SealedCount.Invitations]) and a rotation waits
// for it to be redeemed or to lapse before dropping the old key, exactly as it
// waits out the session lifetime for the cookies the old key signed. One that
// has lapsed is not counted: nothing opens its address again.
//
// A RESERVATION — the claims of an enrolment that has not finished — is not
// moved or counted: it has no document for a record to carry, and nothing
// opens its address. The enrolment's retry writes the address afresh under
// the active key, and one that never finishes is what `/iam/check` names as an
// orphaned reservation.
//
// THE TRAIL is not moved either: an authentication-trail row carries the
// record that wrote it, and nothing ever opens the payload inside — the trail
// is read for who did what, which is in the clear. A trail row sealed under a
// dropped key is a trail row exactly as useful as before.

// SealedCount is how many of the values this estate opens are sealed under
// each keyring key — what a rotation has to move before the old key goes.
type SealedCount struct {
	// People counts every person's name, address and second-factor seed, by
	// the key each is sealed under.
	People map[string]int `json:"people,omitempty"`

	// Invitations counts the addresses of invitations nobody has redeemed
	// and that have not lapsed, by key: not re-sealed, and waited out.
	Invitations map[string]int `json:"invitations,omitempty"`
}

// Outside is how many of the people's values are sealed under a key other
// than active — what a rekey would move.
func (c SealedCount) Outside(active string) int { return outside(c.People, active) }

// InvitationsOutside is how many live invitations' addresses are sealed under
// a key other than active — what a rotation waits out rather than moves.
func (c SealedCount) InvitationsOutside(active string) int {
	return outside(c.Invitations, active)
}

// outside sums a per-key count over every key but active.
func outside(byKey map[string]int, active string) int {
	n := 0
	for key, count := range byKey {
		if key != active {
			n += count
		}
	}
	return n
}

// SealedKeys counts the sealed values this node's rows hold that anything
// opens, by the keyring key each is sealed under, without opening one; now is
// what an invitation's expiry is judged against.
//
// THE KEY ID IS READ OFF THE ENVELOPE ([secrets.EnvelopeKeyID]), which is the
// one parse of that format: what a rotation has to move must be countable on a
// node that could not open a single value.
func (r *Reader) SealedKeys(ctx context.Context, now time.Time) (SealedCount, error) {
	out := SealedCount{People: map[string]int{}, Invitations: map[string]int{}}
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			what, sql string
			into      map[string]int
		}{
			{"names and addresses", `
				SELECT name_sealed FROM iam_people
				WHERE kind <> '' AND length(name_sealed) > 0
				UNION ALL
				SELECT email_sealed FROM iam_people
				WHERE kind <> '' AND length(email_sealed) > 0`,
				out.People},
			{"second factors", `
				SELECT verifier FROM iam_credentials WHERE method = 'totp'`,
				out.People},
			{"invitations", `
				SELECT email_sealed FROM iam_invites
				WHERE redeemed_at = 0 AND expires_at > ? AND length(email_sealed) > 0`,
				out.Invitations},
		} {
			var args []any
			if q.what == "invitations" {
				args = append(args, millis(now))
			}
			if err := countKeys(ctx, tx, q.sql, q.into, args...); err != nil {
				return fmt.Errorf("iamdomain: count the sealed %s: %w", q.what, err)
			}
		}
		return nil
	})
	return out, err
}

// countKeys adds every envelope one query answers to a count by key id.
func countKeys(ctx context.Context, tx *sql.Tx, query string, into map[string]int,
	args ...any) error {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sealed []byte
		if err := rows.Scan(&sealed); err != nil {
			return err
		}
		if key, ok := secrets.EnvelopeKeyID(string(sealed)); ok {
			into[key]++
		}
	}
	return rows.Err()
}

// ResealReport is what one pass over the estate moved.
type ResealReport struct {
	// People are the people whose values a record re-sealed, and Values how
	// many values those records moved.
	People []string
	Values int

	// Unknown are the people whose record nobody could confirm: run the
	// pass again, which re-seals what did not land and finds nothing to do
	// for what did.
	Unknown []string
}

// Reseal moves every person's values onto the keyring's active key, one
// record per person who holds a value under another.
//
// IT IS ITS OWN RETRY. Each person's record is decided in their own snapshot
// against what their row holds NOW, so a pass run again after one that did not
// finish re-seals only what is still under an old key, and a pass over an
// estate already moved publishes nothing. That is also why each record takes a
// fresh operation id rather than one derived from the person: a later rotation
// moving the same person again is a new operation, which a derived id would
// have collapsed into the old one's.
//
// A PERSON WHO FAILS DOES NOT STOP THE REST: they are independent people, and
// one unconfirmed record must not leave everybody after them on a key that is
// about to be dropped. Every failure is returned with the report of what
// moved.
func (w *Writer) Reseal(ctx context.Context, reader *Reader) (ResealReport, error) {
	var report ResealReport
	if w.sealer == nil {
		return report, errors.New("iamdomain: this node holds no keyring to " +
			"re-seal anybody's values with")
	}
	people, err := reader.sealedOutside(ctx, w.sealer.active)
	if err != nil {
		return report, err
	}
	var errs []error
	for _, id := range people {
		if ctx.Err() != nil {
			return report, errors.Join(append(errs, ctx.Err())...)
		}
		moved := 0
		result, err := w.UpdatePerson(ctx, PersonUpdate{
			PersonID: id, OpID: statelog.NewOpID(w.Now(), "reseal"),
			Reason: "re-sealed under keyring key " + w.sealer.active,
			Apply: func(p Person) (Person, error) {
				// THE LAST RUN'S COUNT, which is the one the landed
				// record carries: a decide may run again against a
				// fresh snapshot.
				next, n, err := w.sealer.reseal(id, p)
				moved = n
				if err == nil && n == 0 {
					return p, errNothingToPublish
				}
				return next, err
			},
		})
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("iamdomain: re-seal person %s: %w", id, err))
		case result.Outcome == statelog.OutcomeUnknown:
			report.Unknown = append(report.Unknown, id)
		case moved > 0:
			report.People = append(report.People, id)
			report.Values += moved
		}
	}
	return report, errors.Join(errs...)
}

// sealedOutside is every person holding a value — a name, an address, a
// second factor's seed — sealed under a key other than active, in id order.
func (r *Reader) sealedOutside(ctx context.Context, active string) ([]string, error) {
	stale := map[string]bool{}
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		for _, query := range []string{
			`SELECT id, name_sealed FROM iam_people
			 WHERE kind <> '' AND length(name_sealed) > 0`,
			`SELECT id, email_sealed FROM iam_people
			 WHERE kind <> '' AND length(email_sealed) > 0`,
			`SELECT person_id, verifier FROM iam_credentials WHERE method = 'totp'`,
		} {
			rows, err := tx.QueryContext(ctx, query)
			if err != nil {
				return fmt.Errorf("iamdomain: find the values sealed under an "+
					"old key: %w", err)
			}
			for rows.Next() {
				var (
					id     string
					sealed []byte
				)
				if err := rows.Scan(&id, &sealed); err != nil {
					rows.Close()
					return fmt.Errorf("iamdomain: scan a sealed value: %w", err)
				}
				if key, ok := secrets.EnvelopeKeyID(string(sealed)); ok && key != active {
					stale[id] = true
				}
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(stale)), nil
}

// reseal is one person's document with every value sealed under a key other
// than the active one opened and sealed again, and how many it moved.
//
// A VALUE THIS KEYRING CANNOT OPEN REFUSES THE WHOLE PERSON rather than being
// skipped: it is a key already dropped from the ring, and a record that moved
// the rest would report the person moved while one of their values stays
// unreadable for ever — which is the state the operator is about to drop the
// old key on the strength of. internal/fleetsecrets' rekey refuses the same
// way, for the same reason.
func (s *Sealer) reseal(personID string, p Person) (Person, int, error) {
	moved := 0
	move := func(sealed string, open func(string) (string, error),
		seal func(string) (string, error)) (string, error) {

		key, ok := secrets.EnvelopeKeyID(sealed)
		if !ok || key == s.active {
			return sealed, nil
		}
		plain, err := open(sealed)
		if err != nil {
			return "", err
		}
		moved++
		return seal(plain)
	}
	var err error
	for _, field := range []struct {
		field Field
		value *string
	}{{FieldName, &p.NameSealed}, {FieldEmail, &p.EmailSealed}} {
		if *field.value, err = move(*field.value,
			func(v string) (string, error) { return s.Open(personID, field.field, v) },
			func(v string) (string, error) { return s.Seal(personID, field.field, v) },
		); err != nil {
			return p, 0, err
		}
	}
	credentials := slices.Clone(p.Credentials)
	for i, c := range credentials {
		if c.Method != MethodTOTP {
			continue
		}
		if credentials[i].Verifier, err = move(c.Verifier,
			func(v string) (string, error) {
				return s.OpenCredential(personID, c.ID, FieldTOTP, v)
			},
			func(v string) (string, error) {
				return s.SealCredential(personID, c.ID, FieldTOTP, v)
			},
		); err != nil {
			return p, 0, err
		}
	}
	p.Credentials = credentials
	return p, moved, nil
}
