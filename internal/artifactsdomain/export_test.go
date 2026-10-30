package artifactsdomain

import (
	"context"
	"database/sql"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ShareBeforeTheGrantRule writes a share as a build before
// [artifacts.Audience.ValidateGrant] did: the same record, through the same log
// and applier, with the audience held only to [artifacts.Audience.Validate].
// It stands in a share such a build left stored (a login named as a seat), so
// a test can hold that its owner still sees and withdraws it.
func ShareBeforeTheGrantRule(ctx context.Context, w *Writer, c Call, id artifacts.ID,
	s artifacts.Share) (Written, error) {

	at := w.now()
	grant := artifacts.Share{Audience: s.Audience, Role: s.Role, GrantedBy: c.Authority.Actor, GrantedAt: at}
	opID, err := w.operation(c, string(OpShare), id.String(),
		struct {
			Audience string
			Role     artifacts.Role
		}{s.Audience.String(), s.Role})
	if err != nil {
		return Written{}, err
	}
	subject, scope := ArtifactSubject(id), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decision(stamp, c, subject, OpShare, scope, opID,
				SharePayload{V: baseRecordVersion, Share: grant}, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// PublishError is the framework's refusal of a publish on subject, in this
// domain's terms.
func PublishError(subject statelog.Subject, err error) error { return publishError(subject, err) }

// RefusalError is what a refusal verdict of the applier is told as.
func RefusalError(ctx context.Context, w *Writer, id string, reason Refusal, s HistorySummary,
	number int) error {

	return w.refusal(ctx, id, reason, s, number)
}
