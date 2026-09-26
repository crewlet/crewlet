package statelog

import (
	"context"
	"errors"
	"fmt"
)

// ClaimedElsewhere reports a create-only subject another operation holds.
type ClaimedElsewhere struct {
	Subject Subject
	Holder  string
}

func (e *ClaimedElsewhere) Error() string {
	return fmt.Sprintf("statelog: %s is held by operation %q", e.Subject, e.Holder)
}

// Holder reports which operation holds a create-only subject — the op id of the
// record at its last sequence — and false when the subject holds nothing.
func (p *Publisher) Holder(ctx context.Context, subj Subject) (string, bool, error) {
	subject := p.subjectOf(subj)
	seq, found, err := p.log.LastSeq(ctx, subject)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("statelog: read the last message on %s: %w",
			subject, err)
	case !found:
		return "", false, nil
	}
	_, payload, _, held, err := p.log.At(ctx, seq)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("statelog: read the record at %d on %s: %w",
			seq, subject, err)
	case !held:
		return "", false, fmt.Errorf("statelog: %s's record at %d is gone between "+
			"two reads of it", subject, seq)
	}
	env, err := p.domain.Envelope(payload)
	if err != nil {
		return "", false, fmt.Errorf("statelog: read the envelope at %d on %s: %w",
			seq, subject, err)
	}
	return env.OpID, true, nil
}

// Claim takes a create-only subject for req.OpID, and succeeds when req.OpID
// holds it — by this call's record, or by an earlier attempt's.
//
// # Why the holder is asked, and not only the broker
//
// A transition that crashed after its claim landed is re-run, and the re-run
// meets its own record on the subject. The write path cannot tell that record
// from anybody else's until its own applier has applied it, and the claim is
// exactly the moment a caller may have that applier stopped — so it answers
// `behind`, every time. The record's own op id says whose it is. And a claim
// the broker answered ambiguously, or refused because somebody else's record
// arrived first, is settled the same way: whoever holds the subject is the
// answer, and a holder that is not req.OpID is [ClaimedElsewhere].
//
// req.OpID MUST NAME THE CLAIMANT, not only the thing claimed: two claimants
// minting one id would each read the other's record as their own.
func (p *Publisher) Claim(ctx context.Context, req Request) error {
	if held, err := p.heldBy(ctx, req); err != nil || held {
		return err
	}
	res, err := p.Publish(ctx, req)
	if err == nil && res.Outcome != OutcomeUnknown {
		return nil
	}
	// THE HOLDER DECIDES WHAT IS SAID: another claimant is the answer
	// whatever the publish reported, and otherwise the publish's own
	// refusal names why nothing landed.
	held, heldErr := p.heldBy(ctx, req)
	var elsewhere *ClaimedElsewhere
	switch {
	case held:
		return nil
	case errors.As(heldErr, &elsewhere):
		return heldErr
	case err != nil:
		return err
	case heldErr != nil:
		return heldErr
	}
	// THE THIRD VALUE: unanswered, and nothing holds the subject yet — the
	// record may still land, and a retry under the same op id finds it if
	// it did.
	return fmt.Errorf("statelog: the claim on %s under %q went unanswered and "+
		"nothing holds the subject yet; retry under the same op id",
		req.Subject, req.OpID)
}

// HeldElsewhere reads whether a create-only subject is held by an operation
// other than opID: [ClaimedElsewhere] when it is, and nil when nothing holds it
// or opID does.
//
// A READ, never a claim: it is what a caller asks before it commits to
// anything a claim would decide, and [Publisher.Claim] still arbitrates —
// a record landing after this read is met there.
func (p *Publisher) HeldElsewhere(ctx context.Context, subj Subject, opID string) error {
	_, err := p.heldBy(ctx, Request{Subject: subj, OpID: opID})
	return err
}

// heldBy reports whether req.OpID holds req.Subject: true when it does, false
// with no error when nothing does, and [ClaimedElsewhere] when another
// operation does.
func (p *Publisher) heldBy(ctx context.Context, req Request) (bool, error) {
	holder, found, err := p.Holder(ctx, req.Subject)
	switch {
	case err != nil:
		return false, err
	case !found:
		return false, nil
	case holder == req.OpID:
		return true, nil
	}
	return false, &ClaimedElsewhere{Subject: req.Subject, Holder: holder}
}
