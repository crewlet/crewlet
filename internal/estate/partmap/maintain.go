package partmap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("estate")

// The maintainer: [Next], run once a tick against the stored map and written
// back by compare-and-set. It is the object map's maintainer
// (objstore/upkeep) over this map's record — the same reads, the same pacing
// and the same answer to a lost race — and everything it decides is [Next]'s.
//
// # What it writes, and when it writes nothing
//
// A tick writes the stored map only when [Next] says something changed, and
// it writes a FIRST map only at a partitioned layout, for a company, for a
// fleet with a member, having provisioned the layout's logs first. Under
// layout 0 — every data node holding the whole estate, which is the layout
// this build runs — there is no map to maintain and none may be created, so a
// tick reads the leases, the store and the company and writes nothing at all:
// the duty's own lease is the only record its holder keeps.

// MapStore is what the maintainer needs from the coordination store.
type MapStore interface {
	EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error)
	CreateEstateMap(ctx context.Context, value []byte) (coord.EstateMapRecord, bool, error)
	UpdateEstateMap(ctx context.Context, value []byte, version uint64) (coord.EstateMapRecord, bool, error)
}

// Provisioner creates every log stream a layout has, idempotently — or refuses,
// naming why, where this build cannot create them.
//
// # Streams exist before anyone joins
//
// A joiner opens its partition's logs and an applier consumes them, so a
// partition's logs have to exist before a map names a holder for them.
// Creating them is the MAINTAINER's work rather than each node's at boot: at
// the default layout a node would otherwise walk hundreds of stream creates on
// every boot, and a fleet would repeat them on every node.
//
// ONCE PER LINEAGE AND LAYOUT PER TENURE OF THE DUTY, and on reading a map as
// well as on creating one. A map's logs can be missing while the map is not —
// a broker store restored from before the layout was created, a stream an
// operator deleted — and every joiner the map names would then fail to open
// them with nothing recreating them. Creating them on every tick instead is
// hundreds of stream lookups every fifteen seconds for an answer that almost
// never changes; once per tenure, a new holder of the duty — which is what a
// restart, a failover or a lost lease makes — re-creates whatever is missing
// before it moves a holder, and [Maintainer.Forget] is how a node that gave
// the duty up and takes it back again counts as new.
type Provisioner func(ctx context.Context, l statelog.Layout) error

// MaintainerOptions are a maintainer's dependencies.
type MaintainerOptions struct {
	Store MapStore

	// Live answers every live estate lease ([PresenceOf]).
	Live func(ctx context.Context) ([]Presence, error)

	// Company answers what the company's estate block says, stamped with
	// the activation it came from, and false while this node has none —
	// then a tick changes neither the copies nor the failure domain, and
	// writes no first map.
	Company func() (membership.Company, bool)

	// Layout is the layout a FIRST map is created at: the layout this node
	// runs. A map that exists keeps its own. Under layout 0 no map is ever
	// created — every data node holds that layout's one partition whole.
	Layout statelog.Layout

	// Provision creates a map's logs before the maintainer writes the first
	// map at a layout or moves a holder of one it read ([Provisioner]).
	// REQUIRED at every layout: a node running layout 0 creates no map, but
	// one another node created is a map it maintains, and the logs that map
	// names are the ones its joiners open.
	Provision Provisioner

	// Now stamps what the record shows an operator and is what a hold
	// expires against. Injected for tests.
	Now func() time.Time
}

// Maintainer keeps the stored estate map in step with the fleet. A FLEET
// SINGLETON: the caller runs [Maintainer.Tick] only while it holds the duty,
// once per [membership.TickInterval] after a tick that found a map — and
// [Maintainer.Converge] between two of them when an estate lease changes — and
// every write is a compare-and-set, so a pass that lost the duty mid-write
// loses the race rather than overwriting its successor.
type Maintainer struct {
	opts MaintainerOptions

	mu sync.Mutex
	// provisioned is the map whose logs this maintainer created during the
	// current tenure of the duty, the zero value before it has.
	provisioned provisionedMap
}

// provisionedMap names a map's logs: its lineage and its layout.
type provisionedMap struct {
	generation uuid.UUID
	layout     int
}

// NewMaintainer builds a maintainer, refusing one that could name holders of a
// layout whose logs nothing would create.
func NewMaintainer(opts MaintainerOptions) (*Maintainer, error) {
	switch {
	case opts.Store == nil || opts.Live == nil || opts.Company == nil:
		return nil, errors.New("estate/partmap: a maintainer needs a store, the live " +
			"estate leases and the company")
	case opts.Layout.Validate() != nil:
		return nil, fmt.Errorf("estate/partmap: a maintainer needs the layout this node "+
			"runs: %w", opts.Layout.Validate())
	case opts.Provision == nil:
		return nil, errors.New("estate/partmap: a maintainer needs a provisioner: every " +
			"map it creates or reads names partitions whose logs must exist before a " +
			"node is named to join them")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Maintainer{opts: opts}, nil
}

// errNewerMap is a stored map this build cannot rewrite, which it must not
// overwrite: a newer build wrote it, and a map rewritten in an older shape
// would drop whatever that build added.
var errNewerMap = errors.New("estate/partmap: the stored estate map is one this build cannot rewrite")

// TickResult is what one tick found, for the caller to pace the next by.
type TickResult struct {
	// Mapped is whether the stored map existed when the tick read it — the
	// STORE's answer — and so whether the tick counted one tick of every
	// open absence against it. The next tick must then come no sooner than
	// [membership.TickInterval], whether or not this one's write landed: a
	// write that answered an error may still have.
	Mapped bool

	// Awaited is whether the tick found NO map where one is wanted: this
	// node runs a partitioned layout, so until a first map is written no
	// partition has a holder and nothing in the estate can be served. The
	// caller may tick again sooner then, and should. False under layout 0,
	// where no map is wanted, and whenever a map was found.
	Awaited bool
}

// Tick brings the stored map up to date, once — and counts one tick of every
// open absence, which is why a tick that found a map ([TickResult.Mapped]) must
// be followed by the next only after [membership.TickInterval].
//
// AN INPUT IT COULD NOT READ CHANGES NOTHING: the leases, the store and a map
// this build cannot rewrite each end the tick with an error before anything is
// decided, so nothing is released or removed on an unknown (ADR-0005). A
// company this node does not have, or cannot apply, is a tick that changes
// neither the copies nor the failure domain.
func (m *Maintainer) Tick(ctx context.Context) (TickResult, error) {
	live, err := m.opts.Live(ctx)
	if err != nil {
		return TickResult{}, fmt.Errorf("estate/partmap: read the live estate leases: %w", err)
	}
	company := m.company(ctx)
	rec, found, err := m.opts.Store.EstateMap(ctx)
	if err != nil {
		return TickResult{}, fmt.Errorf("estate/partmap: read the estate map: %w", err)
	}
	res := TickResult{Mapped: found, Awaited: !found && m.opts.Layout.Number != 0}
	var state MapState
	if found {
		if state, err = DecodeMapStateForUpdate(rec.Value); err != nil {
			return res, fmt.Errorf("%w: %w", errNewerMap, err)
		}
		// THE LOGS OF A MAP THAT EXISTS, before anything the tick decides
		// can name a joiner of them — once per tenure ([Provisioner]).
		if perr := m.provision(ctx, state.Map); perr != nil {
			return res, fmt.Errorf("estate/partmap: create the logs of layout %d, which the "+
				"estate map names: %w", state.Map.Layout.Number, perr)
		}
	}
	next, changed := Next(state, Input{
		Layout: m.opts.Layout, Live: live, Company: company, Now: m.opts.Now().UTC(),
	})
	if !changed {
		return res, nil
	}
	raw, err := next.Encode()
	if err != nil {
		return res, err
	}
	var won bool
	if found {
		_, won, err = m.opts.Store.UpdateEstateMap(ctx, raw, rec.Version)
	} else {
		// THE LAYOUT'S LOGS BEFORE THE MAP THAT NAMES THEIR HOLDERS: a
		// node named to join a partition opens its logs, and one that
		// found none would fail the join it was named for. Idempotent,
		// so a first map that lost its race below provisioned nothing
		// the winner's did not.
		if perr := m.opts.Provision(ctx, next.Map.Layout); perr != nil {
			return res, fmt.Errorf("estate/partmap: create layout %d's logs before its "+
				"first map: %w", next.Map.Layout.Number, perr)
		}
		_, won, err = m.opts.Store.CreateEstateMap(ctx, raw)
	}
	if err != nil {
		return res, fmt.Errorf("estate/partmap: write the estate map: %w", err)
	}
	if !won {
		// A LOST RACE: another holder or a gesture wrote first. The next
		// tick reads what it wrote and starts from there — this tick's
		// counts are lost with it, which delays a removal by a tick and
		// never hastens one.
		return res, nil
	}
	if !found {
		m.remember(next.Map)
	}
	logChanges(ctx, state, next)
	return res, nil
}

// Converge runs [Converge] — the holder convergence alone — against the stored
// map and writes the answer back by compare-and-set, reporting whether it
// wrote. The map's duty runs it BETWEEN ticks, once the live estate leases have
// changed and then held still ([LeaseKey]); it counts no absence, so it may run
// as often as they change, and it never creates a map.
//
// ON A TICK'S TERMS otherwise: an input it could not read, or a map this build
// cannot rewrite, changes nothing; the map's logs are made sure of first, once
// per tenure, because a copy it adopts may be a joiner that opens them; and a
// lost race is the other writer's, read again next time.
func (m *Maintainer) Converge(ctx context.Context) (bool, error) {
	live, err := m.opts.Live(ctx)
	if err != nil {
		return false, fmt.Errorf("estate/partmap: read the live estate leases: %w", err)
	}
	rec, found, err := m.opts.Store.EstateMap(ctx)
	switch {
	case err != nil:
		return false, fmt.Errorf("estate/partmap: read the estate map: %w", err)
	case !found:
		return false, nil
	}
	state, err := DecodeMapStateForUpdate(rec.Value)
	if err != nil {
		return false, fmt.Errorf("%w: %w", errNewerMap, err)
	}
	if perr := m.provision(ctx, state.Map); perr != nil {
		return false, fmt.Errorf("estate/partmap: create the logs of layout %d, which the "+
			"estate map names: %w", state.Map.Layout.Number, perr)
	}
	next, changed := Converge(state, live)
	if !changed {
		return false, nil
	}
	raw, err := next.Encode()
	if err != nil {
		return false, err
	}
	_, won, err := m.opts.Store.UpdateEstateMap(ctx, raw, rec.Version)
	if err != nil {
		return false, fmt.Errorf("estate/partmap: write the estate map: %w", err)
	}
	if !won {
		return false, nil
	}
	logChanges(ctx, state, next)
	return true, nil
}

// provision creates the logs of the map read, unless this tenure already has.
func (m *Maintainer) provision(ctx context.Context, read Map) error {
	want := provisionedMap{generation: read.Generation, layout: read.Layout.Number}
	m.mu.Lock()
	done := m.provisioned == want
	m.mu.Unlock()
	if done {
		return nil
	}
	if err := m.opts.Provision(ctx, read.Layout); err != nil {
		return err
	}
	m.remember(read)
	return nil
}

// remember records that a map's logs exist: provisioned by this tenure, or
// before the first map this tenure wrote.
func (m *Maintainer) remember(written Map) {
	m.mu.Lock()
	m.provisioned = provisionedMap{generation: written.Generation, layout: written.Layout.Number}
	m.mu.Unlock()
}

// Forget ends this maintainer's tenure of the duty: the next tick that finds a
// map creates its logs again, whatever it created before. The duty calls it on
// every turn that does not hold the duty, so a node that gave the duty up and
// took it back re-creates what may have been lost while another node held it.
func (m *Maintainer) Forget() {
	m.mu.Lock()
	m.provisioned = provisionedMap{}
	m.mu.Unlock()
}

// company is the company to apply this tick: none when this node has none, or
// when what it has cannot be applied — which is logged, because it is a
// configuration somebody has to fix.
func (m *Maintainer) company(ctx context.Context) membership.Company {
	c, ok := m.opts.Company()
	if !ok {
		return membership.Company{}
	}
	if err := c.Validate(); err != nil {
		log.WarnContext(ctx, "estate_map_company_refused", "error", refused(err),
			"detail", "the estate map keeps the copies and failure domain it has")
		return membership.Company{}
	}
	return c
}

// logChanges says what a written tick changed: who arrived and left, whose
// absence opened and closed ([membership.Diff]), and what the holder table
// did — as counts, one line, since a first map names hundreds of joins.
func logChanges(ctx context.Context, before, after MapState) {
	changes := membership.Diff(before.State, before.Map.Members, after.State, after.Map.Members)
	for _, a := range changes.Admitted {
		m := a.Member
		switch a.How {
		case membership.OnProbation:
			log.InfoContext(ctx, "estate_member_probation", "node", m.Node, "weight", m.Weight,
				"domain", m.Domain, "placed_after_ticks", a.PlacedAfterTicks,
				"detail", "a node removed for absence is back: what it holds is adopted, "+
					"and the map names it to hold more once it has stayed present and healthy")
		case membership.Joined:
			log.InfoContext(ctx, "estate_member_added", "node", m.Node, "weight", m.Weight,
				"domain", m.Domain)
		case membership.Trusted:
			log.InfoContext(ctx, "estate_member_trusted", "node", m.Node,
				"detail", "back from removal and present long enough: the map places on it again")
		}
	}
	for _, d := range changes.Removed {
		attrs := []any{"node", d.Node, "reason", string(d.Removal.Reason),
			"detail", d.Removal.Detail}
		if d.OnProbation {
			attrs = append(attrs, "on_probation", true)
		} else {
			attrs = append(attrs, "absent_ticks", d.AbsentTicks)
		}
		log.WarnContext(ctx, "estate_member_removed", attrs...)
	}
	for _, o := range changes.Absent {
		log.WarnContext(ctx, "estate_member_absent", "node", o.Node,
			"reason", string(o.Absence.Reason), "detail", o.Absence.Detail,
			"out_after_ticks", membership.OutTicks)
	}
	for _, node := range changes.Returned {
		log.InfoContext(ctx, "estate_member_returned", "node", node,
			"present_ticks", membership.StableTicks)
	}
	m := after.Map
	if m.Epoch == before.Map.Epoch && after.Balance == before.Balance {
		return
	}
	h := diffHolders(before.Map, m)
	attrs := []any{"epoch", m.Epoch, "generation", m.Generation.String(),
		"layout", m.Layout.Number, "members", len(m.Members), "replicas", m.Replicas,
		"copies", m.Size(), "failure_domain", m.FailureDomain,
		"joins_named", h.joined, "promoted", h.promoted, "retired", h.retired,
		"withdrawn", h.withdrawn, "restored", h.restored, "adopted", h.adopted,
		"removed", h.removed, "unserved", h.unserved}
	if after.Balance != before.Balance {
		attrs = append(attrs, "balance_tolerance", after.Balance.Tolerance,
			"balance_rounds", after.Balance.Rounds,
			"balance_deviation", after.Balance.Deviation,
			"balance_converged", after.Balance.Converged)
	}
	log.InfoContext(ctx, "estate_map_changed", attrs...)
}

// holderChanges counts what one write did to the holder table.
type holderChanges struct {
	// joined is holders added joining — a join named, or a copy a node was
	// still building adopted where the target wants it — and adopted is
	// holders added serving or leaving: a copy a node already had.
	joined, adopted int

	// promoted is joiners now serving, retired servers now leaving,
	// withdrawn joiners now leaving, and restored leavers serving again.
	promoted, retired, withdrawn, restored int

	// removed is holders gone from the table.
	removed int

	// unserved is the partitions the later map lists no server for.
	unserved int
}

// diffHolders counts what changed between two maps' holder tables, partition by
// partition.
func diffHolders(before, after Map) holderChanges {
	var out holderChanges
	was := map[string]map[string]HolderState{}
	for _, p := range before.Partitions {
		states := map[string]HolderState{}
		for _, h := range p.Holders {
			states[h.Node] = h.State
		}
		was[p.ID] = states
	}
	for _, p := range after.Partitions {
		prior := was[p.ID]
		serving := false
		for _, h := range p.Holders {
			serving = serving || h.State == Serving
			from, held := prior[h.Node]
			delete(prior, h.Node)
			switch {
			case !held && h.State == Joining:
				out.joined++
			case !held:
				out.adopted++
			case from == h.State:
			case from == Joining && h.State == Serving:
				out.promoted++
			case from == Serving && h.State == Leaving:
				out.retired++
			case from == Joining && h.State == Leaving:
				out.withdrawn++
			case from == Leaving && h.State == Serving:
				out.restored++
			}
		}
		out.removed += len(prior)
		if !serving {
			out.unserved++
		}
	}
	return out
}
