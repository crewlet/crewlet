package artifactdataapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ONE ADMISSION every datastore path takes, the HTTP routes here and the
// read questions in internal/api/queries alike: read the artifact's head,
// decide the verb on it through the authority table, and stamp what the table
// concluded. A second copy of this in the questions would be the place the
// two first disagreed about who may read a row.

// Heads is the artifacts estate's read of one artifact's head: what the shares
// and the owner a decision reads come from. The estate router's, so a node
// whose own copy is out of service decides from a peer's.
type Heads interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (
		artifactsdomain.Head, statelog.Position, error)
}

// Gate decides one datastore verb on one artifact for one principal.
type Gate struct {
	Heads Heads

	// Chart answers the lead and unit relations a share is resolved
	// through.
	Chart authz.Chart

	// Company is the running company's artifacts settings (Tier B), read
	// per decision; nil, or a node with no company, keeps agents off other
	// people's artifacts, the closed default.
	Company func() config.CompanyArtifacts

	// Limits are the plan quotas in force on this node (Tier A), stamped
	// into every write.
	Limits func() artifacts.Limits

	// Now is the instant a decision is taken at; nil is the wall clock.
	Now func() time.Time
}

// ErrNoSuchArtifact is an artifact this caller is answered as absent: one that
// does not exist, one a purge destroyed, and one the caller has no relation to
// (03 section 2.7), all alike.
var ErrNoSuchArtifact = errors.New("artifactdataapi: no such artifact")

// Refused is a decision that refused, for the surface to render.
type Refused struct {
	Action   authz.Action
	Decision authz.Decision
}

func (e *Refused) Error() string {
	return fmt.Sprintf("artifactdataapi: %s refused: %s", e.Action, e.Decision.Reason)
}

// Admission is what a decision that admitted concluded.
type Admission struct {
	Head     artifactsdomain.Head
	Decision authz.Decision

	// Role is the caller's effective role on the artifact, exact: what a
	// write is stamped with and a read is shaped by.
	Role artifacts.Role

	Principal iam.Principal
}

// Reading is the admission as a datastore read is shaped by it.
func (a Admission) Reading(fresh statelog.Freshness) artifactdata.Reading {
	return artifactdata.Reading{Record: iam.RecordOwner(a.Principal), Role: a.Role, Fresh: fresh}
}

// Authority is the admission as a datastore write is stamped with it.
func (a Admission) Authority(origin artifacts.WriteOrigin, limits artifacts.Limits) artifacts.Authority {
	return artifacts.Authority{
		Actor: iam.ActorFor(a.Principal), Role: a.Role, Elevated: a.Decision.Elevated,
		Limits: limits, Origin: origin,
	}
}

// decisionRead is the level a head is read at before a decision on it: the
// dashboard's, whatever this node's copy holds while it is in service. The
// window a share revoked between this read and a write's apply leaves open is
// at most that write, the one every stamped decision accepts.
var decisionRead = statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceDashboard)}

// Admit decides action on artifact id for p: [ErrNoSuchArtifact] for an
// artifact the caller is answered as absent, a [*Refused] for one the table
// refuses, and any other error for one this node could not decide.
func (g Gate) Admit(ctx context.Context, p iam.Principal, action authz.Action, id artifacts.ID) (Admission, error) {
	head, _, err := g.Heads.Artifact(ctx, id, decisionRead)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return Admission{}, ErrNoSuchArtifact
	case err != nil:
		return Admission{}, err
	}
	var settings config.CompanyArtifacts
	if g.Company != nil {
		settings = g.Company()
	}
	object := authz.Object{
		Kind: authz.KindArtifact, ID: id.String(), Owner: head.Owner, Shares: head.Shares,
		OwnerIsAgent: head.OwnerKind == iam.ActorAgent, AgentsManageOthers: settings.AgentsManageOthers,
	}
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	d := authz.Decide(ctx, p, action, object, g.Chart, now)
	switch {
	case d.Unknown():
		return Admission{}, d.Err
	case !d.Allowed && d.NotFound():
		return Admission{}, ErrNoSuchArtifact
	case !d.Allowed:
		return Admission{}, &Refused{Action: action, Decision: d}
	}
	// THE ROLE IS THE READ ROW'S STANDING, exact: a write stamped with a
	// role the chart could not settle would have the applier refuse a write
	// the caller may make, or admit one they may not.
	standing, err := authz.StandingOn(ctx, p, object, g.Chart, now)
	if err != nil {
		return Admission{}, err
	}
	if standing.Role == "" {
		// THE VERB ADMITTED AND THE READ DID NOT: only a verb whose row is
		// looser than reading would, and no datastore verb is.
		return Admission{}, ErrNoSuchArtifact
	}
	return Admission{Head: head, Decision: d, Role: standing.Role, Principal: p}, nil
}

// limits is the plan quotas in force.
func (g Gate) limits() artifacts.Limits {
	if g.Limits == nil {
		return artifacts.Limits{}
	}
	return g.Limits()
}
