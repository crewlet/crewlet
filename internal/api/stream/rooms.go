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
// answered with), and undecidable an `unavailable` one with its retry hint.
// None closes the socket. The join is DECIDED AGAIN on every decision of the
// socket's credential and on every movement that can narrow who may read the
// artifact (a share or an unshare, the trash, a transfer, a purge): a refusal
// withdraws the room with an error frame naming the artifact, an undecidable
// answer keeps it, and a role that moved is the role later pushes are shaped
// by. From the moment such a movement lands until its decision does, the room's
// datastore pushes are withheld from its members, so no document reaches a
// reader whose share was just withdrawn; a member kept is told to re-read what
// it missed.
//
// # Every node pushes from its own appliers
//
// Every node serving the API is a data node and applies both artifact logs, so
// each pushes to its own sockets from its own appliers after each commit, and
// nothing is forwarded between nodes. A node whose copy is OUT OF SERVICE
// withholds its appliers' pushes (they describe a copy nobody vouches for) and
// tells every room to re-read when it leaves service and again when it
// returns, while the reads themselves are answered by a sound data node through
// the estate router.
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
	// answered as absent to (no relation to it, in the trash, purged, never
	// created), and an error for a node that could not decide.
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
	Join(id artifacts.ID, who presence.Local) (string, artifacts.PresenceRoster, error)
	Update(id artifacts.ID, session string, activity artifacts.PresenceActivity,
		focus artifacts.Focus) error
	Leave(id artifacts.ID, session string)
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
	Artifact artifacts.ID             `json:"artifact"`
	Session  string                   `json:"session"`
	Role     artifacts.Role           `json:"role"`
	Roster   artifacts.PresenceRoster `json:"roster"`
	Through  int64                    `json:"through"`
	Resync   bool                     `json:"resync"`
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

// roomWork is the rooms whose memberships wait to be decided again.
type roomWork struct {
	mu      sync.Mutex
	pending map[artifacts.ID]bool
	wake    chan struct{}

	stop context.CancelFunc
	done chan struct{}
}

// startRooms starts the decider that takes a narrowing's rooms off the
// applier's goroutine, and hears the presence hub's fan-outs. Its lifetime is
// the service's, ended by [Service.Stop].
func (s *Service) startRooms() {
	ctx, cancel := context.WithCancel(context.Background())
	s.work = &roomWork{pending: map[artifacts.ID]bool{}, wake: make(chan struct{}, 1),
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
		client.Reply(refuse(CodeUnauthorized))
		return
	}
	role, err := s.rooms.Access.Standing(ctx, principal, id)
	switch {
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

	session, roster, err := s.rooms.Presence.Join(id, presence.Local{
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
		s.rooms.Presence.Leave(id, session)
		return
	}
	if replaced != "" {
		s.rooms.Presence.Leave(id, replaced)
	}
	client.Reply(Envelope{Kind: KindResult, ID: req.ID, What: artifactJoinWhat,
		Data: ArtifactJoined{Artifact: id, Session: session, Role: role, Roster: roster,
			Through: through, Resync: resync}})
}

// presenceSeat is the seat handle a member is shown under: the seat a person
// or an agent acts as, and nothing for a credential that holds none (an
// operator's token), whose login is not a seat and is not shown.
func presenceSeat(p iam.Principal) string { return p.Seat }

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
		s.rooms.Presence.Leave(id, session)
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
	if err := s.rooms.Presence.Update(id, session, req.Activity, req.Focus); err != nil {
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
				s.rooms.Presence.Leave(id, session)
			}
		}
	}
	s.hub.Unregister(client)
}

// PresenceChanged pushes one room's presence fan-out to the sockets in it.
// Called on the presence hub's goroutine; never blocks.
func (s *Service) PresenceChanged(c presence.Change) {
	s.hub.Broadcast(PushArtifact(KindArtifactPresence, c.Artifact, c, s.now()))
}

// ArtifactsMoved pushes what one committed artifacts batch moved to each
// artifact's room, and suspends and queues for a new decision every room whose
// access the batch may have narrowed.
//
// CALLED ON THE APPLY LOOP'S GOROUTINE, with the next batch waiting behind it:
// the pushes and the suspension are in memory, and every decision runs on the
// service's own goroutine.
func (s *Service) ArtifactsMoved(moved []artifactsdomain.Moved) {
	if !s.rooms.serves() || s.withholding.Load() {
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
		s.hub.Broadcast(PushArtifact(KindArtifactChanged, m.Artifact, ArtifactChange{
			Artifact: m.Artifact, What: m.What, Head: m.Head, Position: m.Position}, now))
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
	if !s.rooms.serves() || s.withholding.Load() {
		return
	}
	now := s.now()
	for _, m := range moved {
		switch {
		case m.Reset:
			s.rereadData(now)
			continue
		case m.Purged:
			s.hub.Broadcast(PushArtifact(KindArtifactData, m.Artifact,
				ArtifactData{Artifact: m.Artifact, Gone: true}, now))
			continue
		case len(m.Docs) == 0 && !m.Schema:
			continue
		}
		s.hub.Broadcast(PushArtifact(KindArtifactData, m.Artifact, dataPush{
			artifact: m.Artifact, deltas: m.Docs, schema: m.Schema,
			through: m.Through.Packed()}, now))
		s.observeLag(m, now)
	}
}

// observeLag records how long after the broker stored the batch's newest record
// its push was fanned out: the commit-to-socket lag the datastore's live view
// is held to.
func (s *Service) observeLag(m artifactdata.Moved, now time.Time) {
	if s.metrics == nil {
		return
	}
	var newest time.Time
	for _, w := range m.Writes {
		if w.At.After(newest) {
			newest = w.At
		}
	}
	if newest.IsZero() {
		return
	}
	s.metrics.Observe(metrics.ArtifactsPushLag, max(0, now.Sub(newest)),
		metrics.Attrs{"kind": "data"})
}

// rereadData tells every room on this node its datastore pushes may have gaps.
func (s *Service) rereadData(now time.Time) {
	for _, id := range s.hub.RoomsHeld() {
		s.hub.Broadcast(PushArtifact(KindArtifactData, id,
			ArtifactData{Artifact: id, Resync: true}, now))
	}
}

// ArtifactsCopy is this node's artifact copies going out of service (false) or
// back into it (true). While out, its appliers' pushes are withheld; at each
// switch every room is told to re-read, because what moved across it is not
// replayed and the reads are answered by a sound data node meanwhile.
func (s *Service) ArtifactsCopy(serving bool) {
	if !s.rooms.serves() {
		return
	}
	if s.withholding.Swap(!serving) == !serving {
		return
	}
	now := s.now()
	for _, id := range s.hub.RoomsHeld() {
		s.hub.Broadcast(PushArtifact(KindArtifactChanged, id, ArtifactChange{
			Artifact: id, What: artifactsdomain.MoveReset}, now))
	}
	s.rereadData(now)
}

// queueDecision asks the decider to decide id's room again.
func (s *Service) queueDecision(id artifacts.ID) {
	s.work.mu.Lock()
	s.work.pending[id] = true
	s.work.mu.Unlock()
	select {
	case s.work.wake <- struct{}{}:
	default:
	}
}

// decideRooms decides queued rooms again until ctx ends.
func (s *Service) decideRooms(ctx context.Context) {
	defer close(s.work.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.work.wake:
		}
		s.work.mu.Lock()
		ids := make([]artifacts.ID, 0, len(s.work.pending))
		for id := range s.work.pending {
			ids = append(ids, id)
		}
		clear(s.work.pending)
		s.work.mu.Unlock()
		slices.SortFunc(ids, func(a, b artifacts.ID) int { return strings.Compare(a.String(), b.String()) })
		for _, id := range ids {
			for _, m := range s.hub.Members(id) {
				s.decideMember(ctx, m.Client.Audience().Principal(), id, m)
			}
		}
	}
}

// recheckRooms decides every room the socket is in again, as whoever the
// latest decision of its credential resolved: the rooms' half of the watch's
// own recheck.
func (s *Service) recheckRooms(ctx context.Context, client *Client) {
	if !s.rooms.serves() {
		return
	}
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return
	}
	for id, m := range s.hub.Artifacts(client) {
		s.decideMember(ctx, principal, id, m)
	}
}

// decideMember decides one membership of id's room again: a refusal withdraws
// it with an error frame naming the artifact, an undecidable answer keeps it
// at the role it had, and an admission keeps it at the role found now, telling
// the member to re-read anything withheld meanwhile.
func (s *Service) decideMember(ctx context.Context, p iam.Principal, id artifacts.ID, m RoomMember) {
	role, err := s.rooms.Access.Standing(ctx, p, id)
	switch {
	case err != nil:
		if errors.Is(err, context.Canceled) {
			return
		}
		log.InfoContext(ctx, "stream_room_redecision_undecidable", "login", p.Login,
			"artifact", id.String(), "error", err.Error())
		role = m.Role
	case role == "":
		session, ok := s.hub.LeaveArtifactIf(m.Client, id, m.Join)
		if !ok {
			return
		}
		s.rooms.Presence.Leave(id, session)
		log.InfoContext(ctx, "stream_room_withdrawn", "login", p.Login, "artifact", id.String())
		m.Client.Reply(Envelope{Kind: KindError, What: artifactJoinWhat, Error: CodeNotFound,
			Artifact: id.String()})
		return
	}
	missed, ok := s.hub.Resume(m.Client, id, m.Join, role)
	if ok && missed {
		m.Client.send(s.encodeFor(m.Client, PushArtifact(KindArtifactData, id,
			ArtifactData{Artifact: id, Resync: true}, s.now())))
	}
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
