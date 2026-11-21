package statelog

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/recordseal"
)

// EVERY RECORD ON A DOMAIN'S LOG IS SEALED ON ITS WAY TO THE APPENDER, here.
// The seal (frame, keyring, signer, verifier, verdicts) is [recordseal]'s, a
// leaf because every channel carrying instructions shares it and none reads a
// log (ADR-0031); the state log's
// part is that no record leaves unsealed and none reaches a domain unopened:
// the appender below, and [VerifiedLog] and the apply loop on the way back.

// signedAppender signs every body on its way to the log. WRAPPING THE APPENDER
// rather than signing at each call site signs the next append somebody adds by
// construction rather than by review.
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
