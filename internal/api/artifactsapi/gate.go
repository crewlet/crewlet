package artifactsapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ONE ADMISSION every artifact path takes, the HTTP routes here and the read
// questions in internal/api/queries alike: read the artifact's head, decide the
// verb on it through the authority table, and conclude the caller's role from
// the same decision. A second copy of this in the questions would be the place
// the two first disagreed about who may read a private artifact.
//
// # Access is decided before anything is touched
//
// Every handler admits first and reads or writes after: the head a decision
// reads is the artifact's shares and owner and nothing of its content, and a
// caller the table refuses is answered before a version, a file or a byte is
// read. A caller with no relation is answered exactly as an id nothing holds.
//
// # The admin path is recorded where it is used
//
// An administrator may open any artifact (admin is host access, and a refusal
// would promise a privacy the engine cannot keep), and the override is made
// visible: [Gate.Read] and [Gate.Content] write an elevated-read record for
// every read the admin path alone admitted, before the read is answered, and
// REFUSE the read when the record cannot be written. An override nobody can
// see afterwards is the thing the record exists to prevent. Every read of an
// artifact takes one of the two: its metadata through [Gate.Read], its content
// (a version's files, its datastore) through [Gate.Content], whatever verb
// decides it.
//
// # The trash stops serving content at once
//
// An artifact in the trash keeps its head, its versions list and its history
// for the owner's trash view, and serves no content: [Gate.Content] refuses it
// ([artifactsdomain.ErrTrashed]) before anything is read or recorded, so the file routes, the
// datastore's reads and its export, and the room that pushes its data all
// stop together.

// Heads is the artifacts estate's read of one artifact's head: what the shares
// and the owner a decision reads come from. The estate router's, so a node
// whose own copy is out of service decides from a peer's.
type Heads interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (
		artifactsdomain.Head, statelog.Position, error)
}

// Marks are the reader records a read leaves: the admin path's elevated read
// (D09), and when a person last opened an artifact (D21).
type Marks interface {
	ReadElevated(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, version int) (
		artifactsdomain.Written, error)
	Opened(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
}

// Gate decides one artifact verb on one artifact for one principal.
type Gate struct {
	// Heads reads the head a decision is taken on. REQUIRED.
	Heads Heads

	// Chart answers the lead and unit relations a share is resolved
	// through. REQUIRED; pass authz.NoChart to decide on grants alone.
	Chart authz.Chart

	// Company is the running company's artifacts settings (Tier B), read
	// per decision. Nil, or a node with no company, keeps agents off other
	// people's artifacts and the seen-by list to editors: the closed
	// defaults.
	Company func() config.CompanyArtifacts

	// Limits are the plan quotas in force on this node (Tier A), stamped
	// into every write. REQUIRED.
	Limits func() artifacts.Limits

	// Marks records elevated reads and openings. REQUIRED: a read the admin
	// path admitted is refused rather than answered unrecorded.
	Marks Marks

	// Roster enumerates the company's units and seats, for the listing
	// filter and the share dialog's effective access. REQUIRED.
	Roster Roster

	// Now is the instant a decision is taken at; nil is the wall clock.
	Now func() time.Time
}

// validate refuses a gate missing what every decision reads.
func (g Gate) validate() error {
	switch {
	case g.Heads == nil:
		return errors.New("artifactsapi: no artifact heads: every route decides on the " +
			"artifact's owner and shares")
	case g.Chart == nil:
		return errors.New("artifactsapi: no chart: a share to a unit and the lead arm " +
			"are resolved through one; pass authz.NoChart to decide on grants alone")
	case g.Limits == nil:
		return errors.New("artifactsapi: no plan quotas: every write is stamped with the " +
			"limits in force")
	case g.Marks == nil:
		return errors.New("artifactsapi: no reader marks: a read the admin path admits " +
			"is recorded before it is answered")
	case g.Roster == nil:
		return errors.New("artifactsapi: no roster: a listing resolves the units a viewer " +
			"sits in and the agents they lead from it")
	}
	return nil
}

// ErrNoSuchArtifact is an artifact this caller is answered as absent: one that
// does not exist, one a purge destroyed, and one the caller has no relation to
// (03 section 2.7), all alike.
var ErrNoSuchArtifact = errors.New("artifactsapi: no such artifact")

// ErrRecordUnwritten is a read the admin path alone admitted whose
// elevated-read record could not be written: the read is refused rather than
// answered with no trace of the override.
var ErrRecordUnwritten = errors.New("artifactsapi: the elevated read could not be recorded")

// Refused is a decision that refused, for the surface to render.
type Refused struct {
	Action   authz.Action
	Decision authz.Decision
}

func (e *Refused) Error() string {
	return fmt.Sprintf("artifactsapi: %s refused: %s", e.Action, e.Decision.Reason)
}

// Admission is what a decision that admitted concluded.
type Admission struct {
	Head      artifactsdomain.Head
	Decision  authz.Decision
	Principal iam.Principal

	// Standing is the caller's standing on the artifact, as the read row
	// resolves it: the role a reader is shown and a write is stamped with.
	Standing authz.Standing

	// elevated is an admission only the admin path made; see
	// [Admission.Elevated].
	elevated bool
}

// Role is the caller's effective role on the artifact. A lower bound where the
// chart could not settle it ([authz.Standing.Err]): what a read is shaped by,
// in the direction that shows less.
func (a Admission) Role() artifacts.Role { return a.Standing.Role }

// Elevated reports an admission only the admin path made: what a write is
// stamped elevated for and a read records. On a shared row it is the
// decision's own ([authz.Decision.Elevated]: the caller's relations do not
// reach the role the row asks); on the one admin row an artifact has (a purge)
// it is an administrator who does not hold the artifact as its owner.
func (a Admission) Elevated() bool { return a.elevated }

// call is a write's caller context: the admission's authority, the limits in
// force, the request's operation key and the work item the write is for.
//
// THE ROLE MUST BE EXACT: a role the chart could not settle would have the
// applier refuse a write the caller may make, or a share be held to a ceiling
// below the caller's own, so an unsettled one answers unknown.
func (a Admission) call(origin artifacts.WriteOrigin, limits artifacts.Limits, key, workItem string) (
	artifactsdomain.Call, error) {

	if err := a.Standing.Err(); err != nil {
		return artifactsdomain.Call{}, err
	}
	return artifactsdomain.Call{
		Authority: artifacts.Authority{
			Actor: iam.ActorFor(a.Principal), Role: a.Standing.Role, Elevated: a.elevated,
			Limits: limits, Origin: origin, WorkItem: workItem,
		},
		OpKey: key,
	}, nil
}

// decisionRead is the level a head is read at before a decision on it: the
// dashboard's, whatever this node's copy holds while it is in service. The
// window a share revoked between this read and a write's apply leaves open is
// at most that write, the one every stamped decision accepts.
var decisionRead = statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceDashboard)}

// now is this gate's clock.
func (g Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now().UTC()
}

// decidedAt is the instant a decision about this request is taken at: the one
// its route was decided at ([authz.RouteInstant]), because every check of one
// request is judged at one instant — a purge the route admitted on a fresh
// proof is not refused as stale once its row has been read — or this gate's
// clock for a call no route decided, a tool's.
func (g Gate) decidedAt(ctx context.Context) time.Time {
	if at, ok := authz.RouteInstant(ctx); ok {
		return at
	}
	return g.now()
}

// limits is the plan quotas in force.
func (g Gate) limits() artifacts.Limits { return g.Limits() }

// settings is the company's artifacts settings, the closed defaults where none
// is running.
func (g Gate) settings() config.CompanyArtifacts {
	if g.Company == nil {
		return config.CompanyArtifacts{}
	}
	return g.Company()
}

// object is the authority table's object for one artifact's head: its owner,
// its owner's kind and its shares, as the stored row holds them.
func (g Gate) object(head artifactsdomain.Head) authz.Object {
	return authz.Object{
		Kind: authz.KindArtifact, ID: head.ID.String(), Owner: head.Owner, Shares: head.Shares,
		OwnerIsAgent:       head.OwnerKind == iam.ActorAgent,
		AgentsManageOthers: g.settings().AgentsManageOthers,
	}
}

// head reads the artifact a decision is about, answering an absent or purged
// one as [ErrNoSuchArtifact].
func (g Gate) head(ctx context.Context, id artifacts.ID) (artifactsdomain.Head, error) {
	head, _, err := g.Heads.Artifact(ctx, id, decisionRead)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return artifactsdomain.Head{}, ErrNoSuchArtifact
	case err != nil:
		return artifactsdomain.Head{}, err
	}
	return head, nil
}

// Admit decides action on artifact id for p: [ErrNoSuchArtifact] for an
// artifact the caller is answered as absent, a [*Refused] for one the table
// refuses, and any other error for one this node could not decide.
func (g Gate) Admit(ctx context.Context, p iam.Principal, action authz.Action, id artifacts.ID) (Admission, error) {
	head, err := g.head(ctx, id)
	if err != nil {
		return Admission{}, err
	}
	return g.Decide(ctx, p, action, head)
}

// Decide is [Gate.Admit] on a head the caller already read.
func (g Gate) Decide(ctx context.Context, p iam.Principal, action authz.Action,
	head artifactsdomain.Head) (Admission, error) {

	object := g.object(head)
	now := g.decidedAt(ctx)
	d := authz.Decide(ctx, p, action, object, g.Chart, now)
	switch {
	case d.Unknown():
		return Admission{}, d.Err
	case !d.Allowed && d.NotFound():
		return Admission{}, ErrNoSuchArtifact
	case !d.Allowed:
		return Admission{}, &Refused{Action: action, Decision: d}
	}
	standing, elevated := d.Standing, d.Elevated
	if action != authz.ActionArtifactRead && !elevated {
		// A VERB WHOSE ROW IS NOT THE READ ROW resolves the standing the
		// read row does, so the role a write is stamped with is the role a
		// reader is shown, from one resolver. A verb only the admin path
		// admitted keeps its own decision's standing (owner): the read row
		// may admit the same administrator as the sharer they are, and a
		// publish stamped with a viewer's role would be refused by the
		// applier it was admitted to reach.
		var err error
		if standing, err = authz.StandingOn(ctx, p, object, g.Chart, now); err != nil {
			return Admission{}, err
		}
	}
	switch {
	case d.Standing.Role == "":
		// A ROW THAT REACHED NO RELATION (the admin row a purge is decided
		// by): the override is an administrator acting on an artifact they
		// do not own, at owner over whatever their relations grant.
		if !standing.Related.Includes(artifacts.RoleOwner) {
			standing = authz.Standing{Role: artifacts.RoleOwner, Related: standing.Related}
			elevated = true
		}
	case standing.Role == "":
		// THE VERB ADMITTED AND THE READ DID NOT: only a row looser than
		// reading would, and no shared row is. Answered as absent rather
		// than stamped with no role.
		return Admission{}, ErrNoSuchArtifact
	}
	return Admission{Head: head, Decision: d, Principal: p, Standing: standing,
		elevated: elevated}, nil
}

// Read admits a read of artifact id, at version (zero for one that names
// none, which is the head's), and records the admin path's override before the
// read is answered: the one admission every read of an artifact's content and
// metadata takes.
func (g Gate) Read(ctx context.Context, p iam.Principal, id artifacts.ID, version int) (Admission, error) {
	a, err := g.Admit(ctx, p, authz.ActionArtifactRead, id)
	if err != nil {
		return Admission{}, err
	}
	return a, g.recordElevated(ctx, a, version)
}

// Content admits a read of artifact id's CONTENT through action: a version's
// files at version ([authz.ActionArtifactRead]), or its datastore (the data
// read, query and export verbs, at zero, the head's). It refuses an artifact
// in the trash ([artifactsdomain.ErrTrashed]) before anything is read, and
// records the admin
// path's override before the content is answered, refusing the read when the
// record cannot be written ([ErrRecordUnwritten]).
func (g Gate) Content(ctx context.Context, p iam.Principal, action authz.Action, id artifacts.ID,
	version int) (Admission, error) {

	a, err := g.Admit(ctx, p, action, id)
	if err != nil {
		return Admission{}, err
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		return Admission{}, fmt.Errorf("%w: %s", artifactsdomain.ErrTrashed, id)
	}
	return a, g.recordElevated(ctx, a, version)
}

// recordElevated writes the elevated-read record a read only the admin path
// admitted owes, one per (day, administrator, artifact, version): the writer
// derives the operation from them, so every repeat that day collapses into
// the first. A gate with no marks records nothing and so admits nothing on
// the admin path.
func (g Gate) recordElevated(ctx context.Context, a Admission, version int) error {
	if !a.Elevated() {
		return nil
	}
	if g.Marks == nil {
		return fmt.Errorf("%w: this surface has no reader marks", ErrRecordUnwritten)
	}
	if version <= 0 {
		version = a.Head.HeadNumber
	}
	c := artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.ActorFor(a.Principal), Role: artifacts.RoleOwner, Elevated: true,
		Limits: g.limits(), Origin: originOf(a.Principal),
	}}
	written, err := g.Marks.ReadElevated(ctx, c, a.Head.ID, version)
	if err == nil && written.Result.Outcome == statelog.OutcomeUnknown {
		err = fmt.Errorf("whether the record landed is unknown (operation %s)", written.Result.OpID)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordUnwritten, err)
	}
	return nil
}

// markOpened records that the caller opened the artifact, at most once per
// person and artifact in each [artifacts.OpenedDebounce] window: the operation
// key is the window's, so a repeat inside it collapses on the operation ledger
// of whichever node it reaches.
//
// A RECORD THAT COULD NOT BE WRITTEN IS LOGGED AND THE OPEN GOES ON: when
// somebody last opened an artifact orders a listing and fills the seen-by list,
// and neither is worth a viewer's page. Only a person's opening is recorded
// (through a token of theirs too, which acts as them): a machine acting as
// itself opens nothing anybody is shown as having seen.
func (g Gate) markOpened(ctx context.Context, a Admission) {
	actor := iam.ActorFor(a.Principal)
	if actor.Kind != iam.ActorHuman {
		return
	}
	window := g.now().Truncate(artifacts.OpenedDebounce)
	c := artifactsdomain.Call{
		Authority: artifacts.Authority{Actor: actor, Role: a.Standing.Role,
			Limits: g.limits(), Origin: originOf(a.Principal)},
		OpKey: statelog.DeriveOpID(window, "opened", actor.Name, a.Head.ID.String()),
	}
	if _, err := g.Marks.Opened(ctx, c, a.Head.ID); err != nil {
		log.WarnContext(ctx, "artifacts_opened_unrecorded", "artifact", a.Head.ID.String(),
			"seat", actor.Name, "error", err.Error())
	}
}

// originOf is the surface a request came through: the dashboard's screens for
// a browser session, the API for every bearer credential (the CLI, a script).
func originOf(p iam.Principal) artifacts.WriteOrigin {
	if strings.HasPrefix(p.Via, iam.SessionPrefix) {
		return artifacts.OriginDashboard
	}
	return artifacts.OriginAPI
}
