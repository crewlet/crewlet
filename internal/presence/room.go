package presence

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// room is one room's presence on this node: the members this node holds
// itself (its sockets and its agents), what its peers last told it, and what
// its sockets were last shown.
//
// Every method is called with the hub's lock held.
type room struct {
	id Room

	local  map[string]*localMember
	remote map[remoteKey]*remoteMember

	// departed is the sessions a peer said left, at the instant of the
	// frame that said so (its Until, which the peer stamps from its own
	// clock), so a frame sealed before the leave and arriving after it (the
	// join-time answer, which travels by another path than the peer's
	// broadcasts) cannot bring the member back.
	departed map[remoteKey]time.Time

	// presented is what this node's sockets were last shown, by session:
	// the fan-out is the difference between it and what the room presents
	// now. counts is the large-room count they were last shown, nil while
	// the room presents everybody.
	presented map[string]Member
	counts    *Counts

	// waiting is when each member whose change the sockets have not been
	// told yet was first held back ([room.diff]).
	waiting map[string]time.Time

	// truncated is the hysteresis: a room presents only its most recent
	// members once it passes [RoomTop], and everybody
	// again only once it falls below [RoomFull], so a room
	// hovering around the threshold does not flip on every join.
	truncated bool

	// outUpsert and outLeave are the local sessions changed or gone since
	// the last frame this node sent about the room.
	outUpsert map[string]struct{}
	outLeave  map[string]struct{}

	// timer and flushed are the sockets' fan-out: the one pending and when
	// the last ran. sendTimer and sent are the same for the frames this
	// node sends the fleet.
	timer     *time.Timer
	flushed   time.Time
	sendTimer *time.Timer
	sent      time.Time
}

// localMember is a member this node holds: a socket's, or an agent's tool
// session.
type localMember struct {
	member Member

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
	member Member
	heard  time.Time

	// told is the instant of the frame that last said it (the frame's
	// Until, from the peer's clock): what a frame arriving out of order is
	// compared with.
	told time.Time
}

func newRoom(id Room) *room {
	return &room{id: id, local: map[string]*localMember{}, remote: map[remoteKey]*remoteMember{},
		departed:  map[remoteKey]time.Time{},
		presented: map[string]Member{}, waiting: map[string]time.Time{},
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
func (r *room) localMembers() []Member {
	out := make([]Member, 0, len(r.local))
	for _, s := range slices.Sorted(maps.Keys(r.local)) {
		out = append(out, r.local[s].member)
	}
	return out
}

// hear lands a peer's frame, reporting whether the room changed. A full frame
// replaces everything the room held from that node, except what a newer frame
// already said.
//
// # A late frame fills in; it never undoes
//
// Frames from one node arrive in the order it sent them, with one exception:
// the answer to this node's join-time ask is sealed when the peer answers and
// travels to this node's reply mailbox, not the room's subject, so the peer's
// broadcasts can overtake it and say what changed since. Applied as a
// replacement, that answer deleted every member the peer gained in between,
// and the room showed them again only at the peer's next rebroadcast,
// [Rebroadcast] later: measured (internal/artifactspike,
// S6), a room of fifty joined at once across three nodes took fifteen seconds
// to show everybody everywhere, against the second the join objective allows.
// So every member carries the instant of the frame that last said it, and a
// frame changes or deletes a member only where it is newer than that, and
// never brings back a member a newer frame said left. A leave is always
// applied: a session id is never reused, so a leave is final whenever it
// arrives.
func (r *room) hear(f Frame, now time.Time) bool {
	moved := false
	if f.Full {
		named := make(map[string]bool, len(f.Upsert))
		for _, m := range f.Upsert {
			named[m.Session] = true
		}
		for key, held := range r.remote {
			if key.node == f.Node && !named[key.session] && !held.told.After(f.Until) {
				delete(r.remote, key)
				moved = true
			}
		}
	}
	for _, m := range f.Upsert {
		key := remoteKey{node: f.Node, session: m.Session}
		if at, gone := r.departed[key]; gone && !f.Until.After(at) {
			continue
		}
		held, ok := r.remote[key]
		if ok && held.told.After(f.Until) {
			continue
		}
		if !ok || held.member != m {
			moved = true
		}
		r.remote[key] = &remoteMember{member: m, heard: now, told: f.Until}
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
	return moved
}

// expire drops the peers' members not heard for [Expiry]
// (three missed rebroadcasts: their node went away without saying so) and this
// node's agents not touched for [agentLinger], reporting whether the room
// changed.
func (r *room) expire(now time.Time) bool {
	moved := false
	for key, m := range r.remote {
		if now.Sub(m.heard) >= Expiry {
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
func (r *room) everybody() []Member {
	bySession := make(map[string]Member, len(r.local)+len(r.remote))
	keep := func(m Member) {
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
	slices.SortFunc(out, func(a, b Member) int {
		// MOST RECENT FIRST, and the session breaks a tie so two nodes
		// present one room in one order.
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Session, b.Session))
	})
	return out
}

// presentation is what a room shows: its members, and for a large room the
// count of those it does not show.
type presentation struct {
	members []Member
	counts  *Counts
}

// present is what the room shows now, moving the hysteresis.
func (r *room) present() presentation {
	all := r.everybody()
	switch {
	case !r.truncated && len(all) > RoomTop:
		r.truncated = true
	case r.truncated && len(all) < RoomFull:
		r.truncated = false
	}
	if !r.truncated {
		return presentation{members: all}
	}
	counts := &Counts{}
	shown := all
	if len(all) > RoomTop {
		shown = all[:RoomTop]
		for _, m := range all[RoomTop:] {
			if m.Kind == iam.ActorAgent {
				counts.Agent++
			} else {
				counts.Human++
			}
		}
	}
	return presentation{members: shown, counts: counts}
}

// rosterFor is what a socket joining the room is handed: what this node's
// sockets were last shown, and the joiner's own member beside it.
//
// THE BASELINE EVERY FAN-OUT IS TAKEN FROM, never the room as it presents now:
// a fan-out is the difference from what the sockets were last shown
// ([room.diff]), so a socket handed anything else holds a screen the next
// fan-out does not apply to. Handed the room as it presented at the join, a
// member who arrived since the last fan-out and left before the next was on
// the joiner's screen and in no fan-out's leave, and stayed there for good.
// What moved since the last fan-out reaches the joiner in the next one, as it
// reaches every other socket here.
func (r *room) rosterFor(joiner Member) Roster {
	members := make([]Member, 0, len(r.presented)+1)
	for _, m := range r.presented {
		if m.Session != joiner.Session {
			members = append(members, m)
		}
	}
	members = append(members, joiner)
	slices.SortFunc(members, func(a, b Member) int {
		// MOST RECENT FIRST, as the room presents itself.
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Session, b.Session))
	})
	return Roster{Members: members, Counts: r.counts, Truncated: r.counts != nil}
}

// diff is what this node's sockets must be told to go from what they were
// shown to p, whether there is anything to tell, and whether members were
// held back for the next fan-out.
//
// AT MOST [FrameMembers] MEMBERS ARE RE-PRESENTED, the one
// whose change has waited longest first (then the most recently active), so
// every changed member is told within a few fan-outs however many others keep
// moving: ordered by recency alone, a member who moved once in a room where
// ten others move all the time would never be told. What the sockets were
// shown moves only by what was told, so a member held back is told in a
// fan-out that follows. Every member leaving the presentation is told at
// once: a leave is a session id, and a socket showing somebody gone is the one
// error presence must not make.
func (r *room) diff(p presentation, now time.Time) (Change, bool, bool) {
	c := Change{Room: r.id, Upsert: []Member{}, Leave: []string{},
		Counts: p.counts, Truncated: p.counts != nil}
	next := make(map[string]Member, len(p.members))
	var changed []Member
	for _, m := range p.members {
		if held, ok := r.presented[m.Session]; ok && held == m {
			next[m.Session] = m
			continue
		}
		changed = append(changed, m)
		if _, waiting := r.waiting[m.Session]; !waiting {
			r.waiting[m.Session] = now
		}
	}
	// p's order is most recent first, which a stable sort keeps among
	// changes that have waited equally long.
	slices.SortStableFunc(changed, func(a, b Member) int {
		return r.waiting[a.Session].Compare(r.waiting[b.Session])
	})
	for i, m := range changed {
		if i < FrameMembers {
			c.Upsert = append(c.Upsert, m)
			next[m.Session] = m
			delete(r.waiting, m.Session)
			continue
		}
		if held, ok := r.presented[m.Session]; ok {
			next[m.Session] = held
		}
	}
	for _, s := range slices.Sorted(maps.Keys(r.presented)) {
		if _, ok := next[s]; !ok {
			c.Leave = append(c.Leave, s)
		}
	}
	// A WAIT ENDS WITH ITS MEMBER'S CHANGE: told above, or gone from the
	// presentation, or moved back to what the sockets were shown.
	still := make(map[string]bool, len(changed))
	for _, m := range changed[min(len(changed), FrameMembers):] {
		still[m.Session] = true
	}
	for s := range r.waiting {
		if !still[s] {
			delete(r.waiting, s)
		}
	}
	countsMoved := (r.counts == nil) != (p.counts == nil) ||
		(r.counts != nil && *r.counts != *p.counts)
	r.presented = next
	r.counts = p.counts
	return c, len(c.Upsert)+len(c.Leave) > 0 || countsMoved,
		len(changed) > FrameMembers
}

// stopTimer cancels a pending fan-out and a pending frame to the fleet.
func (r *room) stopTimer() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.sendTimer != nil {
		r.sendTimer.Stop()
		r.sendTimer = nil
	}
}
