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

// Moving people's values onto the keyring's active key. These rows derive from
// the log, so a node cannot re-seal them in place (it would diverge from its
// peers, and a replay from the floor would write the old ciphertext back): each
// person holding a value under an old key gets an ordinary content update
// ([Writer.UpdatePerson]) carrying the same values sealed afresh.
//
// An open invitation's address is not moved, since a re-issue is refused while
// it is open: it is counted ([SealedCount.Invitations]) and a rotation waits
// for it to be redeemed or lapse, as it waits out the cookies the old key
// signed; a lapsed one is not counted, since nothing opens its address again.
// The authentication trail is not moved either: nothing opens its
// payloads, and who did what is in the clear.

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
func (c SealedCount) Outside(active string) int { return secrets.KeyCounts(c.People).Outside(active) }

// InvitationsOutside is how many live invitations' addresses are sealed under
// a key other than active — what a rotation waits out rather than moves.
func (c SealedCount) InvitationsOutside(active string) int {
	return secrets.KeyCounts(c.Invitations).Outside(active)
}

// The statements [Reader.SealedKeys] counts by: full scans, since no index
// narrows "every row holding a sealed value".
const (
	sealedPeopleStatement = `
		SELECT name_sealed FROM iam_people
		WHERE length(name_sealed) > 0
		UNION ALL
		SELECT email_sealed FROM iam_people
		WHERE length(email_sealed) > 0`
	sealedInvitationsStatement = `
		SELECT email_sealed FROM iam_invites
		WHERE ` + openInvitation + ` AND length(email_sealed) > 0`
)

// SealedKeys counts the sealed values this node's rows hold that anything
// opens, by the keyring key each is sealed under, without opening one; now
// judges an invitation's expiry. The key id comes off the envelope
// ([secrets.EnvelopeKeyID]), so a node that cannot open a value can still count
// what a rotation must move.
func (r *Reader) SealedKeys(ctx context.Context, now time.Time) (SealedCount, error) {
	out := SealedCount{People: map[string]int{}, Invitations: map[string]int{}}
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			what, sql string
			into      map[string]int
		}{
			{"names and addresses", sealedPeopleStatement, out.People},
			{"second factors", `
				SELECT verifier FROM iam_credentials WHERE method = 'totp'`,
				out.People},
			{"invitations", sealedInvitationsStatement, out.Invitations},
		} {
			var args []any
			if q.what == "invitations" {
				args = append(args, millis(now))
			}
			if err := secrets.CountKeys(ctx, tx, q.sql, q.into, args...); err != nil {
				return fmt.Errorf("iamdomain: count the sealed %s: %w", q.what, err)
			}
		}
		return nil
	})
	return out, err
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
// It is its own retry: each record is decided against the person's row now,
// so a rerun moves only what is still under an old key. Each record takes a
// fresh operation id, not one derived from the person, or a later rotation
// moving the same person would collapse into this one's. A person who fails
// does not stop the rest, who would otherwise stay on a key about to be
// dropped; every failure is returned with the report.
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
				// The last run's count is the landed record's: a decide may rerun
				// against a fresh snapshot.
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

// The scans [Reader.sealedOutside] finds stale names and addresses by; each
// reads every person holding one.
const (
	sealedNamesStatement = `SELECT id, name_sealed FROM iam_people
		WHERE length(name_sealed) > 0`
	sealedAddressesStatement = `SELECT id, email_sealed FROM iam_people
		WHERE length(email_sealed) > 0`
)

// sealedOutside is every person holding a value — a name, an address, a
// second factor's seed — sealed under a key other than active, in id order.
func (r *Reader) sealedOutside(ctx context.Context, active string) ([]string, error) {
	stale := map[string]bool{}
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		for _, query := range []string{
			sealedNamesStatement,
			sealedAddressesStatement,
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
// A value this keyring cannot open refuses the whole person rather than being
// skipped: its key is already dropped, and a record moving the rest would
// report the person moved while that value stays unreadable, the very state
// the operator drops the old key on. internal/fleetsecrets' rekey refuses the
// same way.
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
