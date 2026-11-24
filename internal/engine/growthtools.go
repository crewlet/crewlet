package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/admob"
	"github.com/crewlet/crewlet/internal/adsense"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/appleads"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/googleads"
	"github.com/crewlet/crewlet/internal/googleanalytics"
	"github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/metaads"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/searchconsole"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The growth tools on this node (ADR-0170): each product reads the running
// document at every call, so turning a seat, a key or the integration off
// stops the next call; keys are resolved from the secret store and held in
// memory only.

// growthActClass is the lease class an applied growth act holds: one resource
// per ad account, app or team, so two allowed acts never apply on the same
// stale state ([growthActResource]).
const growthActClass coord.Class = "growth_act"

// growthActResource is the lease an act on resource holds, a product's kind
// and account: the SHA-256 of resource, of one width whatever the account.
// An account is what its integration's configuration accepts (a Search
// Console site has no bound), and nothing reads the lease back by name (its
// release and its log line name resource itself), so no configuration's
// grammar is tied to what a lease's record holds (coord.MaxLeaseNameBytes).
func growthActResource(resource string) string {
	sum := sha256.Sum256([]byte(resource))
	return growthActClass.Resource(hex.EncodeToString(sum[:]))
}

const (
	// growthActLeaseTTL outlives everything an apply does under the lease,
	// which growth.ApplyBound cuts, by half a minute for the lease store's
	// clock and the release's round trip; it lapses an apply its node lost.
	growthActLeaseTTL = growth.ApplyBound + 30*time.Second
	// growthActWait is how long an apply waits for another on the same
	// target before the agent is told to call again.
	growthActWait = 30 * time.Second
	// growthActPoll is how often a waiting apply asks for the lease.
	growthActPoll = 200 * time.Millisecond
)

var (
	_ builtin.GrowthProduct = (*googleanalytics.Product)(nil)
	_ builtin.GrowthProduct = (*admob.Product)(nil)
	_ builtin.GrowthProduct = (*adsense.Product)(nil)
	_ builtin.GrowthProduct = (*googleads.Product)(nil)
	_ builtin.GrowthProduct = (*searchconsole.Product)(nil)
	_ builtin.GrowthProduct = (*googleplay.Product)(nil)
)

// GrowthToolDeps are the growth tools' dependencies on this node, or the zero
// value (which registers none) without an engine.
func GrowthToolDeps(e *Engine) builtin.GrowthDeps {
	if e == nil {
		return builtin.GrowthDeps{}
	}
	return builtin.GrowthDeps{Products: e.growthProducts(), Requesters: turnRequesters{engine: e},
		Chart: ChartAuthorityOf(e), Audit: e.GrowthAudit()}
}

// growthProducts are this node's growth products, made once.
func (e *Engine) growthProducts() []builtin.GrowthProduct {
	e.growthOnce.Do(func() {
		e.growth = []builtin.GrowthProduct{
			metaads.New(metaads.Deps{Company: e.runningConfig, Resolve: e.Resolve, Audit: e.GrowthAudit(),
				Leases: growthLeases{engine: e}, Renewals: metaAdsRenewals{engine: e}, Signals: e.GrowthSignals(),
				Identity: e.seatIdentity(seataccess.LaneMetaAds)}),
			appstoreconnect.New(appstoreconnect.Deps{Company: e.runningConfig, Resolve: e.Resolve,
				Audit: e.GrowthAudit(), Leases: growthLeases{engine: e}, Signals: e.GrowthSignals(),
				Identity: e.seatIdentity(seataccess.LaneAppStoreConnect)}),
			appleads.New(appleads.Deps{Company: e.runningConfig, Resolve: e.Resolve,
				Audit: e.GrowthAudit(), Leases: growthLeases{engine: e}, Signals: e.GrowthSignals()}),
			googleanalytics.New(e.runningConfig, e.resolver, e, e.GrowthAudit(), growthLeases{engine: e}, nil),
			e.adMob(),
			e.adSense(),
			googleads.New(e.runningConfig, e.resolver, e, e.GrowthAudit(), growthLeases{engine: e}, nil),
			searchconsole.New(e.runningConfig, e.resolver, e, e.GrowthAudit(), growthLeases{engine: e}, nil, nil),
			googleplay.New(googleplay.Deps{Company: e.runningConfig, Env: e.resolver, Tokens: e, Audit: e.GrowthAudit(),
				Leases: growthLeases{engine: e}, Reader: e, Media: growthMedia{engine: e}}),
		}
	})
	return e.growth
}

// seatReader is a growth product whose agents' access session offers what the
// product read at its vendor: App Store Connect's apps.
type seatReader interface {
	Catalog() growth.Catalog
	SeatReads(ctx context.Context, fresh bool) seataccess.PassReads
}

// seatReads is what this node's growth product for kind read at its vendor for
// kind's access session to offer, read again where it is stale; nil for a kind
// whose session reads for itself.
func (e *Engine) seatReads(ctx context.Context, kind integration.Kind) seataccess.PassReads {
	for _, p := range e.growthProducts() {
		if r, ok := p.(seatReader); ok && r.Catalog().Kind == string(kind) {
			return r.SeatReads(ctx, false)
		}
	}
	return nil
}

// AppStoreConnect is this node's App Store Connect product.
func (e *Engine) AppStoreConnect() *appstoreconnect.Product {
	for _, p := range e.growthProducts() {
		if a, ok := p.(*appstoreconnect.Product); ok {
			return a
		}
	}
	return nil
}

// AppStoreConnectReach is what this node last read of each App Store Connect
// connection's apps, without asking Apple ([appstoreconnect.Product.HeldReach]).
func (e *Engine) AppStoreConnectReach() appstoreconnect.Reach {
	if p := e.AppStoreConnect(); p != nil {
		return p.HeldReach()
	}
	return nil
}

// GrowthCards words each held growth act's decision card through its product.
func (e *Engine) GrowthCards() growthCards { return growthCards{engine: e} }

// growthCards is [mcpapi.GrowthCards] on this node.
type growthCards struct{ engine *Engine }

// Card is run's held act's card as its product reads its target now; nil for
// a tool no product of this node lists.
func (c growthCards) Card(ctx context.Context, run sandbox.PendingRun, args map[string]any) (*growth.Card, error) {
	held := run.HeldCall
	if held == nil {
		return nil, nil
	}
	for _, p := range c.engine.growthProducts() {
		if _, ok := p.Catalog().Tool(held.Tool); !ok {
			continue
		}
		card, err := p.Card(ctx, growth.Call{Handle: run.AgentHandle, Turn: run.TurnID, Tool: held.Tool, Args: args,
			HeldCall: held.ID}, held.Binding)
		if err != nil {
			return nil, err
		}
		card.LapsesAt = held.LapsesAt
		return &card, nil
	}
	return nil, nil
}

// runningConfig is the running document, empty before one applies.
func (e *Engine) runningConfig() *config.Company {
	if c := e.Company(); c != nil && c.Config != nil {
		return c.Config
	}
	return &config.Company{}
}

// growthLeases hold a growth act's lease across the fleet.
type growthLeases struct{ engine *Engine }

// Hold takes resource's lease, waiting up to [growthActWait] for another
// node's apply, and answers its release.
func (l growthLeases) Hold(ctx context.Context, resource string) (func(), error) {
	e := l.engine
	full := growthActResource(resource)
	owner := e.selfNode() + "/" + uuid.NewString()
	deadline := e.now().Add(growthActWait)
	for {
		lease, _, err := e.backends.Coord.TryAcquire(ctx, full, coord.AcquireOptions{Owner: owner,
			TTL: growthActLeaseTTL, Ungated: true})
		if err != nil {
			return nil, fmt.Errorf("engine: take %s's lease: %w", resource, err)
		}
		if lease != nil {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), growthActWait)
				defer cancel()
				if _, err := e.backends.Coord.Release(releaseCtx, full, owner, lease.Epoch); err != nil {
					log.WarnContext(ctx, "growth_act_lease_kept", "resource", resource, "error", err,
						"detail", "it lapses on its own")
				}
			}, nil
		}
		if !e.now().Before(deadline) {
			return nil, fmt.Errorf("%w: engine: another act on %s is being applied", growth.ErrActHeld, resource)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(growthActPoll):
		}
	}
}
