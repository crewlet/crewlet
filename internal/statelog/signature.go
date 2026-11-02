package statelog

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/recordseal"
)

// EVERY RECORD ON A DOMAIN'S LOG IS SEALED ON ITS WAY TO THE APPENDER, and
// this is where. The seal itself — the frame, the keyring, the signer, the
// verifier and the three verdicts — is [recordseal]'s, a leaf, because every
// other channel whose messages are instructions seals under the same frame and
// none of them reads a log (ADR-0031). What is the state log's is that no
// record of its leaves unsealed and none reaches a domain unopened: the
// appender below, and [VerifiedLog] and the apply loop on the way back.

// signedAppender signs every body on its way to the log.
//
// WRAPPING THE APPENDER rather than signing at each call site is what makes
// the rule impossible to forget: there are three appends in this package
// that sign their own records today (a write authority's, a read index's
// barrier and the anchor sweep's trimmed record), and the next one somebody
// adds is signed by construction rather than by review.
type signedAppender struct {
	Appender
	signer *recordseal.Signer
}

func (a signedAppender) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	return a.Appender.Append(ctx, subject, msgID, expect, a.signer.Seal(body))
}

// signing wraps log so every record it carries is signed, refusing a nil
// signer rather than writing an unsigned one.
func signing(log Appender, signer *recordseal.Signer, domain string) (Appender, error) {
	if signer == nil {
		return nil, fmt.Errorf("%w: %s has no signer, so its records would be "+
			"unsigned and every node that verifies would refuse them — Tier A "+
			"secrets.keys is required wherever a state-log domain runs",
			recordseal.ErrUnsigned, domain)
	}
	return signedAppender{Appender: log, signer: signer}, nil
}
