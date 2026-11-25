package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// Fleet is the in-process twin of the fleet-shared state.
//
// A SEMANTIC twin, held to the KV backend's suite, so it models what a naive
// map would not:
//
//   - Expiry against a stored deadline and the caller's clock, so a lapsed
//     claim is gone unswept; refused beyond [coord.MaxClaimTTL] as the KV's
//     bucket refuses it.
//   - A monotonic activation epoch surviving every write: it is a fencing
//     token, and a restarted counter would reissue a number.
//   - A rate window keyed by its start instant, so callers in one window share
//     a counter and the next starts at zero with nothing to delete.
type Fleet struct {
	mu sync.Mutex

	windows   map[windowKey]int
	claims    map[string]time.Time
	worked    map[string]workedEntry
	applies   map[string]coord.NodeApply
	budgets   map[string]coord.Tally
	channels  map[string]coord.Channel
	follows   map[string]followEntry
	chatReads map[string]coord.ChatReadState
	fires     map[string]map[string]time.Time
	custody   map[string]string
	rebases   map[string]rebaseEntry
	runs      map[string]coord.Record
	// runStandings is each record's standing as its last write stated it,
	// by turn id; a record whose last write stated none has no entry.
	runStandings map[string]coord.RunStanding
	// runParts are the parts beside the runs' records, by turn id and part
	// ([coord.RunPart]) — kept apart from runs as the KV keeps them under
	// keys a listing of records never reads.
	runParts     map[runPartKey]coord.Record
	secrets      map[string]coord.SecretRecord
	integrations map[string][]byte
	atlasSources map[string][]byte
	// atlasMisses is each source's misses, by source, apart from its
	// standing as the KV keeps them under a class a listing never reads.
	atlasMisses map[string][]byte
	// atlasCode is every code scan standing, by its key in the KV's
	// grammar ([coord.AtlasCodeScanKey]).
	atlasCode    map[string][]byte
	atlasRefresh map[string][]byte

	// surfaceFindings is each integration's findings record, apart from its
	// status (integrations) as the KV backend keeps it.
	surfaceFindings map[string][]byte

	// seatAccess is the seat access ledger, by integration then handle.
	seatAccess map[string]map[string][]byte

	// grantRequests is the agents' grant requests, by id.
	grantRequests map[string][]byte

	// accessReviews is each seat's pending access review, by handle, and the
	// counter its revisions are drawn from.
	accessReviews        map[string]accessReview
	accessReviewRevision uint64
	// homeBoards is each seat's Home board, by handle, its revisions drawn
	// from version.
	homeBoards map[string]homeBoard
	// instructions is each seat's Ask Crewlet instructions, by handle, their
	// revisions drawn from version.
	instructions map[string]seatInstructions
	// atlasWakes is each owning seat's atlas wake record, by its key, and the
	// counter its revisions are drawn from.
	atlasWakes        map[string]coord.AtlasWake
	atlasWakeRevision uint64
	// conversationHolds is each seat's held conversations, by conversation key.
	conversationHolds        map[uuid.UUID]map[string]coord.ConversationHold
	conversationHoldRevision uint64
	seen                     map[string]seenMark
	seenRevision             uint64
	// sessionClaims is each minted session's claim, by session id, and the
	// counter its revisions are drawn from.
	sessionClaims        map[string]coord.SessionClaim
	sessionClaimRevision uint64
	// boxReadings is each seat's box readings, by seat and cell.
	boxReadings map[boxReadingKey]coord.BoxReading
	mailboxes   map[uuid.UUID]coord.MailboxRecord

	// objectBackend is the fleet's recorded object backend, empty until a
	// node records one, and objectCollection the collector's last report.
	objectBackend    string
	objectCollection []byte

	// trashReap is the trash reaper's last report, nil until a pass
	// records one.
	trashReap []byte

	// uploadLedger is the upload reservations' ledger, version 0 until one
	// is written, and uploads each upload's record by its id.
	uploadLedger versioned
	uploads      map[string]versioned

	// saveLedgers is each seat's save ledger by its handle, kept apart from
	// the upload ledger and records as the KV keeps it under a class of its
	// own in the uploads bucket.
	saveLedgers map[string]versioned

	// nodeStatuses is every row of the nodes' status as the KV keeps it,
	// its bytes, by its key: decoded on every read, so a reader meets what
	// a real store hands back.
	nodeStatuses map[string][]byte

	pauses      map[uuid.UUID]coord.SeatPause
	positions   map[string]coord.NodePositions
	retirements map[string]coord.NodeRetirement
	holds       map[string]coord.TrimHold
	floors      map[string]coord.TrimFloor
	feedMarks   map[string]coord.FeedMark
	backups     map[string]coord.BackupPoint
	maintenance map[string]coord.MaintenanceOperation
	admissions  map[string]coord.Admission
	maintAcks   map[string]coord.MaintenanceAck
	maintRev    uint64

	// pauseWatchers are the open seat-pause watches, runWatchers the open
	// watches of each executor's row by its turn id, partWatchers those of a
	// part beside one, and chatReadWatchers the open watches of the chat read
	// state, each told of every change under the lock that made it.
	pauseWatchers    map[*watcher[coord.SeatPauseUpdate]]struct{}
	runWatchers      map[string]map[*watcher[coord.SandboxRunUpdate]]struct{}
	partWatchers     map[runPartKey]map[*watcher[coord.SandboxRunUpdate]]struct{}
	chatReadWatchers map[*watcher[coord.ChatReadUpdate]]struct{}

	// retries and signIns are two classes keyed by a model entry, each with
	// its own open watches, and modelChecks the third, which nothing
	// watches.
	retries     entryRecords[coord.ModelRetry, coord.ModelRetryUpdate]
	signIns     entryRecords[coord.DeviceSignIn, coord.DeviceSignInUpdate]
	modelChecks entryRecords[coord.ModelCheck, coord.ModelCheck]

	// listings are each node's listing of each MCP server, by its key, and
	// definitions each tool definition by its digest.
	listings    entryRecords[coord.MCPListing, coord.MCPListingUpdate]
	definitions map[string]coord.MCPToolDefinition

	// restarts are the MCP restarts people asked for, by entry
	// ([coord.MCPRestartEntry]).
	restarts entryRecords[coord.MCPRestart, coord.MCPRestartUpdate]

	// checks are each MCP server's latest test, by the server's name.
	checks entryRecords[coord.MCPCheck, coord.MCPCheck]
	// sshTests are each server's latest test with each authority, by entry
	// ([coord.SSHTestEntry]).
	sshTests entryRecords[coord.SSHTest, coord.SSHTest]
	// issueReports are the engine defects the company reported, by fingerprint.
	issueReports entryRecords[coord.IssueReport, coord.IssueReport]
	// insightRecords are the operating insights the company shares, by class and week.
	insightRecords entryRecords[coord.InsightRecord, coord.InsightRecord]
	// accepted are the definitions MCP tools' levels were decided on, by
	// their whole key.
	accepted entryRecords[coord.MCPAcceptedTool, coord.MCPAcceptedUpdate]

	// oauth are each family's OAuth grants and sign-ins in progress, by
	// the grant's name.
	oauth map[coord.GrantFamily]*grantFamilyRecords

	// googleIdentities are each seat's Google growth identity, by handle.
	googleIdentities entryRecords[coord.GoogleIdentity, coord.GoogleIdentity]

	// version is the one counter every versioned write draws from, sandbox
	// runs, mailbox records, chat read records, trim holds and Home boards alike. Store-wide rather
	// than per record, as a KV revision is, so a record deleted and created
	// again never hands back a version an older incarnation of it already
	// used: a caller still holding that version must lose, not win against a
	// record it never read. A per-record counter restarting at 1 was exactly
	// that bug.
	version uint64

	epoch  int64
	target coord.Activation
	set    bool

	// bodies is every activation body written, by digest, with the instant
	// of its last write, until [Fleet.SweepBodies] removes one no pointer
	// names: the KV's content-addressed keys.
	bodies map[string]storedBody
}

// storedBody is one activation body and when it was last written.
type storedBody struct {
	payload json.RawMessage
	at      time.Time
}

// windowKey is one window's count: the counter's name, the window's width and
// its start — the width as the KV backend's key carries it, so one name
// counted at two widths keeps two counts.
type windowKey struct {
	bucket string
	width  time.Duration
	start  int64
}

type workedEntry struct {
	at     time.Time
	detail string
}

// rebaseEntry is one recorded rebase and the version it was written at.
type rebaseEntry struct {
	at      time.Time
	version uint64
}

var _ coord.Fleet = (*Fleet)(nil)

// NewFleet returns an empty twin.
func NewFleet() *Fleet {
	return &Fleet{
		windows:           map[windowKey]int{},
		claims:            map[string]time.Time{},
		worked:            map[string]workedEntry{},
		applies:           map[string]coord.NodeApply{},
		budgets:           map[string]coord.Tally{},
		channels:          map[string]coord.Channel{},
		follows:           map[string]followEntry{},
		chatReads:         map[string]coord.ChatReadState{},
		fires:             map[string]map[string]time.Time{},
		custody:           map[string]string{},
		rebases:           map[string]rebaseEntry{},
		runs:              map[string]coord.Record{},
		runStandings:      map[string]coord.RunStanding{},
		runParts:          map[runPartKey]coord.Record{},
		secrets:           map[string]coord.SecretRecord{},
		integrations:      map[string][]byte{},
		surfaceFindings:   map[string][]byte{},
		atlasSources:      map[string][]byte{},
		atlasMisses:       map[string][]byte{},
		atlasCode:         map[string][]byte{},
		atlasRefresh:      map[string][]byte{},
		seatAccess:        map[string]map[string][]byte{},
		grantRequests:     map[string][]byte{},
		accessReviews:     map[string]accessReview{},
		homeBoards:        map[string]homeBoard{},
		instructions:      map[string]seatInstructions{},
		atlasWakes:        map[string]coord.AtlasWake{},
		conversationHolds: map[uuid.UUID]map[string]coord.ConversationHold{},
		sessionClaims:     map[string]coord.SessionClaim{},
		seen:              map[string]seenMark{},
		boxReadings:       map[boxReadingKey]coord.BoxReading{},
		mailboxes:         map[uuid.UUID]coord.MailboxRecord{},
		pauses:            map[uuid.UUID]coord.SeatPause{},
		uploads:           map[string]versioned{},
		saveLedgers:       map[string]versioned{},
		nodeStatuses:      map[string][]byte{},
		retries:           newRetryRecords(),
		signIns:           newSignInRecords(),
		modelChecks:       newModelCheckRecords(),
		listings:          newListingRecords(),
		restarts:          newRestartRecords(),
		checks:            newCheckRecords(),
		sshTests:          newSSHTestRecords(),
		issueReports:      newIssueReportRecords(),
		insightRecords:    newInsightRecords(),
		oauth:             newGrantFamilies(),
		googleIdentities:  newGoogleIdentityRecords(),
		accepted:          newAcceptedRecords(),
		bodies:            map[string]storedBody{},
	}
}

// Allow increments a bucket's window and reports whether it stayed in limit.
func (f *Fleet) Allow(_ context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error) {
	if bucket == "" {
		return false, errors.New("coord/memory: a rate bucket needs a name")
	}
	if limit <= 0 || window <= 0 {
		// A limit of zero means "allow nothing", and a caller reaching
		// here with one has already decided; answering false is the only
		// reading of it that is not an invitation to divide by zero.
		return false, nil
	}
	if window > coord.MaxCounterWindow {
		// REFUSED AS THE KV BACKEND REFUSES IT: no bucket there keeps a
		// window this long, so a twin that counted it would pass a caller
		// the fleet refuses.
		return false, fmt.Errorf("coord/memory: a counter window of %v is longer than the %v any backend keeps "+
			"(coord.MaxCounterWindow)", window, coord.MaxCounterWindow)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := windowKey{bucket: bucket, width: window, start: now.Truncate(window).UnixNano()}
	// Older windows are dropped as they are passed rather than swept: the
	// twin runs inside one test process, and a sweep would be state the KV
	// backend does not have — the server expires those keys.
	for existing := range f.windows {
		if existing.bucket == bucket && existing.width == window && existing.start < key.start {
			delete(f.windows, existing)
		}
	}
	if f.windows[key] >= limit {
		return false, nil
	}
	f.windows[key]++
	return true, nil
}

// Claim records a key, reporting whether this caller was first.
//
// An empty key is an ERROR rather than (false, nil), as [coord.Claims] states:
// a caller that named nothing has not lost a race to anybody.
func (f *Fleet) Claim(_ context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	if key == "" {
		return false, errors.New("coord/memory: a claim needs a key")
	}
	if ttl <= 0 {
		return false, errors.New("coord/memory: a claim needs a positive ttl")
	}
	// REFUSED as the KV backend refuses it, whose claims bucket keeps a
	// record for coord.MaxClaimTTL and no longer: a twin that honoured any
	// ttl would certify a dedupe window production cannot keep.
	if ttl > coord.MaxClaimTTL {
		return false, fmt.Errorf("coord/memory: a %v claim exceeds coord.MaxClaimTTL (%v): %w",
			ttl, coord.MaxClaimTTL, coord.ErrTTLTooLong)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if until, held := f.claims[key]; held && until.After(now) {
		return false, nil
	}
	// A LAPSED claim is re-claimable, which is what makes a deliberate
	// replay work: the record is not a tombstone, it is a window.
	f.claims[key] = now.Add(ttl)
	return true, nil
}

// Release drops a claim.
func (f *Fleet) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, key)
	return nil
}

// Worked returns the subset of keys already recorded under scope.
func (f *Fleet) Worked(_ context.Context, scope string, keys []string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		if _, done := f.worked[scope+"\x00"+key]; done {
			out[key] = true
		}
	}
	return out, nil
}

// WorkedAt returns the keys already recorded under scope, each with the
// instant its first record stated.
func (f *Fleet) WorkedAt(_ context.Context, scope string, keys []string) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		if entry, done := f.worked[scope+"\x00"+key]; done {
			out[key] = entry.at
		}
	}
	return out, nil
}

// WorkedBy returns the keys already recorded under scope, each with the detail
// its first record stated.
func (f *Fleet) WorkedBy(_ context.Context, scope string, keys []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if entry, done := f.worked[scope+"\x00"+key]; done {
			out[key] = entry.detail
		}
	}
	return out, nil
}

// Record marks one key worked.
func (f *Fleet) Record(_ context.Context, scope, key, detail string, at time.Time) error {
	if scope == "" || key == "" {
		return errors.New("coord/memory: a ledger entry needs a scope and a key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// FIRST WRITER WINS. A second Record for one key is not an error and
	// does not overwrite: two nodes completing one trigger is exactly the
	// case the ledger exists to collapse, and the first one's detail is
	// the one that describes the turn that actually ran.
	id := scope + "\x00" + key
	if _, done := f.worked[id]; done {
		return nil
	}
	f.worked[id] = workedEntry{at: at, detail: detail}
	return nil
}

// Activate publishes a new target revision.
func (f *Fleet) Activate(_ context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if req.RevisionID == "" {
		return coord.Activation{}, errors.New("coord/memory: an activation needs a revision id")
	}
	if err := req.Origin.Check(); err != nil {
		return coord.Activation{}, fmt.Errorf("coord/memory: activate %s: %w", req.RevisionID, err)
	}
	// THE KV's OWN FORM, so a body reads back here as the broker would
	// hand it back — see [coord.CanonicalPayload].
	payload, err := coord.CanonicalPayload(req.Payload)
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// THE EXPECTATION FIRST, so a caller that has already lost the race
	// changes nothing.
	//
	// The mutex makes the compare and the set one step here, which is
	// exactly what the KV buys with a sequence: the twin has to REFUSE the
	// same writes, not merely be safe by construction, or a suite that
	// passes against it proves nothing about the real store.
	// An unset pointer IS a race for an expectation, since what the write
	// was built on is gone — see [coord.ActivationRequest.Expect].
	if req.Expect != "" && req.ExpectAbsent {
		return coord.Activation{}, errors.New("coord/memory: an activation cannot " +
			"expect a revision and no revision at once")
	}
	if req.ExpectEpoch != 0 && req.Expect == "" {
		return coord.Activation{}, errors.New("coord/memory: an activation cannot " +
			"expect an epoch without the revision it names")
	}
	if req.Expect != "" && !f.set {
		return coord.Activation{}, fmt.Errorf(
			"%w: expected %s, the fleet has no activation",
			coord.ErrActivationRaced, req.Expect)
	}
	if req.Expect != "" && f.target.RevisionID != req.Expect {
		return coord.Activation{}, fmt.Errorf(
			"%w: expected %s, the fleet is on %s",
			coord.ErrActivationRaced, req.Expect, f.target.RevisionID)
	}
	if req.ExpectEpoch != 0 && f.target.Epoch != req.ExpectEpoch {
		return coord.Activation{}, fmt.Errorf(
			"%w: expected %s at epoch %d, the fleet re-activated it at epoch %d",
			coord.ErrActivationRaced, req.Expect, req.ExpectEpoch, f.target.Epoch)
	}
	// CREATE-ONLY, and the comparison an expectation cannot make: this
	// write was built on nothing, so any pointer at all is a race with it.
	if req.ExpectAbsent && f.set {
		return coord.Activation{}, fmt.Errorf(
			"%w: expected no activation, the fleet is on %s",
			coord.ErrActivationRaced, f.target.RevisionID)
	}

	// HELD TO THE KV's BOUNDS, the body to what one key holds and the
	// pointer, at its widest, to its own, refused before anything changes.
	if err = coord.HoldValue("the activation's body", len(payload), coord.MaxValueBytes); err != nil {
		return coord.Activation{}, err
	}
	size, err := coord.ActivationBytes(req)
	if err != nil {
		return coord.Activation{}, fmt.Errorf("coord/memory: %w", err)
	}
	if err = coord.HoldValue("the activation pointer", size, coord.MaxPointerBytes); err != nil {
		return coord.Activation{}, err
	}
	// The body, then the pointer, as the KV writes them (see
	// [coord.Plane.Activate]), here in one step under the mutex, so no body
	// is left by a write that lost, as the KV's may be until its sweep. THE INSTANT
	// IS DECIDED UNDER THE SAME MUTEX as the compare, against the pointer
	// being replaced — see [coord.ActivationAt].
	var previous time.Time
	if f.set {
		previous = f.target.At
	}
	at := coord.ActivationAt(req.At, previous)
	body := coord.BodyOf(payload)
	f.bodies[body.SHA256] = storedBody{payload: payload, at: time.Now()}
	// TWO REVISIONS, as the KV spends: the body's write, then the pointer's.
	// Consecutive here, a case could count on epochs the store never gives.
	f.epoch += 2
	f.target = coord.Activation{
		Epoch: f.epoch, RevisionID: req.RevisionID, At: at, Summary: req.Summary,
		Origin: req.Origin.UTC(), Restored: req.Restore, Body: body,
	}
	f.set = true
	return f.target, nil
}

// Body returns the body a names, false once the sweep removed it.
func (f *Fleet) Body(_ context.Context, a coord.Activation) (json.RawMessage, bool, error) {
	if !a.Body.Named() {
		return nil, false, fmt.Errorf("coord/memory: read the body of %s: %w", a.RevisionID, coord.ErrBodyless)
	}
	if err := a.Body.Check(); err != nil {
		return nil, false, fmt.Errorf("coord/memory: read the body of %s: %w", a.RevisionID, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, found := f.bodies[a.Body.SHA256]
	if !found {
		return nil, false, nil
	}
	if !a.Body.Describes(stored.payload) {
		return nil, false, fmt.Errorf("coord/memory: the body %s is not the %d bytes the "+
			"activation of %s names: %w", a.Body.SHA256, a.Body.Bytes, a.RevisionID, coord.ErrBodyCorrupt)
	}
	return slices.Clone(stored.payload), true, nil
}

// BodyCrossing is none: the twin is in the node's own process.
func (f *Fleet) BodyCrossing(coord.Activation) time.Duration { return 0 }

// Target reads the pointer.
func (f *Fleet) Target(context.Context) (coord.Activation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.set {
		return coord.Activation{}, false, nil
	}
	return f.target, true, nil
}

// RecordApply publishes this node's status for an epoch.
func (f *Fleet) RecordApply(_ context.Context, status coord.NodeApply) error {
	if status.NodeID == "" {
		return errors.New("coord/memory: an apply status needs a node id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	status.UpdatedAt = status.UpdatedAt.UTC()
	status.Error = coord.TruncateApplyError(status.Error)
	f.applies[status.NodeID] = status
	return nil
}

// Fleet returns every node's last status, freshest first.
func (f *Fleet) Fleet(context.Context) ([]coord.NodeApply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]coord.NodeApply, 0, len(f.applies))
	for _, status := range f.applies {
		out = append(out, status)
	}
	// Freshest first, and the node id breaks a tie, so two nodes that
	// reported in the same millisecond order the same way on every read.
	slices.SortFunc(out, func(a, b coord.NodeApply) int {
		// NEWEST FIRST, so the negated compare; the node id breaks a tie
		// ascending so one instant's statuses have a stable order.
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(a.NodeID, b.NodeID))
	})
	return out, nil
}

// ExpireApplies drops apply statuses older than the cutoff.
//
// The twin's stand-in for the KV bucket's own MaxAge, which the server
// enforces. Exported so the contract suite can drive the same freshness
// behaviour against both.
func (f *Fleet) ExpireApplies(cutoff time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for node, status := range f.applies {
		if status.UpdatedAt.Before(cutoff) {
			delete(f.applies, node)
		}
	}
}

// ---- the token counters ------------------------------------------------ //

// Charge records a round in the org's counter and the seat's, in every
// window, and judges whether it fitted.
//
// The twin holds ONE mutex for the whole call, so the partial the KV backend
// reports for a seat write failing after the company's
// ([coord.SeatUncountedError]) never arises: no second writer and no failing
// write, so that path would have no test that could reach it. The arithmetic
// is [coord.Tally]'s, as in the KV backend: the company counted and judged
// first, then the seat (no verdict of its own when the company refused, the
// round counted there all the same); every answer, a refusal's included,
// carries both counters as the charge left them ([coord.Spend.Org]).
func (f *Fleet) Charge(_ context.Context, req coord.ChargeRequest) (coord.Spend, error) {
	if req.Tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if err := req.Validate(); err != nil {
		return coord.Spend{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now().UTC()
	org, refusing := f.budgets[coord.OrgScope].Count(req.Tokens, req.OrgCaps, req.Windows, now)
	f.budgets[coord.OrgScope] = org
	if len(refusing) > 0 {
		// The refusal is stamped on the windows of the scope that made it,
		// and on no other: see coord.WindowUsage.RefusedAt.
		seat, _ := f.budgets[req.Seat].Count(req.Tokens, nil, req.Windows, now)
		f.budgets[req.Seat] = seat
		refused := org.Refusal("org", refusing, req.OrgCaps, req.Windows)
		refused.Org, refused.Agent = org.Usage(coord.OrgScope, req.Windows), seat.Usage(req.Seat, req.Windows)
		return refused, nil
	}
	seat, refusing := f.budgets[req.Seat].Count(req.Tokens, req.SeatCaps, req.Windows, now)
	if len(refusing) > 0 {
		f.budgets[req.Seat] = seat
		refused := seat.Refusal("agent", refusing, req.SeatCaps, req.Windows)
		refused.Org, refused.Agent = org.Usage(coord.OrgScope, req.Windows), seat.Usage(req.Seat, req.Windows)
		return refused, nil
	}
	// ADMITTED, which is also what clears both scopes' refusals: each has
	// just had room in every window.
	org, seat = org.ClearAll(), seat.ClearAll()
	f.budgets[coord.OrgScope], f.budgets[req.Seat] = org, seat
	return coord.Spend{
		OK: true, Org: org.Usage(coord.OrgScope, req.Windows), Agent: seat.Usage(req.Seat, req.Windows),
	}, nil
}

// PostCharge adds spend that already happened to both counters, refusing
// nothing and leaving both refusal stamps as they were. See
// [coord.Budgets.PostCharge].
func (f *Fleet) PostCharge(_ context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error) {
	if tokens <= 0 {
		return coord.Spend{OK: true}, nil
	}
	if seat == "" {
		return coord.Spend{}, errors.New("coord/memory: a charge needs a seat scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Spend{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	org := f.budgets[coord.OrgScope].Roll(windows).Add(tokens, now)
	agent := f.budgets[seat].Roll(windows).Add(tokens, now)
	f.budgets[coord.OrgScope], f.budgets[seat] = org, agent
	return coord.Spend{
		OK: true, Org: org.Usage(coord.OrgScope, windows), Agent: agent.Usage(seat, windows),
	}, nil
}

// PostChargeSeat adds spend that already happened to one seat's counter
// alone, refusing nothing and leaving its refusal stamps as they were. See
// [coord.Budgets.PostChargeSeat].
func (f *Fleet) PostChargeSeat(_ context.Context, seat string, tokens int, windows coord.Windows) (coord.Usage, error) {
	if tokens <= 0 {
		return coord.Usage{}, nil
	}
	if seat == "" {
		return coord.Usage{}, errors.New("coord/memory: a charge needs a seat scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	agent := f.budgets[seat].Roll(windows).Add(tokens, time.Now().UTC())
	f.budgets[seat] = agent
	return agent.Usage(seat, windows), nil
}

// PostChargeOrg adds spend that already happened to the company's counter
// alone, refusing nothing and leaving its refusal stamps as they were. See
// [coord.Budgets.PostChargeOrg].
func (f *Fleet) PostChargeOrg(_ context.Context, tokens int, windows coord.Windows) (coord.Usage, error) {
	if tokens <= 0 {
		return coord.Usage{}, nil
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	org := f.budgets[coord.OrgScope].Roll(windows).Add(tokens, time.Now().UTC())
	f.budgets[coord.OrgScope] = org
	return org.Usage(coord.OrgScope, windows), nil
}

// Refuse stamps every full window of one scope with a refusal the gate made
// without a charge, counting nothing. See [coord.Budgets.Refuse].
//
// The arithmetic is [coord.Tally.Refuse], the KV backend's too, and a scope
// it stamps nothing on is left out of the map exactly as a scope nothing has
// charged is, so a listing never gains a row for a refusal that wrote nothing.
func (f *Fleet) Refuse(_ context.Context, scope string, caps coord.Caps, windows coord.Windows) (coord.Usage, error) {
	if scope == "" {
		return coord.Usage{}, errors.New("coord/memory: a refusal needs a scope")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/memory: %w", err)
	}
	if err := caps.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stamped, full := f.budgets[scope].Refuse(caps, windows, time.Now().UTC())
	if len(full) > 0 {
		f.budgets[scope] = stamped
	}
	return stamped.Usage(scope, windows), nil
}

// Used reports one scope's counter against the given windows.
func (f *Fleet) Used(_ context.Context, scope string, windows coord.Windows) (coord.Usage, error) {
	if scope == "" {
		return coord.Usage{}, errors.New("coord/memory: a budget scope is required")
	}
	if err := windows.Validate(); err != nil {
		return coord.Usage{}, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgets[scope].Usage(scope, windows), nil
}

// Usage returns every counter against the given windows, org first then seats
// by scope.
//
// Nothing here ages a counter out: the twin lives for one process, a far
// shorter span than [coord.BudgetRetention], which is the KV bucket's age and
// no part of what a caller can observe of a counter it is still charging.
func (f *Fleet) Usage(_ context.Context, windows coord.Windows) ([]coord.Usage, error) {
	if err := windows.Validate(); err != nil {
		return nil, fmt.Errorf("coord/memory: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.Usage, 0, len(f.budgets))
	for scope, tally := range f.budgets {
		out = append(out, tally.Usage(scope, windows))
	}
	coord.SortUsage(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ---- the agent-to-agent channels --------------------------------------- //

// OpenChannel records a new channel, ignoring an id that already exists.
//
// Ignoring rather than overwriting: the id is minted per ask, so a collision
// means a retried publish of ONE ask, and overwriting would reset the message
// counter and replace the participants of a channel already carrying an
// answer.
func (f *Fleet) OpenChannel(_ context.Context, ch coord.Channel) error {
	if ch.ID == "" {
		return errors.New("coord/memory: a channel needs an id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.channels[ch.ID]; exists {
		return nil
	}
	f.channels[ch.ID] = normalizeChannel(ch)
	return nil
}

// Channel reads one record.
func (f *Fleet) Channel(_ context.Context, id string) (coord.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	// A VALUE. coord.Channel holds no reference types, so the copy is
	// complete — a field that later does will break this, which is why the
	// suite asserts it rather than trusting the shape.
	return ch, ok, nil
}

// CloseChannel ends a channel, leaving an already-closed one untouched.
func (f *Fleet) CloseChannel(_ context.Context, id string, at time.Time) (coord.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	if !ok {
		return coord.Channel{}, false, nil
	}
	if ch.Open() {
		ch.ClosedAt = at.UTC()
		ch.LastAt = at.UTC()
		f.channels[id] = ch
	}
	return ch, true, nil
}

// CountChannelMessage records one message against a channel's own budget.
func (f *Fleet) CountChannelMessage(_ context.Context, id string, at time.Time) (coord.Channel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	if !ok {
		return coord.Channel{}, false, nil
	}
	ch.Messages++
	ch.LastAt = at.UTC()
	f.channels[id] = ch
	return ch, true, nil
}

// OpenChannels returns every channel still open, by id.
func (f *Fleet) OpenChannels(context.Context) ([]coord.Channel, error) {
	return f.listChannels(coord.Channel.Open), nil
}

// AllChannels returns every channel this store still holds, by id.
func (f *Fleet) AllChannels(context.Context) ([]coord.Channel, error) {
	return f.listChannels(func(coord.Channel) bool { return true }), nil
}

// listChannels is the one walk both listings take — see the KV store's own
// note: two near-identical loops are how one of them stops matching.
func (f *Fleet) listChannels(keep func(coord.Channel) bool) []coord.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []coord.Channel
	for _, id := range slices.Sorted(maps.Keys(f.channels)) {
		if ch := f.channels[id]; keep(ch) {
			out = append(out, ch)
		}
	}
	return out
}

// PurgeChannels deletes channels closed before the cutoff.
func (f *Fleet) PurgeChannels(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, ch := range f.channels {
		if !ch.Open() && ch.ClosedAt.Before(cutoff) {
			delete(f.channels, id)
			n++
		}
	}
	return n, nil
}

// normalizeChannel puts every stamp in UTC.
//
// The KV backend gets this for free — JSON round-trips a time.Time through
// RFC 3339 and back in UTC — so the twin has to do it explicitly or the two
// backends disagree about a stamp a caller handed in with a zone.
func normalizeChannel(ch coord.Channel) coord.Channel {
	ch.OpenedAt = ch.OpenedAt.UTC()
	ch.LastAt = ch.LastAt.UTC()
	if !ch.ClosedAt.IsZero() {
		ch.ClosedAt = ch.ClosedAt.UTC()
	}
	return ch
}

// ---- the scheduled-fire claims ----------------------------------------- //

// ClaimFire records one runner's claim on a minute, reporting whether this
// call wrote it.
//
// KEPT BY MINUTE, each minute's runners together, which is the shape the KV
// backend's keys have and what lets [Fleet.FireMinuteClaimed] answer without
// reading every claim.
func (f *Fleet) ClaimFire(_ context.Context, minute, runner string, at time.Time) (bool, error) {
	if minute == "" {
		return false, errors.New("coord/memory: a fire claim needs a minute")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	runners := f.fires[minute]
	if _, taken := runners[runner]; taken {
		return false, nil
	}
	if runners == nil {
		runners = map[string]time.Time{}
		f.fires[minute] = runners
	}
	runners[runner] = at.UTC()
	return true, nil
}

// FireMinuteClaimed reports whether any runner, or none, holds the minute.
func (f *Fleet) FireMinuteClaimed(_ context.Context, minute string) (bool, error) {
	if minute == "" {
		return false, errors.New("coord/memory: a fire minute read needs a minute")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fires[minute]) > 0, nil
}

// ---- the custody claims ----------------------------------------------- //

// ClaimCustody records node as the keeper of batch unless one is recorded,
// answering the keeper. The bucket's age is not modelled, for the completion
// ledger's reason.
//
// It never answers [coord.ErrCustodyUndecidable]: this store holds nothing a
// claim did not write, and a claim names its node or is refused, so every
// keeper it holds is one a later claim can be told.
func (f *Fleet) ClaimCustody(_ context.Context, batch, node string) (string, error) {
	if batch == "" || node == "" {
		return "", errors.New("coord/memory: a custody claim needs a batch and a node")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.custody[batch]; ok {
		return held, nil
	}
	f.custody[batch] = node
	return node, nil
}

// ---- the rebases -------------------------------------------------------- //

// Rebase reads the instant recorded for a seed and its version.
//
// The bucket's age is not modelled, for the reason the completion ledger's is
// not: it is a property of the bucket the KV backend creates, held there, and
// a twin inside one test process never runs for a month.
func (f *Fleet) Rebase(_ context.Context, seed string) (time.Time, uint64, error) {
	if seed == "" {
		return time.Time{}, 0, errors.New("coord/memory: a rebase needs a seed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.rebases[seed]
	if !ok {
		return time.Time{}, 0, nil
	}
	return entry.at, entry.version, nil
}

// RecordRebase writes a seed's instant, conditional on the version read.
func (f *Fleet) RecordRebase(_ context.Context, seed string, at time.Time, version uint64) (bool, error) {
	if seed == "" {
		return false, errors.New("coord/memory: a rebase needs a seed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.rebases[seed]
	switch {
	case version == 0 && ok, version != 0 && (!ok || entry.version != version):
		return false, nil
	}
	// DRAWN FROM THE STORE-WIDE COUNTER, like every versioned write here, so
	// a version a caller holds can never name a later record.
	f.version++
	f.rebases[seed] = rebaseEntry{at: at.UTC(), version: f.version}
	return true, nil
}

// ---- the sealed credentials -------------------------------------------- //
//
// The twin stores what it is given, exactly as the KV does: the value is an
// envelope the Tier A keyring produced before it arrived, and neither backend
// can open one. A twin that "helpfully" held plaintext would certify a
// contract the real store does not implement.

// Secret reads one sealed value.
func (f *Fleet) Secret(_ context.Context, name string) (coord.SecretRecord, bool, error) {
	if name == "" {
		return coord.SecretRecord{}, false, errors.New("coord/memory: a secret needs a name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.secrets[name]
	return rec, ok, nil
}

// SecretValues returns every sealed value, by name.
func (f *Fleet) SecretValues(_ context.Context) ([]coord.SecretRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.SecretRecord, 0, len(f.secrets))
	for _, rec := range f.secrets {
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b coord.SecretRecord) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// PutSecret writes a sealed value, replacing any prior one.
func (f *Fleet) PutSecret(_ context.Context, rec coord.SecretRecord) error {
	if err := checkSecret(rec); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storeSecret(rec)
	return nil
}

// checkSecret refuses a row no store should hold, for every write alike: by
// the encoding the KV backend stores, so the twin never certifies a record,
// past its class's bound among them, that the store refuses.
func checkSecret(rec coord.SecretRecord) error {
	_, err := coord.EncodeSecret(rec)
	return err
}

// storeSecret writes one row at the next store-wide version. The caller holds
// the lock.
//
// THE VERSION IS THE STORE'S, never the caller's: a write ignores whatever
// version the record it was handed carries, exactly as the KV assigns its own
// revision, so a caller replaying a record it read cannot hand an old version
// back to the store.
func (f *Fleet) storeSecret(rec coord.SecretRecord) {
	f.version++
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	rec.Version = f.version
	f.secrets[rec.Name] = rec
}

// CreateSecret writes a sealed value only where none is stored.
func (f *Fleet) CreateSecret(_ context.Context, rec coord.SecretRecord) (bool, error) {
	if err := checkSecret(rec); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.secrets[rec.Name]; exists {
		return false, nil
	}
	f.storeSecret(rec)
	return true, nil
}

// UpdateSecret replaces a sealed value only while it is still at version.
func (f *Fleet) UpdateSecret(_ context.Context, rec coord.SecretRecord, version uint64) (bool, error) {
	if err := checkSecret(rec); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.secrets[rec.Name]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.storeSecret(rec)
	return true, nil
}

// DeleteSecret removes a value, reporting whether it was there.
func (f *Fleet) DeleteSecret(_ context.Context, name string) (bool, error) {
	if name == "" {
		return false, errors.New("coord/memory: a secret needs a name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.secrets[name]; !ok {
		return false, nil
	}
	delete(f.secrets, name)
	return true, nil
}

// ---- the integration reconcile status ---------------------------------- //

// IntegrationStatuses returns every recorded status, keyed by surface.
func (f *Fleet) IntegrationStatuses(context.Context) (map[string][]byte, error) {
	return f.integrationClass(f.integrations), nil
}

// IntegrationFindings returns every recorded findings record, keyed by
// surface.
func (f *Fleet) IntegrationFindings(context.Context) (map[string][]byte, error) {
	return f.integrationClass(f.surfaceFindings), nil
}

// integrationClass is a copy of one of the two classes.
func (f *Fleet) integrationClass(class map[string][]byte) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(class))
	for kind, value := range class {
		// CLONED on the way out, as on the way in. A caller handed the
		// stored slice could mutate what the next reader sees, which the
		// KV backend makes impossible and a twin has to model rather than
		// merely usually get away with.
		out[kind] = bytes.Clone(value)
	}
	return out
}

// IntegrationStatus returns one surface's status, false where none.
func (f *Fleet) IntegrationStatus(_ context.Context, kind string) ([]byte, bool, error) {
	return f.integrationValue(f.integrations, kind)
}

// IntegrationFindingsOf returns one surface's findings record, false where
// none.
func (f *Fleet) IntegrationFindingsOf(_ context.Context, kind string) ([]byte, bool, error) {
	return f.integrationValue(f.surfaceFindings, kind)
}

// integrationValue is a copy of one surface's value of a class.
func (f *Fleet) integrationValue(class map[string][]byte, kind string) ([]byte, bool, error) {
	if kind == "" {
		return nil, false, errors.New("coord/memory: an integration's record is read per surface")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := class[kind]
	return bytes.Clone(value), ok, nil
}

// PutIntegrationStatus records one surface's status, held to its class's
// bound as the KV backend holds it.
func (f *Fleet) PutIntegrationStatus(_ context.Context, kind string, value []byte) error {
	if kind == "" {
		return errors.New("coord/memory: an integration status needs a surface name")
	}
	if err := coord.HoldValue(coord.IntegrationStatusKey(kind), len(value), coord.MaxIntegrationStatusBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.integrations[kind] = bytes.Clone(value)
	return nil
}

// RecordIntegrationPass records a pass's findings, or drops them where nil,
// and then its status, each held to its class's bound before either is
// written.
func (f *Fleet) RecordIntegrationPass(_ context.Context, kind string, status, findings []byte) error {
	if kind == "" {
		return errors.New("coord/memory: an integration status needs a surface name")
	}
	if err := coord.HoldValue(coord.IntegrationFindingsKey(kind), len(findings),
		coord.MaxIntegrationFindingsBytes); err != nil {
		return err
	}
	if err := coord.HoldValue(coord.IntegrationStatusKey(kind), len(status), coord.MaxIntegrationStatusBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if findings == nil {
		delete(f.surfaceFindings, kind)
	} else {
		f.surfaceFindings[kind] = bytes.Clone(findings)
	}
	f.integrations[kind] = bytes.Clone(status)
	return nil
}

// DeleteIntegrationStatus drops a surface's status and its findings.
// Deleting one that is not there is the outcome asked for rather than an
// error, matching the KV backend's purge of an absent key.
func (f *Fleet) DeleteIntegrationStatus(_ context.Context, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.integrations, kind)
	delete(f.surfaceFindings, kind)
	return nil
}

// ---- the chat thread-follows ------------------------------------------- //

// followEntry is one follow in the twin.
//
// The instant is kept although nothing here reads it, so the twin holds the
// same record the real backend does: a test that asserted on a field the twin
// silently dropped would pass against a backend that never stored it.
type followEntry struct {
	reason string
	at     time.Time
}

// followKey composes one follow's key, through the SAME grammar the real
// backend uses — so a segment containing a separator collides in both or in
// neither, and the conformance suite's case for it means something.
func followKey(backend, handle, channel, thread string) string {
	return coord.DocumentKey("follow", backend, handle, channel, thread)
}

// Follow records or refreshes a seat's follow on a thread, writing the reason
// choose answers for the record it replaces.
//
// ONE MUTEX ACROSS THE READ, THE CHOICE AND THE WRITE, which is the property
// the KV backend reaches by binding its write to the revision it read: no
// other follow can land between what choose was shown and what is written.
func (f *Fleet) Follow(_ context.Context, backend, handle, channel, thread string, at time.Time,
	choose func(held string, following bool) string) (string, error) {

	if backend == "" || handle == "" || thread == "" {
		return "", errors.New("coord/memory: a follow needs a backend, a handle and a thread")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := followKey(backend, handle, channel, thread)
	held, following := f.follows[key]
	reason := choose(held.reason, following)
	f.follows[key] = followEntry{reason: reason, at: at.UTC()}
	return reason, nil
}

// Following reports why a seat follows a thread, and whether it does.
func (f *Fleet) Following(_ context.Context, backend, handle, channel, thread string) (string, bool, error) {
	if backend == "" || handle == "" || thread == "" {
		return "", false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.follows[followKey(backend, handle, channel, thread)]
	if !ok {
		return "", false, nil
	}
	return entry.reason, true, nil
}

// ---- the per-person chat read state ------------------------------------ //

// ChatRead reports one person's read state, the zero state meaning nobody has
// opened chat as that person yet.
func (f *Fleet) ChatRead(_ context.Context, handle string) (coord.ChatReadState, error) {
	if handle == "" {
		return coord.ChatReadState{}, errors.New(
			"coord/memory: read a person's chat state needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneChatRead(f.chatReads[handle]), nil
}

// AdvanceChatRead merges a flush into one person's record.
//
// NO RETRY LOOP HERE and that is not a simplification: the real backend's
// rounds exist to lose a compare-and-set against another PROCESS, and this
// twin holds the whole fleet under one mutex. The MERGE is the shared half —
// [coord.MergeChatRead] — so the twin and the backend cannot disagree about
// what a flush does, which is the only part a conformance suite can check.
func (f *Fleet) AdvanceChatRead(_ context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	if handle == "" {
		return coord.ChatReadState{}, errors.New(
			"coord/memory: advance a person's chat state needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	merged, err := coord.MergeChatRead(f.chatReads[handle], delta)
	if err != nil {
		return coord.ChatReadState{}, err
	}
	// HELD TO THE CLASS'S BOUND by the encoding the KV backend stores, so
	// the twin never certifies a record the store refuses. No flush reaches
	// it today (every id a UUID, every set capped: 137,953 bytes at the
	// widest); it stays so a record that outgrows the bound, a field added,
	// is refused here as the store refuses it, not certified
	// (TestTheTwinRefusesAChatReadRecordPastItsBound).
	if _, err := coord.EncodeChatRead(handle, merged); err != nil {
		return coord.ChatReadState{}, err
	}
	// THE STORE-WIDE COUNTER, as every versioned write here draws from: a
	// record forgotten and written again lands past every state the old
	// one reported, which is what a screen ordering two states relies on.
	f.version++
	merged.Revision = f.version
	f.chatReads[handle] = merged
	f.notifyChatReadLocked(handle, &merged)
	return cloneChatRead(merged), nil
}

// SetChatReadAbsence stamps or clears a record's seat's absence from the
// company, reporting whether the seat has a record; one without is left
// without. The rule is [coord.ChatReadState.WithAbsence], shared with the
// real backend.
func (f *Fleet) SetChatReadAbsence(_ context.Context, handle string, since time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: stamp a person's chat absence needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.chatReads[handle]
	if !ok {
		return false, nil
	}
	if next, changed := stored.WithAbsence(since); changed {
		// The flush's hold, for the flush's reason.
		if _, err := coord.EncodeChatRead(handle, next); err != nil {
			return false, err
		}
		f.version++
		next.Revision = f.version
		f.chatReads[handle] = next
		f.notifyChatReadLocked(handle, &next)
	}
	return true, nil
}

// ForgetChatRead removes a person's record if it has been stamped absent at or
// before absentBy, reporting whether it did.
func (f *Fleet) ForgetChatRead(_ context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: forget a person's chat state needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.chatReads[handle]
	if !ok || !stored.AbsentBy(absentBy) {
		return false, nil
	}
	delete(f.chatReads, handle)
	f.notifyChatReadLocked(handle, nil)
	return true, nil
}

// ChatReaders lists every seat holding read state, in handle order, each with
// its absence stamp.
func (f *Fleet) ChatReaders(_ context.Context) ([]coord.ChatReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	readers := make([]coord.ChatReader, 0, len(f.chatReads))
	for handle, stored := range f.chatReads {
		readers = append(readers, coord.ChatReader{Handle: handle, AbsentSince: stored.AbsentSince})
	}
	slices.SortFunc(readers, func(a, b coord.ChatReader) int { return cmp.Compare(a.Handle, b.Handle) })
	return readers, nil
}

// WatchChatReads streams every seat's read state, the marker, then every
// change, under the twin's one-lock ordering (watch.go). The KV backend's
// WatchChatReads certifies its first answer instead.
func (f *Fleet) WatchChatReads(ctx context.Context) (<-chan coord.ChatReadUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	answer := make([]coord.ChatReadUpdate, 0, len(f.chatReads)+1)
	for _, handle := range slices.Sorted(maps.Keys(f.chatReads)) {
		state := cloneChatRead(f.chatReads[handle])
		answer = append(answer, coord.ChatReadUpdate{Handle: handle, State: &state})
	}
	w := newWatcher(append(answer, coord.ChatReadUpdate{Current: true})...)
	if f.chatReadWatchers == nil {
		f.chatReadWatchers = map[*watcher[coord.ChatReadUpdate]]struct{}{}
	}
	f.chatReadWatchers[w] = struct{}{}
	return w.serve(ctx, func() {
		f.mu.Lock()
		delete(f.chatReadWatchers, w)
		f.mu.Unlock()
	}), nil
}

// notifyChatReadLocked tells every watcher that a seat's record now holds
// state, or nil when it was forgotten. Each watcher gets its own copy, maps
// and slices included, so one consumer that keeps the state cannot see
// another's edits — or the twin's next write.
func (f *Fleet) notifyChatReadLocked(handle string, state *coord.ChatReadState) {
	for w := range f.chatReadWatchers {
		u := coord.ChatReadUpdate{Handle: handle}
		if state != nil {
			copied := cloneChatRead(*state)
			u.State = &copied
		}
		w.push(u)
	}
}

// cloneChatRead copies the maps and slices a caller could otherwise mutate
// under the twin's own lock.
//
// The real backend hands back a decoded record that shares nothing with its
// store; a twin that handed out its live map would let one caller's edit reach
// another's read, and the suite would be proving a property only the twin has.
func cloneChatRead(state coord.ChatReadState) coord.ChatReadState {
	out := state
	if state.Cursors != nil {
		out.Cursors = maps.Clone(state.Cursors)
	}
	out.Muted = slices.Clone(state.Muted)
	return out
}

// SweepMarkers removes nothing, because the twin keeps no markers: a delete
// here removes the entry from its map, so the state a KV backend reaches only
// after its sweep is the state this one is always in. That is the contract's
// promise read from the other side — sweeping changes no answer — and the
// suite holds both backends to it.
func (f *Fleet) SweepMarkers(context.Context, time.Time) (int64, error) { return 0, nil }

// SweepBodies removes the activation bodies written before cutoff that the
// pointer does not name, as the KV's sweep does.
func (f *Fleet) SweepBodies(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var swept int64
	for digest, body := range f.bodies {
		if (f.set && digest == f.target.Body.SHA256) || !body.at.Before(cutoff) {
			continue
		}
		delete(f.bodies, digest)
		swept++
	}
	return swept, nil
}
