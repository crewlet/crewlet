package queries

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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
	live := map[string][]coord.Lease{}
	for _, prefix := range []string{coord.NodePrefix, coord.SeatPrefix, coord.WorkerPrefix} {
		leases, err := s.Coord.ListLive(ctx, prefix)
		if err != nil {
			return nil, err
		}
		live[prefix] = leases
	}
	nodes, seats, duties := live[coord.NodePrefix], live[coord.SeatPrefix], live[coord.WorkerPrefix]
	now := s.clock()

	held := map[string]int{}
	seatRows := make([]map[string]any, 0, len(seats))
	for _, lease := range seats {
		node := nodeOf(lease.Owner)
		held[node]++
		seatRows = append(seatRows, map[string]any{
			"handle":     strings.TrimPrefix(lease.Resource, coord.SeatPrefix),
			"node":       node,
			"owner":      lease.Owner,
			"epoch":      lease.Epoch,
			"expires_in": secondsLeft(lease.ExpiresAt, now),
		})
	}
	sort.Slice(seatRows, func(i, j int) bool {
		return seatRows[i]["handle"].(string) < seatRows[j]["handle"].(string)
	})

	applied := s.applyStatus(ctx)
	nodeRows := make([]map[string]any, 0, len(nodes))
	for _, lease := range nodes {
		id := strings.TrimPrefix(lease.Resource, coord.NodePrefix)
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
		status := applied[id]
		row["config_epoch"] = status.Epoch
		row["config_status"] = status.Status
		row["config_error"] = status.Error
		// WHEN this node last reported. Without it a node that stopped
		// reporting is indistinguishable from one that reported the same
		// epoch a second ago — and the one that stopped is exactly the
		// one an operator is looking for.
		row["config_reported_at"] = isoOrEmpty(status.UpdatedAt)
		// WHAT THAT NODE IS DOING, off its own presence heartbeat.
		// Absent when the node published none — an older build, or one
		// whose engine is not co-located — and absent is NOT zero: a
		// confident 0 would draw an idle row for a process that is
		// simply not saying. See rewrite/decisions/501-node-runtime.md.
		if live, ok := coord.StatusFromMeta(lease.Meta); ok {
			row["in_flight"] = live.InFlight
			row["draining"] = live.Draining
			row["started_at"] = isoOrEmpty(live.StartedAt)
			if live.Posture != "" {
				row["posture"] = live.Posture
			}
		}
		nodeRows = append(nodeRows, row)
	}
	sort.Slice(nodeRows, func(i, j int) bool {
		return nodeRows[i]["id"].(string) < nodeRows[j]["id"].(string)
	})

	dutyRows := make([]map[string]any, 0, len(duties))
	for _, lease := range duties {
		dutyRows = append(dutyRows, map[string]any{
			"duty":       strings.TrimPrefix(lease.Resource, coord.WorkerPrefix),
			"node":       nodeOf(lease.Owner),
			"expires_in": secondsLeft(lease.ExpiresAt, now),
		})
	}
	sort.Slice(dutyRows, func(i, j int) bool {
		return dutyRows[i]["duty"].(string) < dutyRows[j]["duty"].(string)
	})

	return map[string]any{
		"nodes": nodeRows, "seats": seatRows, "duties": dutyRows,
		"unplaceable":    s.unplaceable(nodeRows, seatRows),
		"unmanned_roles": unmannedRoles(nodeRows),
		"this_node":      s.NodeID,
		// What the fleet is converging ON, so a lagging node reads as
		// "3 epochs behind 41" rather than as a number with nothing to
		// compare it to.
		"target_epoch": s.targetEpoch(ctx),
	}, nil
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

// targetEpoch is the newest activation, or 0 when there is none or it cannot
// be read. Zero is safe here because the client compares against it only when
// non-zero — an unknown target renders as no comparison rather than as "every
// node is 41 epochs behind".
func (s Sources) targetEpoch(ctx context.Context) int64 {
	if s.Plane == nil {
		return 0
	}
	target, found, err := s.Plane.Target(ctx)
	if err != nil {
		log.WarnContext(ctx, "fleet_target_epoch_failed", "error", err)
		return 0
	}
	if !found {
		return 0
	}
	return target.Epoch
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
	sort.Slice(out, func(i, j int) bool {
		return out[i]["handle"].(string) < out[j]["handle"].(string)
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
	sort.Strings(out)
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
