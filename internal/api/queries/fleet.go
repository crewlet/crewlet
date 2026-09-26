package queries

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// The fleet, read from the LEASE TABLE rather than from a fan-out of /health
// probes.
//
// /health answers about the node that served it, so behind a load balancer a
// refresh tells a different story each time. The lease table is the one place
// that knows which node holds what, and every node reads the same rows.

// fleet answers the fleet question.
func (s Sources) fleet(ctx context.Context, _ Params) (any, error) {
	// The three listings through ONE error path. Written out, they were
	// three identical checks of which a test could only ever exercise the
	// first — and three copies of "the lease table IS the fleet, so an
	// unreadable one must not answer an empty company" is three chances
	// for one of them to stop saying it.
	live := map[coord.Class][]coord.Lease{}
	for _, class := range []coord.Class{coord.ClassNode, coord.ClassSeat, coord.ClassWorker} {
		leases, err := s.Coord.ListLive(ctx, class)
		if err != nil {
			return nil, err
		}
		live[class] = leases
	}
	nodes, seats, duties := live[coord.ClassNode], live[coord.ClassSeat], live[coord.ClassWorker]
	now := s.clock()

	held := map[string]int{}
	seatRows := make([]map[string]any, 0, len(seats))
	for _, lease := range seats {
		node := nodeOf(lease.Owner)
		held[node]++
		seatRows = append(seatRows, map[string]any{
			"handle":     nameIn(coord.ClassSeat, lease.Resource),
			"node":       node,
			"owner":      lease.Owner,
			"epoch":      lease.Epoch,
			"expires_in": secondsLeft(lease.ExpiresAt, now),
		})
	}
	slices.SortFunc(seatRows, func(a, b map[string]any) int {
		return cmp.Compare(a["handle"].(string), b["handle"].(string))
	})

	applied := s.applyStatus(ctx)
	target := s.activation(ctx)
	nodeRows := make([]map[string]any, 0, len(nodes))
	for _, lease := range nodes {
		id := nameIn(coord.ClassNode, lease.Resource)
		profile := placement.FromMeta(id, lease.Meta)
		row := map[string]any{
			"id":         id,
			"roles":      profile.Roles.Names(),
			"labels":     profile.Labels,
			"owner":      lease.Owner,
			"protocol":   lease.Protocol,
			"seats":      held[id],
			"expires_in": secondsLeft(lease.ExpiresAt, now),
		}
		// THE SHARE OF THE OBJECT STORE THIS NODE OFFERS, off its own
		// presence — absent on a node that holds none, never a 0 that
		// reads as a data node offering nothing.
		if profile.ObjectWeight > 0 {
			row["object_weight"] = profile.ObjectWeight
		}
		status := applied[id]
		row["config_epoch"] = status.Epoch
		row["config_status"] = status.Status
		row["config_error"] = status.Error
		// WHICH REVISION THIS NODE IS ON, which the epoch does not say.
		//
		// Carried on the apply record for exactly this reason — its own
		// doc: "the fleet view is read while nodes are mid-transition,
		// and a node still on the previous revision is exactly what an
		// operator is looking for." It was written by every node, stored
		// by the plane, read into this loop, and dropped here, so the
		// screen could compare epoch numbers and never name what they
		// stood for.
		row["config_revision_id"] = status.RevisionID
		// WHEN this node last reported. Without it a node that stopped
		// reporting is indistinguishable from one that reported the same
		// epoch a second ago — and the one that stopped is exactly the
		// one an operator is looking for.
		row["config_reported_at"] = isoOrEmpty(status.UpdatedAt)
		// WHAT THAT NODE IS DOING, off its own presence heartbeat.
		// Absent when the node published none (a peer running a build
		// older than the field), and absent is NOT zero: a confident 0
		// would draw an idle row for a process that is simply not saying.
		if live, ok := coord.StatusFromMeta(lease.Meta); ok {
			row["in_flight"] = live.InFlight
			row["draining"] = live.Draining
			row["started_at"] = isoOrEmpty(live.StartedAt)
			if live.Posture != "" {
				row["posture"] = live.Posture
			}
			// HOW FAR THIS NODE'S OWN COPY HAS COME UP, which is a
			// different question from its config epoch: a node can
			// hold the current revision and still be hydrating the
			// state it derives from the log, and only one of those
			// two makes its seats servable.
			//
			// ABSENT RATHER THAN ZERO when the node published none —
			// the meta only carries them when the total is non-zero
			// — for the reason `in_flight` is absent: a confident 0
			// of 0 reads as "ready" for a process that is simply not
			// saying.
			if live.ProjectionsTotal > 0 {
				row["projections_ready"] = live.ProjectionsReady
				row["projections_total"] = live.ProjectionsTotal
			}
		}
		nodeRows = append(nodeRows, row)
	}
	slices.SortFunc(nodeRows, func(a, b map[string]any) int {
		return cmp.Compare(a["id"].(string), b["id"].(string))
	})

	dutyRows := make([]map[string]any, 0, len(duties))
	for _, lease := range duties {
		dutyRows = append(dutyRows, map[string]any{
			"duty":       nameIn(coord.ClassWorker, lease.Resource),
			"node":       nodeOf(lease.Owner),
			"expires_in": secondsLeft(lease.ExpiresAt, now),
		})
	}
	slices.SortFunc(dutyRows, func(a, b map[string]any) int {
		return cmp.Compare(a["duty"].(string), b["duty"].(string))
	})

	out := map[string]any{
		"nodes": nodeRows, "seats": seatRows, "duties": dutyRows,
		"unplaceable":    s.unplaceable(nodeRows, seatRows),
		"unmanned_roles": unmannedRoles(nodeRows),
		"this_node":      s.NodeID,
		// What the fleet is converging ON, so a lagging node reads as
		// "3 epochs behind 41" rather than as a number with nothing to
		// compare it to.
		// ONE READ, TWO FIELDS. `target_epoch` is what every node row is
		// compared against; `activation` is what that number STANDS FOR
		// — which revision, activated when, with the summary whoever
		// activated it wrote. An operator reading "node-2 is two epochs
		// behind" could not see any of the second half.
		//
		// Read once rather than once per field: they are the same
		// pointer, and two reads of it inside one answer can disagree
		// while an activation lands between them — a screen saying
		// "target 42" beside "revision r-41" describes a fleet that
		// never existed.
		"target_epoch": target.epoch,
		"activation":   target.detail,
	}
	if s.Objects != nil {
		out["objects"] = s.objectMap(ctx)
	}
	return out, nil
}

// ObjectMapReader is the stored placement map, read as every node reads it.
type ObjectMapReader interface {
	ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error)
}

// objectMap is the fleet's placement map as the fleet view shows it.
//
// THE STORED MAP, not this node's cached copy, for the reason the whole view
// reads the lease table: every node gives the same answer. And THREE STATES
// NAMED APART, never folded into an empty list — a map the store would not
// give up (`available: false`), a fleet with no map yet (`placed: false`,
// where no upload can land), and one a newer build wrote that this one cannot
// read (`unreadable: true`) — because each sends an operator somewhere
// different, and "no members" reads as the second of them whichever it was.
func (s Sources) objectMap(ctx context.Context) map[string]any {
	rec, found, err := s.Objects.ObjectMap(ctx)
	switch {
	case err != nil:
		return map[string]any{"available": false}
	case !found:
		return map[string]any{"available": true, "placed": false}
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		return map[string]any{"available": true, "placed": true, "unreadable": true}
	}
	members := make([]map[string]any, 0, len(state.Map.Members))
	for _, m := range state.Map.Members {
		row := map[string]any{"node": m.Node, "weight": m.Weight}
		if since, gone := state.Absent[m.Node]; gone {
			row["absent_since"] = isoOrEmpty(since)
		}
		members = append(members, row)
	}
	// BOTH COUNTS: the replica count asked for, and how many members hold
	// each chunk now — fewer while the fleet has fewer data nodes than it
	// asks for, which is exactly the shortfall an operator is looking for.
	return map[string]any{
		"available": true, "placed": true,
		"epoch": state.Map.Epoch, "replicas": state.Map.Replicas,
		"copies": state.Map.Size(), "members": members,
	}
}

// applyStatus is each node's last config outcome, keyed by node id.
//
// DEGRADES rather than fails: a fleet answer without the config column is
// still the answer to "which node holds what", and refusing the whole view
// because one of its columns is unreadable would blank the screen an operator
// opens when nodes are dying.
func (s Sources) applyStatus(ctx context.Context) map[string]coord.NodeApply {
	if s.Plane == nil {
		return nil
	}
	rows, err := s.Plane.Fleet(ctx)
	if err != nil {
		log.WarnContext(ctx, "fleet_apply_status_failed", "error", err)
		return nil
	}
	out := make(map[string]coord.NodeApply, len(rows))
	for _, row := range rows {
		out[row.NodeID] = row
	}
	return out
}

// activationTarget is the newest activation as this answer renders it.
//
// The epoch is 0 when there is none or it cannot be read, which is safe
// because the client compares against it only when non-zero — an unknown
// target renders as no comparison rather than as "every node is 41 epochs
// behind".
//
// The detail is NIL rather than an object of empty strings, because "nothing
// has ever been activated" and "the pointer could not be read" both leave a
// screen with nothing to render, and an empty object would have it render a
// revision named "" activated at the zero time. The two are told apart in the
// log, which is where an operator looks for a failure, rather than on a screen
// that has no remedy to offer for either.
type activationTarget struct {
	epoch  int64
	detail any
}

func (s Sources) activation(ctx context.Context) activationTarget {
	if s.Plane == nil {
		return activationTarget{}
	}
	target, found, err := s.Plane.Target(ctx)
	switch {
	case err != nil:
		log.WarnContext(ctx, "fleet_activation_unavailable", "error", err)
		return activationTarget{}
	case !found:
		return activationTarget{}
	}
	return activationTarget{epoch: target.Epoch, detail: map[string]any{
		"epoch":       target.Epoch,
		"revision_id": target.RevisionID,
		"at":          isoOrEmpty(target.At),
		"summary":     target.Summary,
	}}
}

// unplaceable are the seats the company declares that no live node may run.
//
// The question a fleet view exists to answer and the one a list of leases
// cannot: a seat pinned to a label no node carries is not "unclaimed yet", it
// is unclaimable, and it stays that way until somebody changes the config or
// starts a node that matches.
func (s Sources) unplaceable(nodes, seats []map[string]any) []map[string]any {
	if s.Company == nil {
		return []map[string]any{}
	}
	company := s.Company()
	if company == nil {
		return []map[string]any{}
	}
	organization, err := company.Organization()
	if err != nil {
		log.Warn("fleet_unplaceable_failed", "error", err)
		return []map[string]any{}
	}
	claimed := make(map[string]bool, len(seats))
	for _, seat := range seats {
		claimed[seat["handle"].(string)] = true
	}
	profiles := make([]placement.NodeProfile, 0, len(nodes))
	for _, node := range nodes {
		roles, err := placement.ParseRoles(node["roles"].([]string))
		if err != nil {
			// A role this build does not know, which is a peer running a
			// newer one. Its own seats are its business; what this answer
			// must not do is conclude that a seat is unplaceable because
			// the node that can run it uses a word we have not learned.
			log.Warn("fleet_unknown_node_role", "node", node["id"], "error", err)
			roles = placement.DefaultRoles()
		}
		profiles = append(profiles, placement.NodeProfile{
			ID: node["id"].(string), Roles: roles,
			Labels: node["labels"].(map[string]string),
		})
	}

	out := []map[string]any{}
	for role := range organization.AllRoles() {
		if !role.IsAgent() {
			continue
		}
		handle := role.Handle()
		if claimed[handle] {
			continue
		}
		if placementFits(role.Placement, profiles) {
			// Unclaimed but placeable: a node could take it, so this is
			// a moment rather than a fault.
			continue
		}
		out = append(out, map[string]any{
			"handle":    handle,
			"placement": role.Placement,
		})
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		return cmp.Compare(a["handle"].(string), b["handle"].(string))
	})
	return out
}

// placementFits reports whether any live node may run a seat with this
// placement.
//
// A node that does not run seats does not count, however well it matches: the
// placement selector and the node's ROLE are different constraints, and a
// company whose only label-matching node is an ingress-only node has a seat
// nothing will ever claim.
func placementFits(p placement.SeatPlacement, nodes []placement.NodeProfile) bool {
	for _, node := range nodes {
		if node.RunsSeats() && p.Matches(node.ID, node.Labels) {
			return true
		}
	}
	return false
}

// unmannedRoles are the node roles no live node is running.
//
// A company whose workers role is unmanned still answers webhooks and still
// runs turns; what it stops doing is every scheduled and background duty, with
// no error anywhere. That silence is the whole reason this is a field.
func unmannedRoles(nodes []map[string]any) []string {
	running := map[string]bool{}
	for _, node := range nodes {
		for _, role := range node["roles"].([]string) {
			running[role] = true
		}
	}
	out := []string{}
	for role := range placement.DefaultRoles() {
		if !running[string(role)] {
			out = append(out, string(role))
		}
	}
	slices.Sort(out)
	return out
}

// nodeOf reads the node id out of an owner incarnation ({node}:{random}).
//
// The incarnation is what fences a restarted process; the node id is what an
// operator recognises and what everything else is keyed on.
func nodeOf(owner string) string {
	id, _, _ := strings.Cut(owner, ":")
	return id
}

// secondsLeft is how long a lease has, floored at zero.
//
// Negative would be a lease already gone, which the live listing does not
// return — but a clock that moved between the query and this line would
// produce one, and "-3 seconds left" is not something to render.
func secondsLeft(expires, now time.Time) int {
	if expires.IsZero() {
		return 0
	}
	return max(int(expires.Sub(now)/time.Second), 0)
}

func isoOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// nameIn is a lease's name within its class, for a listing that already asked
// for that class.
//
// Through [coord.Class.Name] rather than a bare TrimPrefix: the prefix is the
// class AND its separator, and trimming the class alone leaves the separator
// on the front of every handle the dashboard renders.
func nameIn(class coord.Class, resource string) string {
	name, _ := class.Name(resource)
	return name
}
