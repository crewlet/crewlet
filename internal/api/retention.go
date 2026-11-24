package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE THREE RETENTION GESTURES THAT WRITE, over HTTP for one reason: each
// changes FLEET state held in the broker, and the default embedded broker binds
// no socket, so a CLI cannot reach it while the engine is UP
// (internal/queue/jetstream's StoreDirLock). `crewlet retention ack`, `evict`
// and `readmit` are clients of these routes.
//
// All are POSTs: each moves the trim's floor or whether a machine's records
// apply, so no proxy may retry nor a browser prefetch it. `admin` decides, by
// the authority table where [App.mountDeployment] mounts them (the two reads
// included); handlers resolve the caller for the audit line only.

// retentionWriter is the slice of the fleet these routes need.
//
// Declared here, by the consumer, and deliberately not [coord.Fleet]: a route
// that could also reach the activation pointer or the secret store would
// eventually be given a reason to.
type retentionWriter interface {
	PutBackupPoint(ctx context.Context, p coord.BackupPoint) error
}

// NodeGate is the eviction half, which is a RECORD on every identity-claiming
// log rather than a coordination write — one gesture, judged once, written to
// each log, and answered per log with the same three-valued outcome every other
// write has. See [engine.NodeGate].
//
// EXPORTED, unlike its neighbour, because the caller has to convert a typed
// nil to a genuine one before handing it over: a nil *engine.NodeGate inside a
// non-nil interface passes this route's own check and panics on the first
// press.
type NodeGate interface {
	// Evict answers a [*statelog.EvictionRefusal], wrapped, for a node
	// still holding a live presence lease — which this route reports as a
	// refusal rather than a failure.
	//
	// THE REQUEST CARRIES WHO PRESSED, as the principal the guard
	// resolved, and every log's record is written AS them: an eviction's
	// history names the person, their kind and the credential they
	// pressed it through, where it used to name this node's own writer —
	// so "who stopped node-3 writing" had one answer, the node that
	// happened to serve the request. See [engine.GateRequest.By].
	Evict(ctx context.Context, req engine.GateRequest) (engine.GateResult, error)

	// Readmit answers a [*statelog.ReadmissionRefusal], wrapped, for a node
	// below a trim floor it would be counted against — likewise a refusal.
	Readmit(ctx context.Context, req engine.GateRequest) (engine.GateResult, error)
}

// serveRetentionAck answers POST /worklet/retention/ack.
//
// It publishes an OPERATOR backup floor: that a copy reached wherever the
// policy says, which the engine cannot see. `?stream=` and `?position=` are
// required, none guessed: this moves the floor the trim deletes against. One
// log per acknowledgement ([coord.BackupPointKey]).
//
// `?taken_at=` IS WHEN THE COPY WAS TAKEN, RFC 3339 ([coord.BackupPoint.At]):
// the backup-age alarm ages the COPY, so a call-time default would clear it for
// a copy days old. Zero is refused as missing, a future instant (by this node's
// clock, no allowance) as impossible.
// AN OLDER COPY MAY REPLACE A NEWER ONE: falling back is conservative.
func (a *App) serveRetentionAck(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	position, err := strconv.ParseUint(r.URL.Query().Get("position"), 10, 64)
	if stream == "" || err != nil || position == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodePositionRequired, map[string]string{
			"detail": "name the stream and the sequence your copy reaches. An " +
				"acknowledgement moves the floor the trim deletes against, so " +
				"there's no value to guess",
		})
		return
	}
	raw := r.URL.Query().Get("taken_at")
	takenAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || takenAt.IsZero() {
		// THE VALUE AS RECEIVED, because the commonest way to send a valid
		// instant and have it refused is an offset's '+' left unescaped in
		// the query, which arrives as a space.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeTakenAtRequired, map[string]string{
			"detail": fmt.Sprintf("name when your copy was taken in taken_at, as an RFC 3339 "+
				"instant such as 2026-11-17T03:00:00Z (received %q; an offset's + is sent as %%2B). "+
				"The backup's age is measured from it, so there's no value to guess", raw),
		})
		return
	}
	takenAt = takenAt.UTC()
	if now := a.now().UTC(); takenAt.After(now) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeTakenAtInFuture, map[string]string{
			"detail": fmt.Sprintf("taken_at %s is later than this node's clock, which reads %s. "+
				"Give when the copy was taken, by this node's clock",
				takenAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)),
		})
		return
	}
	generation, err := a.generationOf(stream)
	if err != nil {
		// A BARE SEQUENCE NAMES A NUMBER SPACE. Publishing one at the
		// wrong generation pins a position on a log that no longer
		// exists, which the trim reads as no acknowledgement at all —
		// so a stream this node does not run is refused rather than
		// acknowledged at some other log's generation.
		//
		// 404 AND NOT 503, which is the classification GET
		// /worklet/retention/reanchor already gives the same refusal from
		// the same call: nothing here is transient, and a 503 tells an
		// operator who typed the stream name wrong to wait and try the
		// identical request again.
		unknownStream(w, stream, err)
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	point := coord.BackupPoint{
		Owner: coord.OperatorBackupOwner,
		At:    takenAt,
		Streams: map[string]coord.Position{
			stream: {Stream: stream, Generation: generation, Seq: position},
		},
		// VERIFIED BY THE PERSON, which is the whole content of the
		// gesture: under `backup_floor: operator` the engine's own
		// verification is not what the policy trusts.
		Verified: true,
	}
	if err := a.retention.PutBackupPoint(r.Context(), point); err != nil {
		log.Warn("api_retention_ack_failed", "stream", stream, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeAckFailed)
		return
	}
	log.Info("retention_acknowledged", "by", by.Name, "operator", by.OperatorID,
		"stream", stream, "position", position, "generation", generation, "taken_at", takenAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"stream": stream, "position": position, "generation": generation, "taken_at": takenAt,
	})
}

// generationOf is the generation the NAMED stream's log stands at, taken from
// the domain this node is running rather than from the fleet's registers.
//
// The fleet's registers are keyed by DOMAIN and name no stream, and a reanchor
// moves ONE domain's generation, so none can answer per stream. A generation
// too HIGH is what this route prevents: the trim reads a point at or above its
// generation as covering the log ([statelog.TrimInputs]), so a neighbour's
// number would delete records the copy lacks. The running domain is the SAME
// number the trim compares; a node behind a reanchor answers LOW, which the
// trim's guard refuses. An unrun stream has no cursor and is REFUSED.
func (a *App) generationOf(stream string) (uint32, error) {
	// FROM MEMORY, never through the reanchor's status read: that one reads
	// the stream's creation instant LIVE from the broker, which this route
	// has no use for — and a broker that did not answer would then have
	// been reported as a stream name the operator mistyped.
	return a.capacity.StreamGeneration(stream)
}

// mountRetention registers the write half of the retention surface, through
// the mount [App.mountDeployment] hands it — which is what decides the grant.
//
// NO PURGE HERE: a work item's purge is `POST /worklet/items/{key}/purge` on the
// human write surface (internal/api/workapi), decided by
// [authz.ActionWorkPurge], its op id the `Idempotency-Key`.
func (a *App) mountRetention(mount func(string, authz.Effect, http.HandlerFunc)) {
	mount("POST /worklet/retention/ack", authz.EffectRewrites, a.serveRetentionAck)
	// AN EVICTION IS TAKEN BACK by a readmission, its inverse commit, so it
	// rewrites the node's standing rather than destroying it.
	mount("POST /worklet/retention/evict/{node}", authz.EffectRewrites, a.gate(true))
	mount("POST /worklet/retention/readmit/{node}", authz.EffectRewrites, a.gate(false))
}

// callerOpID is the operation id a caller brought in `?op_id=`, or a fresh one
// named name where it brought none.
//
// A retry is only a retry under the SAME id: a gesture that came back `unknown`
// or partial is finished by sending the id it answered with; a fresh id is a
// second gesture. THE ID CARRIES THE INSTANT IT WAS MINTED, which the state log
// reads to decide whether its ledger can vouch for the retry ([statelog.NewOpID]).
//
// An id [statelog.CheckCallerOpID] refuses is answered `400 op_id_invalid` and
// false is returned, the response written; nothing is judged or written.
func callerOpID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	opID := r.URL.Query().Get("op_id")
	if opID == "" {
		return statelog.NewOpID(time.Now(), name), true
	}
	if err := statelog.CheckCallerOpID(opID); err != nil {
		detail := "this isn't an operation id this engine would have minted"
		var refused *statelog.CallerOpIDError
		if errors.As(err, &refused) {
			detail = "the op_id " + refused.Fault
		}
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeOpIDInvalid,
			httpjson.Detail{"field": "op_id", "detail": detail + ". Send back the " +
				"op_id an earlier answer returned, unchanged, or leave it out to " +
				"start the gesture afresh"})
		return "", false
	}
	return opID, true
}

// unknownStream answers a gesture naming a stream this node does not run:
// `404 unknown_stream`, naming the streams it does ([engine.UnknownStreamError]).
func unknownStream(w http.ResponseWriter, stream string, err error) {
	detail, ok := statelog.Tell(err)
	if !ok {
		detail = fmt.Sprintf("%q isn't a log this node keeps", stream)
	}
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownStream,
		map[string]string{"detail": detail})
}

// gateVerb names a gate gesture in the fresh operation id it is minted under.
func gateVerb(evict bool) string {
	if evict {
		return "evict"
	}
	return "readmit"
}

// gateNoun names a gate gesture in a sentence.
func gateNoun(evict bool) string {
	if evict {
		return "eviction"
	}
	return "readmission"
}

// gate answers the eviction and readmission routes.
//
// ONE HANDLER FOR BOTH: a readmission is the INVERSE COMMIT, so two handlers
// would copy one refusal vocabulary. The engine judges once and writes to every
// log the trim counts nodes on ([engine.NodeGate]). A refusal is a 409 with
// nothing written (a live presence lease unless `force=true`; a trim floor the
// node would be counted against), carrying its numbers and its remedy as
// `actions` ([statelog.GateAction]) beside `hint`. Past the judgement the
// answer is 200 and PER LOG ([GateAnswer], rendered only by [RenderGate]);
// `complete` says whether every log holds the record; one left unfinished
// carries `actions` (`retry_same_op`: resend with the `op_id` it answered with).
// A dropped request does not stop a gesture half-way ([engine.GateLogBudget]).
func (a *App) gate(evict bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.PathValue("node")
		// THE CONFIRMATION ECHOES THE NODE ID, the same shape the
		// destructive CLI gestures already use: an eviction stops a
		// machine's records applying and a readmission lets them apply
		// again, and neither is a value to get from a shell history.
		if node == "" || r.URL.Query().Get("confirm") != node {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
				"detail": "repeat the node id in ?confirm=. An eviction stops " +
					"that machine's records applying anywhere in the fleet",
			})
			return
		}
		caller, ok := auth.Caller(w, r)
		if !ok {
			return
		}
		by := iam.ActorFor(caller)
		// The node through [statelog.OpNamePart], because a node id may hold
		// the dot that begins a step in the grammar.
		opID, ok := callerOpID(w, r, gateVerb(evict)+"-"+statelog.OpNamePart(node))
		if !ok {
			return
		}
		req := engine.GateRequest{
			Node: node, OpID: opID, By: caller,
			Force: evict && r.URL.Query().Get("force") == "true",
		}
		var result engine.GateResult
		var err error
		if evict {
			result, err = a.nodes.Evict(r.Context(), req)
		} else {
			result, err = a.nodes.Readmit(r.Context(), req)
		}
		if refusal, ok := RenderGateRefusal(node, opID, err); ok {
			// A REFUSAL IS AN ANSWER, NOT A FAULT: nothing was judged, or
			// the judgement said no, and nothing was written anywhere. Its
			// body is rendered in one place for the reason the 200 is; what
			// is logged is this route's own.
			logGateRefusal(by, node, err)
			refusal.Write(w)
			return
		}
		if err != nil {
			// A FAULT NOTHING TOLD: every refusal above is rendered, and
			// past the judgement the gesture answers per log rather than
			// failing, so what reaches here stopped the judgement and
			// nothing was written. Its words are the log's.
			log.Warn("api_retention_gate_failed", "node", node,
				"evict", evict, "op_id", opID, "error", err)
			httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeGateFailed,
				httpjson.Detail{"detail": fmt.Sprintf("couldn't judge the %s of %s, so "+
					"nothing was written. Try again with the same op_id; this node's "+
					"log says what went wrong", gateNoun(evict), node),
					"node": node, "op_id": opID})
			return
		}
		answer := RenderGate(evict, result)
		log.Info("retention_gate", "by", by.Name, "operator", by.OperatorID,
			"node", node, "evict", evict, "force", req.Force, "op_id", answer.OpID,
			"complete", answer.Complete)
		writeJSON(w, http.StatusOK, answer)
	}
}

// capacityRunner is the slice of the engine the capacity routes need.
//
// Declared here, by the consumer: a route that could also reach the seat host
// or the config surface would eventually be given a reason to.
type capacityRunner interface {
	Reanchor(ctx context.Context, req engine.ReanchorRequest) (statelog.ReanchorPlan, error)
	ReanchorStatus(ctx context.Context, stream string) (engine.ReanchorView, error)
	StreamGeneration(stream string) (uint32, error)
	SetCapacity(ctx context.Context, req engine.CapacityRequest) (coord.MaintenanceOperation, error)
	AbandonCapacity(ctx context.Context, stream, operationID string, by iam.Actor) (coord.MaintenanceOperation, error)
	ExcludeParticipant(ctx context.Context, stream, node string, by iam.Actor) (coord.MaintenanceOperation, error)
	RetireNode(ctx context.Context, node string, by iam.Actor) (coord.NodeRetirement, error)
	CapacityStatus(ctx context.Context, stream string) (engine.CapacityState, error)
	CapacityAcks(ctx context.Context, stream string) ([]coord.MaintenanceAck, error)
	CapacityWindows(ctx context.Context) ([]coord.MaintenanceOperation, []string)
	Mode() statelog.MaintenanceMode
}

// windowLeft is one capacity window an answer names as still open: its log,
// its operation, where it stands and its target, which is what a client needs
// to name the command that finishes it.
type windowLeft struct {
	Stream         string                 `json:"stream"`
	OperationID    string                 `json:"operation_id"`
	Phase          coord.MaintenancePhase `json:"phase"`
	TargetMaxBytes uint64                 `json:"target_max_bytes"`
}

// windowsLeft adds to a capacity gesture's answer every window open on any
// log once the gesture is done, the gesture's own included while it stands
// (`open_windows`), and every log whose window this node couldn't read
// (`unread_windows`).
//
// A NODE WON'T START IN SERVICE WHILE EITHER HOLDS, so returning the fleet to
// service is the next step only when both are empty, and only the node can say
// whether they are: a client told "restart in service" whenever its own window
// closed sent an operator resizing two logs into a fleet-wide restart every
// node refused.
func (a *App) windowsLeft(ctx context.Context, body map[string]any) {
	open, unread := a.capacity.CapacityWindows(ctx)
	left := make([]windowLeft, 0, len(open))
	for _, op := range open {
		left = append(left, windowLeft{Stream: op.Stream, OperationID: op.OperationID,
			Phase: op.Phase, TargetMaxBytes: op.TargetMaxBytes})
	}
	if unread == nil {
		unread = []string{}
	}
	body["open_windows"], body["unread_windows"] = left, unread
}

// mountCapacity registers the maintenance surface.
//
// # Why these are routes at all, on a node that starts no publisher
//
// The verb needs a broker and no publisher, and on the default topology the
// embedded broker binds NO SOCKET — so a tool outside the process has no
// address to reach it at, and a node-client command needs the node this
// procedure requires to be stopped. Both cannot hold. What resolves it is that
// the maintenance-mode node runs its API: these are that mode's own control
// surface rather than the write routes the mode withholds.
func (a *App) mountCapacity(mount func(string, authz.Effect, http.HandlerFunc)) {
	mount("POST /worklet/retention/capacity", authz.EffectRewrites, a.serveSetCapacity)
	mount("GET /worklet/retention/maintenance", "", a.serveMaintenanceStatus)
	mount("POST /worklet/retention/maintenance/abandon", authz.EffectRewrites, a.serveAbandon)
	mount("POST /worklet/retention/maintenance/exclude", authz.EffectRewrites, a.serveExclude)
	// A RETIREMENT IS TAKEN BACK by the node itself running again, so it
	// rewrites the node's standing rather than destroying it.
	mount("POST /worklet/retention/maintenance/retire", authz.EffectRewrites, a.serveRetire)
	mount("GET /worklet/retention/reanchor", "", a.serveReanchorStatus)
	// A REANCHOR DECLARES EVERY POSITION BELOW ITS NEW GENERATION STALE, and
	// on a restored log discards what the restore did not hold.
	mount("POST /worklet/retention/reanchor", authz.EffectDestroys, a.serveReanchor)
}

// serveReanchorStatus answers GET /worklet/retention/reanchor: the stream's own
// creation instant, which the confirmation has to echo, and the case a reanchor
// would answer now: `recreated` (followed from its first surviving record),
// `restored` (from its end) or `abandoned` (from this node's own checkpoint, a
// generation only an evicted peer held made void), with the sequence the
// checkpoint would go to, or why there is nothing to re-anchor. A restored log
// holding records this node's rows lack also answers `discards` (the newest)
// and `discarding`, why a reanchor refuses them without `discard=true`.
//
// A SEPARATE READ, so the confirmation says "I looked at the thing I am
// re-anchoring", not one call confirming against its own output.
func (a *App) serveReanchorStatus(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeStreamRequired)
		return
	}
	view, err := a.capacity.ReanchorStatus(r.Context(), stream)
	switch {
	case errors.Is(err, engine.ErrUnknownStream):
		unknownStream(w, stream, err)
		return
	case err != nil:
		// THE LOG EXISTS AND THE BROKER DID NOT ANSWER. The instant is read
		// live, so this is the one failure here worth retrying — and
		// reporting it as an unknown stream sent an operator looking for a
		// typo in a name that was right.
		//
		// THE RETRY-AFTER IS THE UNDECIDED SCALE ([authz.RetryUndecidedSeconds])
		// and for its reason: what the caller waits for is one read of the
		// broker and the fleet's registers coming back — a store blip, not
		// an outage — and a longer hint leaves an operator waiting on a
		// status that could already answer.
		log.Warn("api_reanchor_status_unreadable", "stream", stream, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeStreamUnreadable,
			authz.RetryUndecidedSeconds, httpjson.Detail{"detail": fmt.Sprintf(
				"couldn't read %s just now, so nothing was decided. Ask again in "+
					"a moment", stream)})
		return
	}
	answer := map[string]any{
		"stream": stream, "created_at": view.CreatedAt, "generation": view.Generation,
	}
	if view.Case != "" {
		answer["case"], answer["cursor"] = view.Case, view.Cursor
	} else {
		answer["nothing_to_reanchor"] = view.Refusal
	}
	if view.Discards != nil {
		answer["discards"], answer["discarding"] = view.Discards, view.Discarding
	}
	writeJSON(w, http.StatusOK, answer)
}

// serveReanchor answers POST /worklet/retention/reanchor.
func (a *App) serveReanchor(w http.ResponseWriter, r *http.Request) {
	stream, confirm := r.URL.Query().Get("stream"), r.URL.Query().Get("confirm")
	if stream == "" || confirm == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "echo the stream's own created_at from GET " +
				"/worklet/retention/reanchor. A reanchor declares every position " +
				"below the new generation stale, and the confirmation is what " +
				"says you looked at the stream you're re-anchoring",
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	plan, err := a.capacity.Reanchor(r.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: confirm, By: by,
		Force:   r.URL.Query().Get("force") == "true",
		Discard: r.URL.Query().Get("discard") == "true",
	})
	if err != nil {
		log.Warn("api_reanchor_refused", "stream", stream, "by", by.Name,
			"operator", by.OperatorID, "error", err)
		said, told := statelog.Tell(err)
		switch {
		case errors.Is(err, engine.ErrUnknownStream):
			unknownStream(w, stream, err)
		case told:
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeReanchorRefused,
				map[string]string{"detail": said})
		default:
			httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeReanchorFailed,
				map[string]string{"detail": fmt.Sprintf("the reanchor of %s stopped "+
					"on an error, and this node's log says which. Read where it "+
					"stands with `crewlet retention reanchor -stream %s` before "+
					"running it again", stream, stream)})
		}
		return
	}
	log.Warn("reanchored", "by", by.Name, "operator", by.OperatorID,
		"stream", stream, "generation", plan.Generation, "case", string(plan.Case),
		"cursor", plan.Cursor, "discarded", plan.Discarded != nil)
	answer := map[string]any{
		"stream": stream, "generation": plan.Generation, "case": plan.Case,
		"cursor": plan.Cursor,
	}
	if plan.Discarded != nil {
		// WHAT THE OPERATOR'S WORD DISCARDED, in the answer as well as the
		// audit row: the newest record written after the restore that is
		// now applied on no node.
		answer["discarded"] = plan.Discarded
	}
	writeJSON(w, http.StatusOK, answer)
}

// serveSetCapacity answers POST /worklet/retention/capacity.
func (a *App) serveSetCapacity(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	target, err := strconv.ParseUint(r.URL.Query().Get("bytes"), 10, 64)
	if stream == "" || err != nil || target == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeTargetRequired, map[string]string{
			"detail": "name the stream and the byte ceiling. A target is chosen " +
				"once for the life of an operation and never changed, so there's " +
				"no value to guess",
		})
		return
	}
	if confirm := r.URL.Query().Get("confirm"); confirm != strconv.FormatUint(target, 10) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "repeat the byte count in ?confirm=. This restarts the " +
				"whole fleet three times and changes what the broker will accept",
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	op, err := a.capacity.SetCapacity(r.Context(), engine.CapacityRequest{
		Stream: stream, TargetMaxBytes: target,
		By: by.Name, OperatorID: by.OperatorID,
		Assert: r.URL.Query().Get("assert_excluded") == "true",
	})
	if err != nil {
		// THE OPERATION IS RETURNED WITH THE REFUSAL where there is one:
		// a caller told only that something failed cannot tell an
		// operation that never opened from one that is open and stuck,
		// and those have opposite next steps.
		log.Warn("api_set_capacity_refused", "stream", stream, "by", by.Name,
			"operator", by.OperatorID, "error", err)
		a.capacityRefused(w, r, stream, "the capacity change", err, op)
		return
	}
	log.Info("capacity_requested", "by", by.Name, "operator", by.OperatorID,
		"stream", stream,
		"target", target, "phase", op.Phase)
	body := map[string]any{"operation": a.operationOrNil(r.Context(), op),
		"mode": a.capacity.Mode()}
	a.windowsLeft(r.Context(), body)
	writeJSON(w, http.StatusOK, body)
}

// serveMaintenanceStatus answers GET /worklet/retention/maintenance.
func (a *App) serveMaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeStreamRequired)
		return
	}
	state, err := a.capacity.CapacityStatus(r.Context(), stream)
	if errors.Is(err, engine.ErrUnknownStream) {
		unknownStream(w, stream, err)
		return
	}
	if err != nil {
		// THE LOG IS RIGHT AND COORDINATION DIDN'T ANSWER, which a wait clears,
		// so it is 503 with the undecided scale ([authz.RetryUndecidedSeconds])
		// for the reanchor status's reason: what the caller waits for is one
		// read of the registers coming back. It was a 500, which tells a
		// client the node itself faulted, under a sentence telling a person to
		// try again. THIS ERROR IS THE WINDOW'S OWN READ: a part read after it
		// that failed is answered below beside the window it read.
		log.Warn("api_maintenance_unreadable", "stream", stream, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeMaintenanceUnreadable,
			authz.RetryUndecidedSeconds, httpjson.Detail{"detail": fmt.Sprintf(
				"couldn't read whether a capacity window is open on %s just now. "+
					"Ask again in a moment, and if it keeps failing, this node's "+
					"log says why", stream)})
		return
	}
	op, found := state.Op, state.Found
	body := map[string]any{"stream": stream, "open": found, "mode": a.capacity.Mode()}
	// A PART THAT COULDN'T BE READ IS SAID AS THAT, beside the window that
	// was read, and nothing formed from it is answered: an empty list of
	// acknowledgements or admissions reads as nobody owing anything, and
	// folded into the window's own 503 it said the window couldn't be read
	// when it just had been.
	acksRead, admissionsRead := state.AcksErr == nil, state.AdmissionsErr == nil
	// AN UNJUDGED RETIREMENT IS NOT "NONE RETIRED": every list below waives
	// the retired, so each is left out with it.
	judged := state.RetiredErr == nil
	if !judged {
		log.Warn("api_maintenance_part_unreadable", "stream", stream,
			"part", "retirements", "error", state.RetiredErr)
		body["retired_unread"] = true
	}
	if acksRead {
		body["acks"] = state.Acks
	} else {
		log.Warn("api_maintenance_part_unreadable", "stream", stream,
			"part", "acknowledgements", "error", state.AcksErr)
		body["acks_unread"] = true
	}
	if admissionsRead {
		body["admissions"] = state.Admissions
	} else {
		log.Warn("api_maintenance_part_unreadable", "stream", stream,
			"part", "admissions", "error", state.AdmissionsErr)
		body["admissions_unread"] = true
	}
	if found {
		body["operation"] = operationWith(op, state.Acks, state.AcksErr)
		// WHAT IS MISSING, computed here: an operator runs this to learn
		// why the window hasn't moved. WHICH LIST IS THE PHASE'S: at
		// `opened` the next step is the baseline (a restart into maintenance
		// from every participant, and the admissions), not the seal's list,
		// which would name every node as owing a seal restart. Past it the
		// seal is next and no step reads an admission again. ONCE SEALED,
		// THE SEAL IS A FACT OF THE PHASE: verify and release wait on no
		// node, and re-applying the seal's rule to later writes read "not
		// sealed" under PHASE sealed.
		switch op.Phase {
		case coord.PhaseOpened:
			if acksRead && judged {
				body["restarts_missing"] = statelog.BaselineMissing(op, state.Acks)
			}
			if admissionsRead && judged {
				body["admissions_blocking"] = statelog.AdmissionBlocks(state.Admissions,
					op.Waived())
			}
		case coord.PhaseSealed, coord.PhaseConfirmed:
			body["sealed"] = true
		default:
			if acksRead && judged {
				held, missing := statelog.SealHolds(op, state.Acks)
				body["sealed"] = held
				body["blocking"] = missing
			}
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// serveAbandon answers POST /worklet/retention/maintenance/abandon.
//
// THE CONFIRMATION IS CHECKED BEFORE ANYTHING IS ABANDONED: ?confirm= repeats
// the open operation's id, which says the caller read which window they are
// abandoning. Missing, it is refused here; naming another operation, the
// engine refuses it against the operation it reads before changing anything
// ([engine.ConfirmMismatchError]). It used to be compared by the CLI after the
// abandonment had run.
func (a *App) serveAbandon(w http.ResponseWriter, r *http.Request) {
	// THE LOG FIRST, so the refusal of a missing confirmation can name the
	// status read as the CLI takes it, which needs the log.
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeStreamRequired)
		return
	}
	confirm := r.URL.Query().Get("confirm")
	if confirm == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": fmt.Sprintf("repeat the open operation's id in ?confirm=, "+
				"which `crewlet retention maintenance status -stream %s` shows: a "+
				"window is abandoned knowing which one it is", stream),
		})
		return
	}
	a.capacityGesture(w, r, "abandon", "the abandonment", func(ctx context.Context, stream string,
		by iam.Actor) (coord.MaintenanceOperation, error) {
		return a.capacity.AbandonCapacity(ctx, stream, confirm, by)
	})
}

// serveExclude answers POST /worklet/retention/maintenance/exclude.
func (a *App) serveExclude(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if node == "" || r.URL.Query().Get("confirm") != node {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "repeat the node id in ?confirm=. Excluding a participant " +
				"asserts that its process is stopped and holds no outstanding " +
				"request, which is the only thing that waives its acknowledgement",
		})
		return
	}
	a.capacityGesture(w, r, "exclude", "the exclusion", func(ctx context.Context, stream string,
		by iam.Actor) (coord.MaintenanceOperation, error) {
		return a.capacity.ExcludeParticipant(ctx, stream, node, by)
	})
}

// serveRetire answers POST /worklet/retention/maintenance/retire: an
// operator's assertion that ?node= will never run again, so no capacity window
// counts it ([engine.Engine.RetireNode]). Fleet-wide, so no log is named; the
// node id is repeated in ?confirm=.
func (a *App) serveRetire(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if node == "" || r.URL.Query().Get("confirm") != node {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "repeat the node id in ?confirm=. Retiring a node asserts that no process will " +
				"run under its id again, so no capacity window counts it",
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	retired, err := a.capacity.RetireNode(r.Context(), node, by)
	if err != nil {
		log.Warn("api_node_retire_refused", "node", node, "by", by.Name, "operator", by.OperatorID,
			"error", err)
		if said, told := statelog.Tell(err); told {
			httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeCapacityRefused,
				httpjson.Detail{"detail": said})
			return
		}
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeCapacityFailed,
			httpjson.Detail{"detail": fmt.Sprintf("retiring %s stopped on an error, and this node's "+
				"log says which; nothing is retired unless `crewlet retention maintenance retire` "+
				"succeeds", node)})
		return
	}
	log.Info("node_retire", "node", node, "by", by.Name, "operator", by.OperatorID)
	writeJSON(w, http.StatusOK, map[string]any{"retired": retired})
}

// capacityGesture is the shared shape of the two operator writes, each handed
// who is making it so the operation can record them. Gesture is the verb the
// log lines carry and what is the gesture as a reader is told it ("the
// abandonment"): a verb used as a noun read "the abandon on X stopped".
func (a *App) capacityGesture(w http.ResponseWriter, r *http.Request, gesture, what string,
	run func(context.Context, string, iam.Actor) (coord.MaintenanceOperation, error)) {

	stream := r.URL.Query().Get("stream")
	if stream == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeStreamRequired)
		return
	}
	// WHO IS ACTING, resolved BEFORE the gesture runs, as every other
	// write on this surface does. It was resolved after — so an abandon or
	// an exclusion took effect and only then was asked who had made it,
	// and a caller the node could not resolve changed the capacity window
	// and was answered 503 as though nothing had happened.
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	op, err := run(r.Context(), stream, by)
	var mismatch *engine.ConfirmMismatchError
	if errors.As(err, &mismatch) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, httpjson.Detail{
			"detail": fmt.Sprintf("the operation open on %s is %s, not %s, so "+
				"nothing was abandoned: read `crewlet retention maintenance "+
				"status -stream %s`, then repeat its id in ?confirm=",
				mismatch.Stream, mismatch.Open, mismatch.Named, mismatch.Stream),
			"operation": a.operationOrNil(r.Context(), op),
		})
		return
	}
	if err != nil {
		log.Warn("api_capacity_gesture_refused", "gesture", gesture,
			"stream", stream, "by", by.Name, "operator", by.OperatorID,
			"error", err)
		a.capacityRefused(w, r, stream, what, err, op)
		return
	}
	log.Info("capacity_gesture", "by", by.Name, "operator", by.OperatorID,
		"gesture", gesture, "stream", stream)
	body := map[string]any{"operation": a.operationOrNil(r.Context(), op)}
	a.windowsLeft(r.Context(), body)
	writeJSON(w, http.StatusOK, body)
}

// capacityRefused answers a capacity gesture that did not go through: a stream
// this node does not run is `404 unknown_stream`, a refusal the engine told
// ([statelog.Told]) is `409 capacity_refused` in its words, and anything else
// is a fault, `500 capacity_failed`, told here and kept in the log the caller
// has already written. Each carries the operation where there is one, since
// "never opened" and "open and stuck" have opposite next steps.
func (a *App) capacityRefused(w http.ResponseWriter, r *http.Request, stream, what string,
	err error, op coord.MaintenanceOperation) {

	if errors.Is(err, engine.ErrUnknownStream) {
		unknownStream(w, stream, err)
		return
	}
	if said, ok := statelog.Tell(err); ok {
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeCapacityRefused,
			httpjson.Detail{"detail": said, "operation": a.operationOrNil(r.Context(), op)})
		return
	}
	httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeCapacityFailed,
		httpjson.Detail{"detail": fmt.Sprintf("%s on %s stopped on an error, and "+
			"this node's log says which. Read where it stands with `crewlet "+
			"retention maintenance status -stream %s` before going on", what,
			stream, stream), "operation": a.operationOrNil(r.Context(), op)})
}

// operationOrNil renders an operation, or nil where there is none — so a
// reader can tell "no window" from "a window in phase opened", which are
// different facts with different next steps.
//
// A BLOCKED WINDOW'S SENTENCE READS ITS ACKNOWLEDGEMENTS: what closes an
// abandoned window is its seal's to say ([statelog.BlockedDetail]), so they are
// read here for a blocked one, and their failure to read is the sentence's to
// say. An unblocked one reads nothing more.
func (a *App) operationOrNil(ctx context.Context, op coord.MaintenanceOperation) any {
	var (
		acks []coord.MaintenanceAck
		err  error
	)
	if op.OperationID != "" && op.Blocked != "" {
		acks, err = a.capacity.CapacityAcks(ctx, op.Stream)
		if err != nil {
			log.Warn("api_maintenance_part_unreadable", "stream", op.Stream,
				"part", "acknowledgements", "error", err)
		}
	}
	return operationWith(op, acks, err)
}

// operationWith renders an operation with the acknowledgements its block's
// sentence is decided by, or nil where there is none.
func operationWith(op coord.MaintenanceOperation, acks []coord.MaintenanceAck,
	acksErr error) any {

	if op.OperationID == "" {
		return nil
	}
	return operationAnswer{MaintenanceOperation: op, Retired: op.Retired,
		RetiredUnread: op.RetiredUnjudged, BlockedDetail: statelog.BlockedDetail(op, acks, acksErr)}
}

// operationAnswer is an operation as every answer here carries it: the record,
// whose `blocked` is a code for scripts, and `blocked_detail`, the sentence a
// person reads for it ([statelog.BlockedDetail]), so no client has to turn a
// code into words of its own or print the code.
type operationAnswer struct {
	coord.MaintenanceOperation
	// Retired is the participants whose retirement stands as this node read
	// the window, never stored ([coord.MaintenanceOperation.Retired]), and
	// RetiredUnread that it couldn't judge them: Retired is then no answer.
	Retired []string `json:"retired,omitempty"`
	//wiregate:absent-is-false only an answer that couldn't judge retirements is marked
	RetiredUnread bool   `json:"retired_unread,omitempty"`
	BlockedDetail string `json:"blocked_detail,omitempty"`
}
