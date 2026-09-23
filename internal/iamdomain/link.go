package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PROVIDER LINK: an identity provider's subject pinned to one person.
//
// # Linking is explicit, and an email match is never a link
//
// A provider sign-in proves one thing — that whoever is at the browser holds a
// particular SUBJECT at a particular ISSUER — and a link is what turns that
// into a person here. It is made by exactly two gestures: an invitation
// redeemed THROUGH the provider pins the subject the provider came back with to
// the person the redemption creates ([Enrolment.Link]), and an administrator
// pins one to somebody who already exists ([Writer.Link]). Nothing links on an
// address, because at most providers a person sets their own, and the person an
// address match would link them to is whoever is most worth becoming.
//
// # It is a CLAIM, and the claim is the uniqueness
//
// Two administrators pinning one subject to two people must contend, or the
// subject signs in as whichever row a read happens to find — so a link
// arbitrates on its own subject ([KindLink]), create-only at an expectation of
// zero, like an address. What the claim's apply writes is a CREDENTIAL ROW,
// method oidc: the link is what the person signs in WITH, and the credential
// table is where a provider sign-in has always resolved a subject
// ([Reader.PersonBySubjectBlind]). The apply is the ONLY writer of those rows —
// a person's document never carries one ([authoredPerson] refuses it, the
// applier skips it and never deletes one) — so the arbitration and the row are
// one fact. A person holds at most one link: pinning a second revokes the
// first, which is the column semantics every other claim has, and the move
// records the old subject's release after it.
//
// A REMOVAL RELEASES IT with every other credential the person held, in its
// own transaction, and its tombstone does not name it: the removal's payload is
// pinned for ever ([GateRecordVersion]), and a field it cannot have belongs on
// a record that is not a gate.

// Link is one provider subject as this estate holds it: the issuer in the
// clear and the subject BLINDED ([Blinder.Subject]) — the subject identifies a
// person at a third party, and this estate never holds one in the clear.
type Link struct {
	Issuer string
	Blind  string
}

// LinkChange is an administrator pinning a provider subject to somebody.
type LinkChange struct {
	PersonID string

	// Link is the subject to pin.
	Link Link

	// Replacing is the blind of the link the person holds NOW, when this
	// moves them from one subject to another, and empty for a first link.
	// The caller has just read the person, and the decide confirms it —
	// see [Writer.Rebind] for why a move names where it moves FROM.
	Replacing string

	OpID   string
	Reason string
}

// Link pins a provider subject to a person who already exists, and announces
// it ([types.IAMIdentityLinked], via an administrator).
//
// A SUBJECT SOMEBODY ELSE HOLDS IS REFUSED, naming them ([ErrClaimed] of kind
// [KindLink]) — never relinked: taking a subject off one person and giving it
// to another is two gestures somebody makes on purpose, an unlink and a link,
// and a link that did both would be the one way to take over an account that
// looks like maintenance. The same holds from the other side: a person already
// linked to a DIFFERENT subject is refused ([ErrLinked]) unless the change
// names that subject as the one it replaces. A MOVE (Replacing set) claims the
// new subject first and releases the old one after, for [Writer.Rename]'s
// reason: a new subject refused leaves the person linked where they were.
func (w *Writer) Link(ctx context.Context, in LinkChange) (statelog.Result, error) {
	if err := w.mayAdminister(OpClaim); err != nil {
		return statelog.Result{}, err
	}
	switch {
	case in.PersonID == "" || in.OpID == "":
		return statelog.Result{}, fmt.Errorf("iamdomain: pinning a provider "+
			"subject needs the person and an operation id, and has (%q, %q)",
			in.PersonID, in.OpID)
	case in.Link.Issuer == "" || in.Link.Blind == "":
		return statelog.Result{}, fmt.Errorf("%w: a provider link needs the "+
			"issuer and the subject's blind, and this one has (%q, %q)",
			ErrInvalid, in.Link.Issuer, in.Link.Blind)
	case in.Replacing == in.Link.Blind:
		return statelog.Result{}, fmt.Errorf("%w: person %s is already "+
			"linked to that subject", ErrInvalid, in.PersonID)
	case len(in.Reason) > MaxReason:
		return statelog.Result{}, fmt.Errorf("%w: the reason is %d bytes "+
			"and the cap is %d", ErrInvalid, len(in.Reason), MaxReason)
	}
	payload := Claim{Issuer: in.Link.Issuer}
	var (
		result statelog.Result
		err    error
	)
	if in.Replacing != "" {
		result, err = w.replace(ctx, KindLink, in.PersonID, in.Replacing,
			in.Link.Blind, payload, in.OpID, in.Reason)
	} else {
		result, err = w.claim(ctx, w.gesture(), KindLink, in.Link.Blind,
			in.PersonID, payload, "", in.OpID+":"+string(KindLink), "")
	}
	w.announce(ctx, result, err, types.IAMIdentityLinked{
		Person: in.PersonID, Issuer: in.Link.Issuer, Via: types.LinkViaAdmin,
		By: w.Actor,
	})
	return result, err
}

// Unlink takes a provider subject off the person holding it, and announces it
// ([types.IAMIdentityUnlinked]). Their provider sign-in resolves to nobody from
// the next request on every node that has applied it.
//
// THE CALLER NAMES THE HOLDER and the decide confirms it, for [Writer.Release]'s
// reason: the record is filed under the holder's bucket, which is where a read
// about them looks. The subject keeps its arbitration anchor, so the next
// person it is pinned to contends with anybody else pinning it.
func (w *Writer) Unlink(ctx context.Context, personID string, link Link,
	opID, reason string) (statelog.Result, error) {

	if link.Blind == "" {
		return statelog.Result{}, fmt.Errorf("%w: unlinking needs the "+
			"subject's blind", ErrInvalid)
	}
	result, err := w.release(ctx, w.gesture(), KindLink, link.Blind, personID,
		opID, reason, false)
	w.announce(ctx, result, err, types.IAMIdentityUnlinked{
		Person: personID, Issuer: link.Issuer, By: w.Actor, Reason: reason,
	})
	return result, err
}

// ErrLinked is a person who already holds a live link to a DIFFERENT provider
// subject than the one being pinned, from a change that did not name it as the
// one it replaces.
//
// NEVER RELINKED IN PASSING. A person signs in through exactly one subject,
// and a second pin that quietly superseded the first would be a way to take
// over somebody's provider sign-in that reads as a routine link: the account
// they have used for a year stops working and a different one starts. So a
// change that moves them says which link it moves them OFF — an
// administrator's move names it, having just read the person — and an
// invitation redeemed a second time through a different provider account than
// its first attempt pinned is refused rather than switched.
var ErrLinked = errors.New("iamdomain: this person is already linked to a " +
	"different identity provider subject")

// unlinkedElsewhere refuses a link for somebody who holds a live link to a
// different subject than both the one being pinned and the one this change
// replaces, read inside the claim's own snapshot — see [ErrLinked].
//
// THE SAME SUBJECT PASSES, because that is a retry: an enrolment through the
// provider re-driven after its link claim landed re-runs this decide, and
// refusing it there would strand the redemption it is finishing.
func unlinkedElsewhere(ctx context.Context, tx *sql.Tx, personID, blind,
	replacing string) error {

	held, err := liveLinkOf(ctx, tx, personID)
	switch {
	case err != nil:
		return err
	case held.Blind == "", held.Blind == blind, held.Blind == replacing:
		return nil
	case replacing != "":
		return fmt.Errorf("%w: the link this move replaces is not the one "+
			"person %s holds now — read them again", ErrLinked, personID)
	}
	return fmt.Errorf("%w: person %s — move them (naming the link it "+
		"replaces) or unlink them first", ErrLinked, personID)
}

// liveLinkOf is the live link one person holds, read INSIDE a decide's
// snapshot, or the zero Link for somebody linked to nothing.
//
// THE LOWEST ID when a restore has left them two, which is the row the
// directory reports too ([PersonRow.Link]); the claim report names the
// duplicate. A credential document this build cannot open still yields the
// blind — the person IS linked — with the issuer unknown.
func liveLinkOf(ctx context.Context, tx *sql.Tx, personID string) (Link, error) {
	var (
		out      Link
		document []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT subject_blind, document FROM iam_credentials
		 WHERE person_id = ? AND method = ? AND revoked_at = 0
		 ORDER BY id LIMIT 1`, personID, string(MethodOIDC)).
		Scan(&out.Blind, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Link{}, nil
	case err != nil:
		return Link{}, fmt.Errorf("iamdomain: read person %s's provider "+
			"link: %w", personID, err)
	}
	if held, err := DecodeCredential(document); err == nil {
		out.Issuer = held.Issuer
	}
	return out, nil
}

// linkHolderOf is who holds a LIVE link to one provider subject, read INSIDE a
// decide's snapshot — the credential-row half of [holderOf].
//
// ONE HOLDER, the lowest id, when a restore has left two: the claim's decide
// refuses naming them either way, and [Reader.PersonBySubjectBlind] refuses
// the sign-in for both until an operator removes one.
func linkHolderOf(ctx context.Context, tx *sql.Tx, blind string) (
	string, bool, error) {

	var holder string
	err := tx.QueryRowContext(ctx, `
		SELECT person_id FROM iam_credentials
		 WHERE method = ? AND subject_blind = ? AND revoked_at = 0
		 ORDER BY person_id LIMIT 1`, string(MethodOIDC), blind).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("iamdomain: read the link claim on a "+
			"provider subject: %w", err)
	}
	return holder, true, nil
}

// linkNamespace is the uuid namespace a link's credential id is derived in.
var linkNamespace = uuid.NewSHA1(uuid.Nil, []byte("crewlet.iam.link"))

// linkCredentialID is the credential row one (person, subject) pair is held
// under.
//
// DERIVED, so every node writes the one row the same record names, and a
// re-link of the same person to the same subject revives the row it left
// rather than growing a second. Keyed on BOTH halves, because a restore can
// leave two people holding one subject, and one id for both would be a primary
// key collision inside an apply transaction — which stops the log on every
// node at once.
func linkCredentialID(personID, blind string) string {
	return uuid.NewSHA1(linkNamespace, []byte(personID+"\x00"+blind)).String()
}

// authoredPerson is the encoding of a person document a WRITER authors, which
// refuses what only a claim may write.
//
// A PROVIDER LINK IN A DOCUMENT IS REFUSED: the link's claim is the only writer
// of an oidc credential ([KindLink]), and a document that carried one would be
// a second writer of the same fact — one the broker never arbitrated, so two
// documents could pin one subject to two people. The applier skips such an
// entry rather than failing, because a record it cannot refuse is one every
// node would stall on; this is the refusal, at the one frame that can make it.
func authoredPerson(p Person) ([]byte, error) {
	for _, c := range p.Credentials {
		if c.Method == MethodOIDC {
			return nil, fmt.Errorf("%w: a provider link is not part of a "+
				"person's document — it is pinned with Writer.Link, or by an "+
				"invitation redeemed through the provider", ErrInvalid)
		}
	}
	return EncodePerson(p)
}
