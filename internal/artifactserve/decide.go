package artifactserve

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE AUTHORIZATION: who a ticket's credential stands for now, whether they may
// still read the artifact, and whether the version is still kept, decided from
// rows this node can vouch for or not at all.
//
// # A document is decided on every request
//
// A document request never reads a remembered verdict: it re-composes the
// principal from the identity rows, re-reads the artifact through the estate
// router under a staleness bound, and asks the authority table again. That is
// the whole of revocation's latency for a page: a share withdrawn, a session
// signed out, a person suspended, an artifact trashed or moved to another
// owner stops the next document request, on every node, as soon as that
// node's copy has applied it, and a copy that cannot say it has (out of
// service, past the bound, cut off from the broker) answers 503 rather than
// serve from rows that may still hold the share.
//
// # A subresource may ride the document's verdict, for thirty seconds
//
// The files a document loads are useless without it, and there are dozens of
// them per page, so each decision is remembered under the ticket for
// [artifacts.SubresourceVerdictAge], and every artifact movement this node's
// applier reports (a share, a trash, a transfer, a purge, a new asset)
// forgets the artifact's verdicts at once. A document request decides again
// whatever is remembered, and remembers the new decision.

// retryUnavailable is the Retry-After a 503 carries where the cause names no
// hint of its own: an apply loop's scale, which is what a stale or briefly
// unreachable copy is waiting for, and the same two seconds the API's guard
// tells a browser it could not check a session.
const retryUnavailable = 2 * time.Second

// maxVerdicts bounds the remembered verdicts: a ticket a viewer is using is
// decided again at its next document request, so a verdict evicted early
// costs one decision and nothing else. Sixteen thousand is far more distinct
// frames than one node opens in the thirty seconds a verdict is worth.
const maxVerdicts = 16384

// versionCacheBytes bounds the manifests kept for the subresources of open
// frames: a version is immutable, so a kept manifest is never wrong, and a
// manifest is at most a few hundred kilobytes (511 files of bounded paths).
const versionCacheBytes = 32 << 20

// The reasons a ticket is refused ([metrics.ArtifactsTicketRefusals]).
const (
	reasonInvalid    = "invalid"
	reasonExpired    = "expired"
	reasonHost       = "host"
	reasonNavigation = "navigation"
	reasonDocument   = "document"
	reasonRevoked    = "revoked"
	reasonStale      = "stale"
)

// The outcomes of a decision made through a peer ([metrics.ArtifactsAuthzPeerChecks]).
const (
	peerServed      = "served"
	peerRefused     = "refused"
	peerUnavailable = "unavailable"
)

// verdict is one admission, remembered for a ticket's subresources.
type verdict struct {
	artifact artifacts.ID
	number   int
	at       time.Time

	// documents is how the artifact's kind serves documents, read from the
	// row the decision was made over: whether a ticket opens its own
	// document alone.
	documents artifacts.Documents

	// fresh says this request decided it, rather than reading it back.
	fresh bool
}

// refusal is a decision that did not admit: the status to answer, the reason
// it is counted under, and a 503's retry hint.
type refusal struct {
	status int
	reason string
	retry  time.Duration
}

func refused() *refusal { return &refusal{status: 404, reason: reasonRevoked} }

// unavailable is a 503 for cause, its hint the state log's own rule for it
// (none for a refusal no wait clears on this node).
//
// EXCEPT THE ORIGIN'S OWN BOUND. The state log reads a read refused as too
// stale as the caller's to widen, so it names no wait; here the bound is the
// stall grace this package chose, and a copy past it is one catching up, which
// a wait does clear.
func unavailable(cause error) *refusal {
	retry := statelog.RetryAfter(cause, retryUnavailable)
	var refused *statelog.Refused
	if errors.As(cause, &refused) && refused.Code == statelog.RefuseTooStale {
		retry = retryUnavailable
	}
	return &refusal{status: 503, reason: reasonStale, retry: retry}
}

// verdicts is the remembered admissions, by ticket.
type verdicts struct {
	now func() time.Time

	mu sync.Mutex
	// gen moves on every movement this node hears, so a decision that read
	// its rows before a movement and finished after it is never remembered:
	// it was right for the request it answered and may already be wrong for
	// the next one.
	gen        uint64
	byTicket   map[string]verdict
	byArtifact map[artifacts.ID]map[string]struct{}
}

func newVerdicts(now func() time.Time) *verdicts {
	return &verdicts{now: now, byTicket: map[string]verdict{},
		byArtifact: map[artifacts.ID]map[string]struct{}{}}
}

// generation is the movement count a decision starts under.
func (v *verdicts) generation() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen
}

// get is the remembered verdict for a ticket, if one younger than
// [artifacts.SubresourceVerdictAge] is held.
func (v *verdicts) get(key string) (verdict, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	got, ok := v.byTicket[key]
	if !ok || v.now().Sub(got.at) >= artifacts.SubresourceVerdictAge {
		return verdict{}, false
	}
	return got, true
}

// put remembers a verdict decided under generation gen, unless something moved
// since.
func (v *verdicts) put(key string, got verdict, gen uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if gen != v.gen {
		return
	}
	if len(v.byTicket) >= maxVerdicts {
		v.sweep()
	}
	got.fresh = false
	v.byTicket[key] = got
	keys := v.byArtifact[got.artifact]
	if keys == nil {
		keys = map[string]struct{}{}
		v.byArtifact[got.artifact] = keys
	}
	keys[key] = struct{}{}
}

// sweep drops every verdict past its age, and when that frees nothing every
// verdict: called with the lock held, at the bound, which is rare by
// construction.
func (v *verdicts) sweep() {
	now := v.now()
	for key, got := range v.byTicket {
		if now.Sub(got.at) >= artifacts.SubresourceVerdictAge {
			v.drop(key, got.artifact)
		}
	}
	if len(v.byTicket) >= maxVerdicts {
		v.byTicket = map[string]verdict{}
		v.byArtifact = map[artifacts.ID]map[string]struct{}{}
	}
}

func (v *verdicts) drop(key string, id artifacts.ID) {
	delete(v.byTicket, key)
	if keys := v.byArtifact[id]; keys != nil {
		delete(keys, key)
		if len(keys) == 0 {
			delete(v.byArtifact, id)
		}
	}
}

// forget drops every verdict about the artifacts named, or every verdict at
// all, and moves the generation.
func (v *verdicts) forget(ids []artifacts.ID, all bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.gen++
	if all {
		v.byTicket = map[string]verdict{}
		v.byArtifact = map[artifacts.ID]map[string]struct{}{}
		return
	}
	for _, id := range ids {
		for key := range v.byArtifact[id] {
			delete(v.byTicket, key)
		}
		delete(v.byArtifact, id)
	}
}

// ArtifactsMoved is what this node hears an artifacts applier say after a
// committed batch (internal/engine's OnArtifactsMoved): its own while its copy
// serves, and a sound peer's, relayed, while it does not — so a share withdrawn
// on the fleet is forgotten here even when this node's applier is halted.
// Every verdict about an artifact that moved is forgotten, and a reset — which
// every switch between the two sources is — forgets everything.
//
// ANY MOVEMENT, NOT ONLY A NARROWING: a new asset changes what an asset route
// may answer, and telling a narrowing from a widening here would be a second
// reading of what the applier already decided. Over-forgetting costs one
// decision; under-forgetting is a revoked share served for thirty seconds.
//
// IT NEVER BLOCKS: it runs on the apply loop's own goroutine, and takes one
// mutex for a map deletion.
func (h *Handler) ArtifactsMoved(moved []artifactsdomain.Moved) {
	var ids []artifacts.ID
	for _, m := range moved {
		if m.What == artifactsdomain.MoveReset {
			h.verdicts.forget(nil, true)
			h.versions.removeIf(func(versionKey) bool { return true })
			return
		}
		ids = append(ids, m.Artifact)
		if m.What == artifactsdomain.MovePurge {
			id := m.Artifact
			h.versions.removeIf(func(k versionKey) bool { return k.artifact == id })
		}
	}
	if len(ids) > 0 {
		h.verdicts.forget(ids, false)
	}
}

// versionKey names one immutable version.
type versionKey struct {
	artifact artifacts.ID
	number   int
}

// versionSize estimates a version's weight in the cache by its manifest.
func versionSize(v artifactsdomain.Version) int64 {
	size := int64(512)
	for _, e := range v.Manifest.Files {
		size += int64(len(e.Path)+len(e.ContentType)) + 256
	}
	return size
}

// ticketKey is the verdict cache's key for a ticket: everything the claims say
// that a decision depends on, so two tickets can never share a verdict even if
// they shared a nonce.
func ticketKey(c artifacts.TicketClaims) string {
	return c.Nonce + "|" + c.Artifact.String() + "|" + strconv.Itoa(c.Version) + "|" +
		c.Lineage + "|" + c.Principal + "|" + strconv.FormatInt(c.AccessEpoch, 10) + "|" +
		c.Document
}

// authorize is the admission for one request: the remembered verdict for a
// subresource where one is held, and a fresh decision otherwise.
func (h *Handler) authorize(ctx context.Context, c artifacts.TicketClaims, fresh bool) (
	verdict, artifactsdomain.Version, *refusal) {

	key := ticketKey(c)
	if !fresh {
		if got, ok := h.verdicts.get(key); ok {
			if v, ok := h.versions.get(versionKey{c.Artifact, c.Version}); ok {
				return got, v, nil
			}
		}
	}
	return h.decide(ctx, c, key)
}

// readFreshness is the staleness bound an authorization reads the artifact
// under: [statelog.StallGrace], the bound the framework already alarms on and
// the identity session table already refuses past. A copy farther behind its
// log than that refuses the read, and the request answers 503: anything that
// discloses fails closed on staleness.
var readFreshness = statelog.Freshness{Level: statelog.ReadStale, MaxLag: statelog.StallGrace}

// decide makes the decision for one ticket, from the rows, and remembers it.
func (h *Handler) decide(ctx context.Context, c artifacts.TicketClaims, key string) (
	verdict, artifactsdomain.Version, *refusal) {

	gen := h.verdicts.generation()
	// WHETHER THIS NODE ANSWERS FROM ITS OWN COPY, read before the router
	// is asked, so the instrument can tell a decision a peer made.
	local := h.local.ArtifactsCopyServes(ctx)
	got, v, ref := h.decideRows(ctx, c)
	if !local {
		outcome := peerServed
		switch {
		case ref != nil && ref.status == 503:
			outcome = peerUnavailable
		case ref != nil:
			outcome = peerRefused
		}
		h.metrics.Add(metrics.ArtifactsAuthzPeerChecks, 1, metrics.Attrs{"outcome": outcome})
	}
	if ref != nil {
		return verdict{}, artifactsdomain.Version{}, ref
	}
	h.versions.put(versionKey{c.Artifact, c.Version}, v)
	h.verdicts.put(key, got, gen)
	return got, v, nil
}

// decideRows is the decision itself.
//
// # The order is the contract
//
// THE CREDENTIAL FIRST: a ticket whose credential ended is refused before a
// single artifact row is read, and one whose credential now acts as another
// record (a person moved to another seat) is refused too, since the ticket
// was minted for the record it no longer is.
//
// THEN THE ARTIFACT, through the router under the staleness bound: gone,
// purged or trashed is refused; a copy older than the ticket itself (its
// access epoch below the one the ticket was minted at) has not applied what
// the minting node had, so it answers 503 rather than decide from rows that
// may predate a narrowing.
//
// THEN THE AUTHORITY TABLE, `get_artifact` for the re-composed principal over
// the rows just read: owner, shares, units through the chart, the lead arm,
// and the admin path. An unknown is a 503, never a refusal.
//
// LAST THE VERSION: one past retention is gone, whatever the share says.
func (h *Handler) decideRows(ctx context.Context, c artifacts.TicketClaims) (
	verdict, artifactsdomain.Version, *refusal) {

	p, how := h.creds.Principal(ctx, c.Lineage)
	switch how {
	case iam.Resolved:
	case iam.Unknown:
		log.WarnContext(ctx, "artifacts_serve_identity_unavailable", "artifact", c.Artifact.String())
		return verdict{}, artifactsdomain.Version{}, unavailable(nil)
	default:
		return verdict{}, artifactsdomain.Version{}, refused()
	}
	if iam.RecordOwner(p) != c.Principal {
		log.InfoContext(ctx, "artifacts_serve_record_moved", "artifact", c.Artifact.String(),
			"ticket_record", c.Principal, "record", iam.RecordOwner(p))
		return verdict{}, artifactsdomain.Version{}, refused()
	}
	head, _, err := h.reads.Artifact(ctx, c.Artifact, readFreshness)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return verdict{}, artifactsdomain.Version{}, refused()
	case err != nil:
		log.WarnContext(ctx, "artifacts_serve_rows_unavailable", "artifact", c.Artifact.String(),
			"error", err)
		return verdict{}, artifactsdomain.Version{}, unavailable(err)
	case head.Status != artifactsdomain.StatusLive || !head.Kind.Framed():
		return verdict{}, artifactsdomain.Version{}, refused()
	case head.AccessEpoch < c.AccessEpoch:
		log.WarnContext(ctx, "artifacts_serve_copy_behind_ticket", "artifact", c.Artifact.String(),
			"copy_epoch", head.AccessEpoch, "ticket_epoch", c.AccessEpoch)
		return verdict{}, artifactsdomain.Version{}, unavailable(nil)
	}
	d := authz.Decide(ctx, p, authz.ActionArtifactRead, authz.Object{
		Kind:         authz.KindArtifact,
		ID:           c.Artifact.String(),
		Owner:        head.Owner,
		Shares:       head.Shares,
		OwnerIsAgent: head.OwnerKind == iam.ActorAgent,
	}, h.chart, h.now())
	switch {
	case d.Unknown():
		log.WarnContext(ctx, "artifacts_serve_undecided", "artifact", c.Artifact.String(),
			"error", d.Err)
		return verdict{}, artifactsdomain.Version{}, &refusal{status: 503, reason: reasonStale,
			retry: time.Duration(authz.RetryUndecidedSeconds) * time.Second}
	case !d.Allowed:
		return verdict{}, artifactsdomain.Version{}, refused()
	}
	v, err := h.reads.Version(ctx, c.Artifact, c.Version)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return verdict{}, artifactsdomain.Version{}, refused()
	case err != nil:
		log.WarnContext(ctx, "artifacts_serve_rows_unavailable", "artifact", c.Artifact.String(),
			"version", c.Version, "error", err)
		return verdict{}, artifactsdomain.Version{}, unavailable(err)
	}
	spec, _ := head.Kind.Spec()
	return verdict{artifact: c.Artifact, number: c.Version, at: h.now(), fresh: true,
		documents: spec.Documents}, v, nil
}
