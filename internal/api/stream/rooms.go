package stream

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ARTIFACT ROOMS: a socket in an artifact's room receives what moves on that
// artifact (`artifact_changed`), what its datastore commits (`artifact_data`)
// and who else is there (`artifact_presence`), and tells the room what it is
// doing (`presence`).
//
// # A join is decided like reading the artifact, and stays decided
//
// `artifact_join` is decided by `get_artifact` ([Rooms.Access]), three-valued
// like a watch: allowed puts the socket in the room at the role the decision
// found, refused is a `not_found` error frame (a caller with no relation is
// told the artifact does not exist, in the words an id nothing holds is
// answered with) — or `artifact_trashed` for a caller the artifact is
// disclosed to while it is in the trash, whose content no room carries until
// its owner restores it — and undecidable an `unavailable` one with its retry
// hint.
// None closes the socket. The join is DECIDED AGAIN on every decision of the
// socket's credential and on every movement that can narrow who may read the
// artifact (a share or an unshare, the trash, a transfer, a purge): a refusal
// withdraws the room with an error frame naming the artifact, and a role that
// moved is the role later pushes are shaped by. From the moment such a movement
// lands until its decision does, the room's datastore pushes are withheld from
// its members, so no document reaches a reader whose share was just withdrawn;
// a member kept is told to re-read what it missed.
//
// # An undecidable answer holds the room, and is asked again
//
// A decision this node could not reach (the estate router, an identity or
// chart copy that cannot vouch, a record it could not read) FAILS CLOSED, as
// the join itself does on the same answer: the membership stays in the room,
// so it keeps hearing what moves and who is present, but its datastore pushes
// are withheld until a decision lands. The room is decided again after a
// backoff ([roomRedecideFloor] doubling to [roomRedecideCeiling], or the
// refusal's own wait when it names a longer one) for as long as the answer
// stays undecidable, and the decision that finally lands resumes the
// membership at the role it found or withdraws it. Keeping the role the member
// held before would push documents, judged on a share that may just have been
// withdrawn, to a reader nobody vouched for.
//
// # Every data node pushes from the appliers it hears
//
// Every node serving the API is a data node and applies both artifact logs, so
// each pushes to its own sockets from its own appliers after each commit. A
// node whose copy is OUT OF SERVICE hears a sound peer's appliers instead,
// relayed (the engine's live relay, one source at a time for every domain),
// while the reads themselves are answered by a sound data node through the
// estate router; and every switch of source reaches the rooms as the domains'
// own reset, which re-reads and decides again every room on the node.
//
// # A datastore push is shaped per reader
//
// A document in an `own_rows`, `editors` or `private` collection reaches only
// the members its rule admits, by the role each member's join decided and the
// record each member is ([artifactdata.Readable], the rule every read path
// asks). One payload is encoded per set of documents a reader may see, so a
// room of readers who all see everything costs one encode.

// Rooms is what an artifact's room on this node is decided by and fed from.
// The zero value serves no room: a join is answered `unknown_query`, the code
// for a surface this process does not have.
type Rooms struct {
	// Access decides a join, and every decision of it again.
	Access ArtifactAccess

	// Stores says where an artifact's datastore stands on this node, for a
	// join's `through` and `resync`.
	Stores DataStores

	// Presence is this node's presence hub.
	Presence Presence
}

// serves reports whether every half is wired: a room that could be joined and
// never told who is present, or the reverse, is a room that lies.
func (r Rooms) serves() bool {
	return r.Access != nil && r.Stores != nil && r.Presence != nil
}

// ArtifactAccess decides who may be in an artifact's room.
type ArtifactAccess interface {
	// Standing is p's role on artifact id, decided as reading it is
	// (`get_artifact`): the role, EMPTY for a caller the artifact is
	// answered as absent to (no relation to it, purged, never created),
	// [artifactsdomain.ErrTrashed] for one it is disclosed to while it is in
	// the trash, and any other error for a node that could not decide.
	Standing(ctx context.Context, p iam.Principal, id artifacts.ID) (artifacts.Role, error)
}

// DataStores is where an artifact's datastore stands on this node.
type DataStores interface {
	// Through is the store's version (the `since` a client's next catch-up
	// names) and the floor below which a catch-up is no longer answered
	// incrementally (a delete older than it has been forgotten).
	Through(ctx context.Context, id artifacts.ID) (through, floor int64, err error)
}

// Presence is this node's presence hub as the socket uses it.
type Presence interface {
	Join(room presence.Room, who presence.Local) (string, presence.Roster, error)
	Update(room presence.Room, session string, activity presence.Activity,
		focus presence.Focus) error
	Leave(room presence.Room, session string)
	OnChange(fn func(presence.Change)) (stop func())
}

// Metrics is the recorder the socket's pushes report through.
type Metrics interface {
	Observe(name string, d time.Duration, attrs metrics.Attrs)
}

// What an artifact room's frames are called in their `what`.
const (
	artifactJoinWhat = "artifact_join"
)

// ArtifactJoined is a join's answer: the session the socket is known by in the
// room, its role, the room as it presents now, and where the datastore stands
// (`through`), with `resync` set when the socket's `since` cannot be caught up
// incrementally and the collections must be read whole.
type ArtifactJoined struct {
	Artifact artifacts.ID    `json:"artifact"`
	Session  string          `json:"session"`
	Role     artifacts.Role  `json:"role"`
	Roster   presence.Roster `json:"roster"`
	Through  int64           `json:"through"`
	Resync   bool            `json:"resync"`
}

// ArtifactPresence is an `artifact_presence` push: one artifact room's
// fan-out ([presence.Change]), the room named by its artifact as every other
// artifact frame names it.
type ArtifactPresence struct {
	Artifact  artifacts.ID      `json:"artifact"`
	Upsert    []presence.Member `json:"upsert"`
	Leave     []string          `json:"leave"`
	Counts    *presence.Counts  `json:"counts,omitempty"`
	Truncated bool              `json:"truncated"`
}

// ArtifactChange is an `artifact_changed` push: which artifact, what moved,
// the head version after it, and the position of the record that moved it.
type ArtifactChange struct {
	Artifact artifacts.ID             `json:"artifact"`
	What     artifactsdomain.MoveKind `json:"what"`
	Head     int                      `json:"head,omitempty"`
	Position statelog.Position        `json:"position"`
}

// ArtifactData is an `artifact_data` push as one reader receives it.
type ArtifactData struct {
	Artifact artifacts.ID `json:"artifact"`

	// Deltas are the changed documents this reader may see, inline, in the
	// order the batch changed them; a later delta for one document
	// supersedes an earlier one.
	Deltas []artifactdata.DocDelta `json:"deltas,omitempty"`

	// Keys are those documents' keys alone, sent in place of Deltas when
	// they would not fit [artifacts.PushFrameMaxBytes]: Reread is then set,
	// and the reader asks for them.
	Keys   []DocKey `json:"keys,omitempty"`
	Reread bool     `json:"reread,omitempty"`

	// Schema says the store's collections changed: read the schema again.
	Schema bool `json:"schema,omitempty"`

	// Resync says what this node pushed may have gaps (a record it could
	// not read, a copy that left or returned to service, a push withheld
	// while the reader's access was being decided again): read whole.
	Resync bool `json:"resync,omitempty"`

	// Gone says the datastore was purged with its artifact.
	Gone bool `json:"gone,omitempty"`

	// Through is the store's version after the batch, the `since` the
	// reader's next catch-up names; zero on a hint that carries none.
	Through int64 `json:"through,omitempty"`
}

// DocKey is a changed document named and not carried.
type DocKey struct {
	Collection string `json:"collection"`
	Doc        string `json:"doc"`
	Version    int64  `json:"version"`
	Deleted    bool   `json:"deleted,omitempty"`
}

// dataPush is one store's batch, shaped per reader when it is fanned out.
type dataPush struct {
	artifact artifacts.ID
	deltas   []artifactdata.DocDelta
	schema   bool
	through  int64
}

// deltaOverhead is what one delta weighs on the wire beyond its body: its
// keys, version, rule, creator, writer and instant. A measured upper bound,
// so the inline-or-keys choice is made from the bodies alone without encoding
// the frame once per reader to find out.
const deltaOverhead = 512

// ForReader is the batch as r may see it: the documents r's role and record
// admit, inline while they fit and as keys past it. The variant names which
// documents those were, so readers who see the same ones share one encode. A
// reader who may see none of it, and no schema change, is sent nothing: an
// empty push would say only that somebody wrote somewhere they cannot read.
func (p dataPush) ForReader(r Reader) (string, any, bool) {
	reading := artifactdata.Reading{Record: r.Audience.Record(), Role: r.Role}
	seen := make([]byte, (len(p.deltas)+7)/8)
	var shown []artifactdata.DocDelta
	weight := int64(0)
	for i, d := range p.deltas {
		if !artifactdata.Readable(d.Read, reading, d.CreatedBy) {
			continue
		}
		seen[i/8] |= 1 << (i % 8)
		shown = append(shown, d)
		weight += int64(len(d.Body)) + deltaOverhead
	}
	if len(shown) == 0 && !p.schema {
		return "", nil, false
	}
	out := ArtifactData{Artifact: p.artifact, Schema: p.schema, Through: p.through}
	variant := "docs=" + hex.EncodeToString(seen)
	if weight <= artifacts.PushFrameMaxBytes {
		out.Deltas = shown
	} else {
		out.Reread = true
		for _, d := range shown {
			out.Keys = append(out.Keys, DocKey{Collection: d.Collection, Doc: d.Doc,
				Version: d.Version, Deleted: d.Deleted})
		}
		variant += ";keys"
	}
	return variant, out, true
}

// How soon a room whose decision could not be reached is asked again. The
// floor is one estate round trip with room to spare, so a transient router or
// read error clears within a blink of the reader's eye; each further miss
// doubles it, up to a ceiling that bounds what a node whose identity or chart
// copy stays behind spends asking (one decision per member per room) while a
// held reader waits at most that long once the node recovers. A refusal that
// names its own longer wait is asked again no sooner than that wait.
const (
	roomRedecideFloor   = 250 * time.Millisecond
	roomRedecideCeiling = 30 * time.Second
)

// roomRedecideDelay is how long after its attempts-th undecidable answer a
// room is decided again, given the error that answer carried.
func roomRedecideDelay(attempts int, err error) time.Duration {
	delay := roomRedecideCeiling
	if shift := attempts - 1; shift >= 0 && shift < 8 {
		delay = min(roomRedecideFloor<<shift, roomRedecideCeiling)
	}
	if hint := statelog.RetryAfter(err, 0); hint > delay {
		delay = min(hint, roomRedecideCeiling)
	}
	return delay
}

// roomWork is the rooms whose memberships wait to be decided again: now
// (pending), or once a backoff after an undecidable answer runs out (due, the
// backoff's timer), with the number of undecidable answers in a row each room
// has had (misses), which is what its next backoff is sized by.
//
// A BACKOFF IS A TIMER, NEVER A DUE INSTANT. An instant would be read off a
// clock and compared with another reading of it, and the service's clock
// ([Options.Now]) is the one every decision and stamp here is taken at, which
// a test pins: a backoff measured on it never runs out. A timer measures the
// delay itself and reads no clock at all.
type roomWork struct {
	mu      sync.Mutex
	pending map[artifacts.ID]bool
	due     map[artifacts.ID]*time.Timer
	misses  map[artifacts.ID]int
	wake    chan struct{}

	stop context.CancelFunc
	done chan struct{}
}

// startRooms starts the decider that takes a narrowing's rooms off the
// applier's goroutine, and hears the presence hub's fan-outs. Its lifetime is
// the service's, ended by [Service.Stop].
func (s *Service) startRooms() {
	ctx, cancel := context.WithCancel(context.Background())
	s.work = &roomWork{pending: map[artifacts.ID]bool{}, due: map[artifacts.ID]*time.Timer{},
		misses: map[artifacts.ID]int{}, wake: make(chan struct{}, 1),
		stop: cancel, done: make(chan struct{})}
	s.stopPresence = s.rooms.Presence.OnChange(s.PresenceChanged)
	go s.decideRooms(ctx)
}

// stopRooms ends the decider and stops hearing the hub.
func (s *Service) stopRooms() {
	if s.work == nil {
		return
	}
	s.stopPresence()
	s.work.stop()
	<-s.work.done
}

// joinArtifact handles one `artifact_join` frame.
func (s *Service) joinArtifact(ctx context.Context, client *Client, req request) {
	refuse := func(code httpjson.Code) Envelope {
		return Envelope{Kind: KindError, ID: req.ID, What: artifactJoinWhat, Error: code,
			Artifact: req.Artifact}
	}
	if !s.rooms.serves() {
		client.Reply(refuse(CodeUnknownQuery))
		return
	}
	id, err := artifacts.ParseID(strings.TrimSpace(req.Artifact))
	if err != nil {
		env := refuse(CodeBadParams)
		env.Detail = httpjson.Sentence("artifact is not an artifact id; join with the id " +
			"the artifact's own address carries")
		client.Reply(env)
		return
	}
	if !client.Posture().ServesQueries() {
		env := refuse(CodeUnavailable)
		env.unavailable(Unavailable{RetryAfter: httpjson.RetrySeconds(s.interval)})
		client.Reply(env)
		return
	}
	if _, in := s.hub.Session(client, id); !in && len(s.hub.Artifacts(client)) >= MaxRoomsPerSocket {
		env := refuse(CodeBadParams)
		env.Detail = httpjson.Sentence(fmt.Sprintf("a socket is in at most %d artifact "+
			"rooms at once; leave one before joining another", MaxRoomsPerSocket))
		client.Reply(env)
		return
	}
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		// A JOIN ASKED BY NOBODY IS A FAULT, as a question asked by nobody
		// is: every socket authenticates at its handshake and closes when
		// its credential ends, so a join reaching here unresolved means this
		// node lost who the socket is — which no refusal a client acts on
		// describes, and an `unauthorized` naming no decision least of all.
		log.ErrorContext(ctx, "stream_room_join_unresolved",
			"artifact", id.String(), "resolution", string(how))
		client.Reply(refuse(CodeQueryFailed))
		return
	}
	role, err := s.rooms.Access.Standing(ctx, principal, id)
	switch {
	case errors.Is(err, artifactsdomain.ErrTrashed):
		client.Reply(refuse(CodeArtifactTrashed))
		return
	case err != nil:
		log.InfoContext(ctx, "stream_room_undecidable", "login", principal.Login,
			"artifact", id.String(), "error", err.Error())
		env := refuse(CodeUnavailable)
		env.unavailable(Unavailable{RetryAfter: UnavailableOf(err).RetryAfter})
		client.Reply(env)
		return
	case role == "":
		client.Reply(refuse(CodeNotFound))
		return
	}

	through, floor, storeErr := s.rooms.Stores.Through(ctx, id)
	if storeErr != nil {
		// THE ROOM IS STILL JOINED: who is present and what moves on the
		// artifact do not wait on the datastore. The answer says it
		// cannot place the reader in the store, which is a whole read.
		log.InfoContext(ctx, "stream_room_store_unread", "artifact", id.String(),
			"error", storeErr.Error())
		through = 0
	}
	resync := storeErr != nil || req.Since <= 0 || req.Since < floor

	session, roster, err := s.rooms.Presence.Join(presence.ArtifactRoom(id), presence.Local{
		Seat: presenceSeat(principal), Kind: presenceKind(principal)})
	if err != nil {
		log.WarnContext(ctx, "stream_room_presence_refused", "artifact", id.String(),
			"error", err.Error())
		env := refuse(CodeUnavailable)
		env.unavailable(Unavailable{})
		client.Reply(env)
		return
	}
	_, replaced, ok := s.hub.JoinArtifact(client, id, role, session)
	if !ok {
		// THE SOCKET LEFT while the join was being decided.
		s.rooms.Presence.Leave(presence.ArtifactRoom(id), session)
		return
	}
	if replaced != "" {
		s.rooms.Presence.Leave(presence.ArtifactRoom(id), replaced)
	}
	client.Reply(Envelope{Kind: KindResult, ID: req.ID, What: artifactJoinWhat,
		Data: ArtifactJoined{Artifact: id, Session: session, Role: role, Roster: roster,
			Through: through, Resync: resync}})
}

// presenceSeat is the name a member is shown under: the one the principal's
// own record and every write it makes are kept under ([iam.RecordOwner]), so
// the room names a reader as the artifact's history does. That is the seat a
// person or an agent acts as, and the login of one acting under none (an
// unbound person, an operator's token). It used to be the seat alone, which
// is nothing for those, and every such reader in a room was drawn as an
// unnamed "somebody" while the history beside it named them.
func presenceSeat(p iam.Principal) string { return iam.RecordOwner(p) }

// presenceKind is the kind a member is shown as: an agent for a seat's own
// credential, a person for a person, and an operator for a credential acting
// under its own name.
func presenceKind(p iam.Principal) iam.ActorKind { return iam.ActorFor(p).Kind }

// leaveArtifact handles one `artifact_leave` frame. Leaving a room the socket
// is not in is not an error.
func (s *Service) leaveArtifact(client *Client, req request) {
	if !s.rooms.serves() {
		return
	}
	id, err := artifacts.ParseID(strings.TrimSpace(req.Artifact))
	if err != nil {
		return
	}
	if session, ok := s.hub.LeaveArtifact(client, id); ok {
		s.rooms.Presence.Leave(presence.ArtifactRoom(id), session)
	}
}

// presenceFrame handles one `presence` frame: what the socket is doing in a
// room it is in, and where it is looking. A frame for a room the socket is not
// in, or one outside the vocabulary, is dropped: presence is a display, and
// nothing waits on an answer.
func (s *Service) presenceFrame(ctx context.Context, client *Client, req request) {
	if !s.rooms.serves() || !client.Posture().ServesQueries() {
		return
	}
	id, err := artifacts.ParseID(strings.TrimSpace(req.Artifact))
	if err != nil {
		return
	}
	session, ok := s.hub.Session(client, id)
	if !ok {
		return
	}
	if err := s.rooms.Presence.Update(presence.ArtifactRoom(id), session, req.Activity,
		req.Focus); err != nil {
		log.DebugContext(ctx, "stream_presence_refused", "artifact", id.String(),
			"error", err.Error())
	}
}

// leave takes a closing socket out of every room it was in, its presence
// included, and off the hub. Idempotent.
func (s *Service) leave(client *Client) {
	if s.rooms.serves() {
		for id, m := range s.hub.Artifacts(client) {
			if session, ok := s.hub.LeaveArtifactIf(client, id, m.Join); ok {
				s.rooms.Presence.Leave(presence.ArtifactRoom(id), session)
			}
		}
	}
	s.hub.Unregister(client)
}

// PresenceChanged pushes one artifact room's presence fan-out to the sockets in
// it. A room of any other kind is not this socket surface's: the hub's every
// sink hears every room and keeps the kind it serves. Called on the presence
// hub's goroutine; never blocks.
func (s *Service) PresenceChanged(c presence.Change) {
	if c.Room.Kind != presence.RoomArtifact {
		return
	}
	id := artifacts.ID(c.Room.ID)
	s.hub.Broadcast(PushArtifact(KindArtifactPresence, id, ArtifactPresence{
		Artifact: id, Upsert: c.Upsert, Leave: c.Leave, Counts: c.Counts,
		Truncated: c.Truncated}, s.now()))
}

// ArtifactsMoved pushes what one committed artifacts batch moved to each
// artifact's room, and suspends and queues for a new decision every room whose
// access the batch may have narrowed.
//
// CALLED ON THE APPLY LOOP'S GOROUTINE, with the next batch waiting behind it:
// the pushes and the suspension are in memory, and every decision runs on the
// service's own goroutine.
func (s *Service) ArtifactsMoved(moved []artifactsdomain.Moved) {
	if !s.rooms.serves() {
		return
	}
	now := s.now()
	for _, m := range moved {
		if m.What == artifactsdomain.MoveReset {
			// A MOVEMENT NAMING NO ARTIFACT: every room on this node may
			// have moved, so every one is decided again and told so.
			for _, id := range s.hub.RoomsHeld() {
				s.hub.Suspend(id)
				s.hub.Broadcast(PushArtifact(KindArtifactChanged, id, ArtifactChange{
					Artifact: id, What: artifactsdomain.MoveReset, Position: m.Position}, now))
				s.queueDecision(id)
			}
			continue
		}
		visible(s.judge, s.hub.Broadcast(PushArtifact(KindArtifactChanged, m.Artifact,
			ArtifactChange{Artifact: m.Artifact, What: m.What, Head: m.Head, Position: m.Position},
			now)), m.At)
		s.observeLag("changed", m.At, now)
		if narrows(m.What) {
			s.hub.Suspend(m.Artifact)
			s.queueDecision(m.Artifact)
		}
	}
}

// narrows reports whether a movement can change who may read the artifact:
// a share granted, changed or withdrawn, the trash, a transfer, a purge.
func narrows(what artifactsdomain.MoveKind) bool {
	switch what {
	case artifactsdomain.MoveShare, artifactsdomain.MoveTrash, artifactsdomain.MoveTransfer,
		artifactsdomain.MovePurge:
		return true
	}
	return false
}

// ArtifactDataMoved pushes what one committed datastore batch changed to each
// artifact's room, shaped per reader. Called on the apply loop's goroutine;
// never blocks.
func (s *Service) ArtifactDataMoved(moved []artifactdata.Moved) {
	if !s.rooms.serves() {
		return
	}
	now := s.now()
	for _, m := range moved {
		switch {
		case m.Reset:
			s.rereadData(now)
			continue
		case m.Purged:
			visible(s.judge, s.hub.Broadcast(PushArtifact(KindArtifactData, m.Artifact,
				ArtifactData{Artifact: m.Artifact, Gone: true}, now)), m.At)
			continue
		case len(m.Docs) == 0 && !m.Schema:
			continue
		}
		visible(s.judge, s.hub.Broadcast(PushArtifact(KindArtifactData, m.Artifact, dataPush{
			artifact: m.Artifact, deltas: m.Docs, schema: m.Schema,
			through: m.Through.Packed()}, now)), m.At)
		s.observeLag("data", m.At, now)
	}
}

// observeLag records how long after the broker stored a record its push of
// kind (`changed`, an artifact's movement; `data`, a datastore's documents)
// was fanned out: the commit-to-socket lag a room's live view is held to.
func (s *Service) observeLag(kind string, stored, now time.Time) {
	if s.metrics == nil || stored.IsZero() {
		return
	}
	s.metrics.Observe(metrics.ArtifactsPushLag, max(0, now.Sub(stored)),
		metrics.Attrs{"kind": kind})
}

// rereadData tells every room on this node its datastore pushes may have gaps.
func (s *Service) rereadData(now time.Time) {
	for _, id := range s.hub.RoomsHeld() {
		s.hub.Broadcast(PushArtifact(KindArtifactData, id,
			ArtifactData{Artifact: id, Resync: true}, now))
	}
}

// queueDecision asks the decider to decide id's room again.
func (s *Service) queueDecision(id artifacts.ID) {
	s.work.mu.Lock()
	s.work.pending[id] = true
	s.work.mu.Unlock()
	s.wakeDecider()
}

// retryDecision asks the decider to decide id's room again after a backoff,
// because a decision about one of its members could not be reached (err). A
// retry already scheduled keeps its timer, so several members missing in one
// pass, or a credential recheck missing beside it, cost one backoff step and
// not one each.
func (s *Service) retryDecision(id artifacts.ID, err error) {
	s.work.mu.Lock()
	defer s.work.mu.Unlock()
	if _, scheduled := s.work.due[id]; scheduled {
		return
	}
	s.work.misses[id]++
	s.work.due[id] = time.AfterFunc(roomRedecideDelay(s.work.misses[id], err), func() {
		s.work.mu.Lock()
		delete(s.work.due, id)
		s.work.pending[id] = true
		s.work.mu.Unlock()
		s.wakeDecider()
	})
}

// settleDecision records that every member of id's room was decided, so its
// next undecidable answer starts the backoff from the floor again. A retry
// scheduled meanwhile is kept: it was scheduled for a member decided after
// this pass.
func (s *Service) settleDecision(id artifacts.ID) {
	s.work.mu.Lock()
	delete(s.work.misses, id)
	s.work.mu.Unlock()
}

// wakeDecider tells the decider its work changed.
func (s *Service) wakeDecider() {
	select {
	case s.work.wake <- struct{}{}:
	default:
	}
}

// takeDecisions is every room due to be decided now, sorted and taken off the
// queue: each queued room, and each whose backoff has run out.
func (s *Service) takeDecisions() []artifacts.ID {
	s.work.mu.Lock()
	defer s.work.mu.Unlock()
	ids := make([]artifacts.ID, 0, len(s.work.pending))
	for id := range s.work.pending {
		ids = append(ids, id)
	}
	clear(s.work.pending)
	slices.SortFunc(ids, func(a, b artifacts.ID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}

// stopBackoffs stops every backoff still waiting to run out, once the decider
// that would take its room has ended.
func (s *Service) stopBackoffs() {
	s.work.mu.Lock()
	defer s.work.mu.Unlock()
	for id, timer := range s.work.due {
		timer.Stop()
		delete(s.work.due, id)
	}
}

// decideRooms decides queued rooms again until ctx ends: each as soon as it is
// queued, and each whose decision could not be reached again when its backoff
// is due.
func (s *Service) decideRooms(ctx context.Context) {
	defer close(s.work.done)
	defer s.stopBackoffs()
	for {
		for _, id := range s.takeDecisions() {
			var undecided error
			for _, m := range s.hub.Members(id) {
				if err := s.decideMember(ctx, m.Client.Audience().Principal(), id, m); err != nil {
					undecided = err
				}
			}
			if ctx.Err() != nil {
				return
			}
			if undecided != nil {
				s.retryDecision(id, undecided)
			} else {
				s.settleDecision(id)
			}
		}
		// A ROOM QUEUED DURING THE PASS, or a backoff run out, has already
		// signalled wake, so the next take collects it at once.
		select {
		case <-ctx.Done():
			return
		case <-s.work.wake:
		}
	}
}

// recheckRooms decides every room the socket is in again, as whoever the
// latest decision of its credential resolved: the rooms' half of the watch's
// own recheck. A membership it could not decide is held and its room asked
// again by the decider after a backoff.
func (s *Service) recheckRooms(ctx context.Context, client *Client) {
	if !s.rooms.serves() {
		return
	}
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return
	}
	for id, m := range s.hub.Artifacts(client) {
		if err := s.decideMember(ctx, principal, id, m); err != nil {
			s.retryDecision(id, err)
		}
	}
}

// decideMember decides one membership of id's room again: a refusal withdraws
// it with an error frame naming the artifact, an admission resumes it at the
// role found now (telling the member to re-read anything withheld meanwhile),
// and an undecidable answer HOLDS it, withholding its datastore pushes, and is
// returned so the caller asks again. A membership already gone, or a decision
// ended by ctx, returns nil: there is nothing left to ask about.
func (s *Service) decideMember(ctx context.Context, p iam.Principal, id artifacts.ID, m RoomMember) error {
	role, err := s.rooms.Access.Standing(ctx, p, id)
	var refused httpjson.Code = CodeNotFound
	if errors.Is(err, artifactsdomain.ErrTrashed) {
		// THE TRASH WITHDRAWS THE ROOM as a refusal does, under its own
		// code: the member may read the artifact again once it is
		// restored, which not_found would tell it never to expect.
		role, err, refused = "", nil, CodeArtifactTrashed
	}
	switch {
	case err != nil:
		if ctx.Err() != nil {
			//nolint:nilerr // A decision ended by ctx leaves nothing to ask; see the doc above.
			return nil
		}
		log.InfoContext(ctx, "stream_room_redecision_undecidable", "login", p.Login,
			"artifact", id.String(), "error", err.Error())
		if !s.hub.Hold(m.Client, id, m.Join) {
			return nil
		}
		return err
	case role == "":
		session, ok := s.hub.LeaveArtifactIf(m.Client, id, m.Join)
		if !ok {
			return nil
		}
		s.rooms.Presence.Leave(presence.ArtifactRoom(id), session)
		log.InfoContext(ctx, "stream_room_withdrawn", "login", p.Login, "artifact", id.String())
		m.Client.Reply(Envelope{Kind: KindError, What: artifactJoinWhat, Error: refused,
			Artifact: id.String()})
		return nil
	}
	missed, ok := s.hub.Resume(m.Client, id, m.Join, role)
	if ok && missed {
		m.Client.send(s.encodeFor(m.Client, PushArtifact(KindArtifactData, id,
			ArtifactData{Artifact: id, Resync: true}, s.now())))
	}
	return nil
}

// encodeFor is one room push encoded for one client, nil when it may not have
// it: [Hub.Broadcast]'s rule, for the one frame a decision owes one member.
func (s *Service) encodeFor(c *Client, env Envelope) *Frame {
	if !c.Receives(env.Kind) || !c.Posture().Delivers(env.Kind) {
		return nil
	}
	frame, err := EncodeFrame(env)
	if err != nil {
		log.Error("stream_encode_failed", "kind", env.Kind, "error", err)
		return nil
	}
	return frame
}
