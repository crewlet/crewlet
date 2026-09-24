package pages

import (
	"context"
	"database/sql"

	"github.com/crewlet/crewlet/internal/statelog"
)

// PublishGeneration writes a reanchor's record onto this log's NEW stream.
//
// A CLAIM ON THE GENERATION'S OWN SUBJECT ([statelog.Publisher.Claim]): the
// first node to publish there holds the transition, a second node deriving the
// same number meets that record and is refused, and this node's own earlier
// attempt — one that crashed after this step — is found by its op id and counts
// as done. The applier writes `pages_log_generations` from it on every node
// that applies the new stream, this one included.
func (s *Store) PublishGeneration(ctx context.Context, actor Actor, nodeID string,
	gen uint32, in statelog.ReanchorInputs) error {

	if err := actor.validate(); err != nil {
		return err
	}
	if nodeID == "" {
		return invalid("node", "a generation record names the node that re-anchored")
	}
	subject := GenerationSubject(gen)
	scope := ScopeSet{Subject: true}
	at := s.now()
	opID := statelog.GenerationOpID(gen, in.StreamCreatedAt, nodeID)
	return s.publisher.Claim(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return s.decide(actor, subject, OpGeneration, scope, opID, Generation{
				V: GateRecordVersion, Generation: gen, By: actor.Name(),
				PrevHighest: in.Highest, StreamCreatedAt: in.StreamCreatedAt.UTC(),
			}, nil, at)
		},
	})
}
