package presence

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// room is one artifact's presence on this node: the members this node holds
// itself (its sockets and its agents), what its peers last told it, and what
// its sockets were last shown.
//
// Every method is called with the hub's lock held.
type room struct {
	id artifacts.ID

	local  map[string]*localMember
	remote map[remoteKey]*remoteMember

	// told is, per peer node, the instant of the newest frame this room has
	// applied from it (the frame's Until, which the peer stamps from its
	// own clock), and departed the sessions a peer said left, at the
	// instant of the frame that said so. Together they let a frame that
	// arrives after newer ones from the same node (the join-time answer,
	// which waits out the ask's budget before it is applied) fill in only
	// what the room has not heard since, never undo it.
	told     map[string]time.Time
	departed map[remoteKey]time.Time

	// presented is what this node's sockets were last shown, by session:
	// the fan-out is the difference between it and what the room presents
	// now. counts is the large-room count they were last shown, nil while
	// the room presents everybody.
	presented map[string]artifacts.PresenceMember
	counts    *artifacts.PresenceCounts

	// truncated is the hysteresis: a room presents only its most recent
	// members once it passes [artifacts.PresenceRoomTop], and everybody
	// again only once it falls below [artifacts.PresenceRoomFull], so a room
	// hovering around the threshold does not flip on every join.
	truncated bool

	// outUpsert and outLeave are the local sessions changed or gone since
	// the last frame this node sent about the room.
	outUpsert map[string]struct{}
	outLeave  map[string]struct{}

	timer   *time.Timer
	flushed time.Time
}

// localMember is a member this node holds: a socket's, or an agent's tool
// session.
type localMember struct {
	member artifacts.PresenceMember

	// socket says a socket's session, which is what makes this node a
	// listener on the room; an agent's tool session sees nothing and so
	// listens to nothing.
	socket bool

	// touched is the member's last sign of life here: an agent leaves the
	// room [agentLinger] after it.
	touched time.Time
}

// remoteKey names a peer's member: the node that holds it and its session.
type remoteKey struct {
	node    string
	session string
}

// remoteMember is a member a peer holds, as it last said.
type remoteMember struct {
	member artifacts.PresenceMember
	heard  time.Time
}

func newRoom(id artifacts.ID) *room {
	return &room{id: id, local: map[string]*localMember{}, remote: map[remoteKey]*remoteMember{},
		told: map[string]time.Time{}, departed: map[remoteKey]time.Time{},
		presented: map[string]artifacts.PresenceMember{},
		outUpsert: map[string]struct{}{}, outLeave: map[string]struct{}{}}
}

// changed marks a local session's state as one the fleet must be told.
func (r *room) changed(session string) {
	delete(r.outLeave, session)
	r.outUpsert[session] = struct{}{}
}

// left marks a local session gone, for the fleet to be told.
func (r *room) left(session string) {
	delete(r.outUpsert, session)
	r.outLeave[session] = struct{}{}
}

// hasSockets reports whether one of this node's sockets is in the room.
func (r *room) hasSockets() bool {
	for _, m := range r.local {
		if m.socket {
			return true
		}
	}
	return false
}

// agentMember is the member seat's tool session holds in the room, or nil.
func (r *room) agentMember(seat string) *localMember {
	for _, m := range r.local {
		if !m.socket && m.member.Seat == seat {
			return m
		}
	}
	return nil
}

// localMembers is every member this node holds in the room, by session.
func (r *room) localMembers() []artifacts.PresenceMember {
	out := make([]artifacts.PresenceMember, 0, len(r.local))
	for _, s := range slices.Sorted(maps.Keys(r.local)) {
		out = append(out, r.local[s].member)
	}
	return out
}

// hear lands a peer's frame, reporting whether the room changed. A full frame
// replaces everything the room held from that node, unless it is OLDER than a
// frame the room already applied from that node.
//
// # A late full frame fills in; it never undoes
//
// Frames from one node arrive in the order it sent them, with one exception:
// the answer to this node's join-time ask is sealed when the peer answers and
// applied only once the ask has waited out its budget, by which time the
// peer's broadcasts have already said what changed since. Applied as a
// replacement, that answer deleted every member the peer gained in between,
// and the room showed them again only at the peer's next rebroadcast,
// [artifacts.PresenceRebroadcast] later: measured (internal/artifactspike,
// S6), a room of fifty joined at once across three nodes took fifteen seconds
// to show everybody everywhere, against the second the join objective allows.
// So a frame older than the newest one heard from its node deletes nothing,
// leaves a member it names alone where a newer frame spoke for that member,
// and does not bring back a member a newer frame said left. A leave is always
// applied: a session id is never reused, so a leave is final whenever it
// arrives.
func (r *room) hear(f Frame, now time.Time) bool {
	moved := false
	stale := f.Until.Before(r.told[f.Node])
	if f.Full && !stale {
		named := make(map[string]bool, len(f.Upsert))
		for _, m := range f.Upsert {
			named[m.Session] = true
		}
		for key := range r.remote {
			if key.node == f.Node && !named[key.session] {
				delete(r.remote, key)
				moved = true
			}
		}
	}
	for _, m := range f.Upsert {
		key := remoteKey{node: f.Node, session: m.Session}
		held, ok := r.remote[key]
		if stale {
			if _, gone := r.departed[key]; ok || gone {
				continue
			}
		}
		if !ok || held.member != m {
			moved = true
		}
		r.remote[key] = &remoteMember{member: m, heard: now}
	}
	for _, s := range f.Leave {
		key := remoteKey{node: f.Node, session: s}
		if at, ok := r.departed[key]; !ok || f.Until.After(at) {
			r.departed[key] = f.Until
		}
		if _, ok := r.remote[key]; ok {
			delete(r.remote, key)
			moved = true
		}
	}
	if !stale {
		r.told[f.Node] = f.Until
	}
	return moved
}

// expire drops the peers' members not heard for [artifacts.PresenceExpiry]
// (three missed rebroadcasts: their node went away without saying so) and this
// node's agents not touched for [agentLinger], reporting whether the room
// changed.
func (r *room) expire(now time.Time) bool {
	moved := false
	for key, m := range r.remote {
		if now.Sub(m.heard) >= artifacts.PresenceExpiry {
			delete(r.remote, key)
			moved = true
		}
	}
	// A DEPARTURE IS REMEMBERED UNTIL ITS FRAME STOPS BEING BELIEVED: any
	// frame sealed before it has an earlier Until and is refused unopened
	// by then, so none is left that could bring the member back.
	for key, until := range r.departed {
		if !now.Before(until) {
			delete(r.departed, key)
		}
	}
	for node, until := range r.told {
		if !now.Before(until) {
			delete(r.told, node)
		}
	}
	for s, m := range r.local {
		if !m.socket && now.Sub(m.touched) >= agentLinger {
			delete(r.local, s)
			r.left(s)
			moved = true
		}
	}
	return moved
}

// everybody is every member in the room, most recent first: what a large room
// takes its top from. A session two sources both name (a node restarted under
// another id, a frame racing a leave) is shown once, the most recent wins.
func (r *room) everybody() []artifacts.PresenceMember {
	bySession := make(map[string]artifacts.PresenceMember, len(r.local)+len(r.remote))
	keep := func(m artifacts.PresenceMember) {
		if held, ok := bySession[m.Session]; !ok || m.At.After(held.At) {
			bySession[m.Session] = m
		}
	}
	for _, m := range r.local {
		keep(m.member)
	}
	for _, m := range r.remote {
		keep(m.member)
	}
	out := slices.Collect(maps.Values(bySession))
	slices.SortFunc(out, func(a, b artifacts.PresenceMember) int {
		// MOST RECENT FIRST, and the session breaks a tie so two nodes
		// present one room in one order.
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Session, b.Session))
	})
	return out
}

// presentation is what a room shows: its members, and for a large room the
// count of those it does not show.
type presentation struct {
	members []artifacts.PresenceMember
	counts  *artifacts.PresenceCounts
}

// present is what the room shows now, moving the hysteresis.
func (r *room) present() presentation {
	all := r.everybody()
	switch {
	case !r.truncated && len(all) > artifacts.PresenceRoomTop:
		r.truncated = true
	case r.truncated && len(all) < artifacts.PresenceRoomFull:
		r.truncated = false
	}
	if !r.truncated {
		return presentation{members: all}
	}
	counts := &artifacts.PresenceCounts{}
	shown := all
	if len(all) > artifacts.PresenceRoomTop {
		shown = all[:artifacts.PresenceRoomTop]
		for _, m := range all[artifacts.PresenceRoomTop:] {
			if m.Kind == iam.ActorAgent {
				counts.Agent++
			} else {
				counts.Human++
			}
		}
	}
	return presentation{members: shown, counts: counts}
}

// roster is a presentation as a joining socket is handed it.
func (r *room) roster(p presentation) artifacts.PresenceRoster {
	members := p.members
	if members == nil {
		members = []artifacts.PresenceMember{}
	}
	return artifacts.PresenceRoster{Artifact: r.id, Members: members, Counts: p.counts,
		Truncated: p.counts != nil}
}

// diff is what this node's sockets must be told to go from what they were
// shown to p, and whether there is anything to tell; p becomes what they were
// shown.
func (r *room) diff(p presentation) (Change, bool) {
	c := Change{Artifact: r.id, Upsert: []artifacts.PresenceMember{}, Leave: []string{},
		Counts: p.counts, Truncated: p.counts != nil}
	next := make(map[string]artifacts.PresenceMember, len(p.members))
	for _, m := range p.members {
		next[m.Session] = m
		if held, ok := r.presented[m.Session]; !ok || held != m {
			c.Upsert = append(c.Upsert, m)
		}
	}
	for _, s := range slices.Sorted(maps.Keys(r.presented)) {
		if _, ok := next[s]; !ok {
			c.Leave = append(c.Leave, s)
		}
	}
	countsMoved := (r.counts == nil) != (p.counts == nil) ||
		(r.counts != nil && *r.counts != *p.counts)
	r.presented = next
	r.counts = p.counts
	return c, len(c.Upsert)+len(c.Leave) > 0 || countsMoved
}

// stopTimer cancels a pending fan-out.
func (r *room) stopTimer() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}
