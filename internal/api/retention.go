package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE THREE RETENTION GESTURES THAT WRITE, over HTTP for one reason.
//
// Every one of them changes FLEET state — the positions register's own
// records, which live in the coordination store. On the default topology that
// store is the engine's own embedded broker, which binds no socket: a CLI run
// while the engine is down cannot reach it, and one run while the engine is UP
// must not try, because a second server on the same store directory is
// accepted rather than refused and two writers on one JetStream store is
// corruption rather than contention.
//
// So each gesture goes where the state is reachable: through a node that
// already holds it. `crewlet retention ack`, `evict` and `readmit` are clients
// of these routes, exactly as `crewlet budgets reset` and `crewlet backup` are
// of theirs.
//
// # Why they are POSTs even though one of them only publishes a number
//
// An acknowledgement moves the floor the trim deletes against, an eviction
// stops a machine writing, and a readmission lets it write again — none is a
// read, and nothing about one should be retried by a proxy or prefetched by a
// browser the way a GET may be.
//
// # Who may make them
//
// `fleet:operate`, decided by the authority table where [App.mountDeployment]
// mounts them — every route in this file, the two reads included. The handlers
// resolve the caller for the audit line and decide nothing themselves.

// retentionWriter is the slice of the fleet these routes need.
//
// Declared here, by the consumer, and deliberately not [coord.Fleet]: a route
// that could also reach the activation pointer or the secret store would
// eventually be given a reason to.
type retentionWriter interface {
	PutBackupPoint(ctx context.Context, p coord.BackupPoint) error
}

// NodeGate is the eviction half, which is a RECORD on the log rather than a
// coordination write — so it goes through the same writer a seat's tools do,
// and its outcome is the same three-valued answer every other write has.
//
// EXPORTED, unlike its neighbour, because the caller has to convert a typed
// nil to a genuine one before handing it over: a nil *tracker.Writer inside a
// non-nil interface passes this route's own check and panics on the first
// press.
type NodeGate interface {
	EvictNode(ctx context.Context, opID, nodeID string) (tracker.WriteResult, error)
	ReadmitNode(ctx context.Context, opID, nodeID string) (tracker.WriteResult, error)
}

// serveRetentionAck answers POST /work/retention/ack.
//
// It publishes an OPERATOR backup floor: the assertion that a copy has reached
// wherever the company's policy says it must, which the engine cannot see for
// itself — a backup is not a backup until it leaves the host.
//
// `?position=` is the sequence, and it is per STREAM because a position's
// number space belongs to the stream it came from: `?stream=` names which. A
// call naming neither is refused rather than guessed at, because guessing here
// moves the floor the trim deletes against.
func (a *App) serveRetentionAck(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	position, err := strconv.ParseUint(r.URL.Query().Get("position"), 10, 64)
	if stream == "" || err != nil || position == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodePositionRequired, map[string]string{
			"detail": "name the stream and the sequence your copy reaches — " +
				"an acknowledgement moves the floor the trim deletes against, " +
				"so there is no value to guess",
		})
		return
	}
	generation, err := a.generationOf(r.Context(), stream)
	if err != nil {
		// A BARE SEQUENCE NAMES A NUMBER SPACE. Publishing one at the
		// wrong generation pins a position on a log that no longer
		// exists, which the trim reads as no acknowledgement at all —
		// so a stream this node does not run is refused rather than
		// acknowledged at some other log's generation.
		//
		// 404 AND NOT 503, which is the classification GET
		// /work/retention/reanchor already gives the same refusal from
		// the same call: nothing here is transient, and a 503 tells an
		// operator who typed the stream name wrong to wait and try the
		// identical request again.
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownStream, map[string]string{
			"detail": err.Error(),
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	operator := auth.OperatorID(caller)
	point := coord.BackupPoint{
		Owner: coord.OperatorBackupOwner,
		At:    time.Now().UTC(),
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
	log.Info("retention_acknowledged", "operator", operator,
		"stream", stream, "position", position, "generation", generation)
	writeJSON(w, http.StatusOK, map[string]any{
		"stream": stream, "position": position, "generation": generation,
	})
}

// generationOf is the generation the NAMED stream's log stands at, taken from
// the domain this node is running rather than from the fleet's registers.
//
// # The question is per stream, and neither register can be asked per stream
//
// An acknowledgement is filed under a STREAM — [coord.BackupPoint.Streams] is
// what the trim's backup term looks its reach up in — while the published
// floors are keyed by DOMAIN and every node's positions heartbeat by domain
// too, and neither record carries a stream name at all. So there was no lookup
// to perform in them, and what the two sources here actually did was take the
// FIRST row of whichever one answered. That is not "the fleet's generation":
// a reanchor moves ONE domain's, so on an estate where the tracker has been
// re-anchored and the pages log has not, the two numbers differ and which one
// came back was whichever key the store happened to list first.
//
// # And a generation that is too HIGH is not a refused acknowledgement
//
// The trim discards a backup point BELOW its own generation and reads one at
// or above it as covering the log (see [statelog.TrimInputs]'s backup term).
// So a point stamped with a re-anchored neighbour's higher generation counts —
// at a sequence belonging to a number space that is not this log's — and the
// trim then deletes records the acknowledged copy does not contain. The one
// failure this whole route exists to prevent.
//
// # Why the running domain is the right source
//
// It is the SAME number the trim compares against: both read that stream's
// applier committed cursor. A node that has not yet applied a reanchor answers
// LOW, which the trim's own guard refuses — the safe direction, and a visible
// one, because the acknowledgement echoes back the generation it recorded.
//
// A stream this build does not run has no such cursor and is REFUSED, where
// the register sources answered it confidently with some other log's number:
// a mistyped stream name used to read back as a successful acknowledgement
// that nothing would ever count.
func (a *App) generationOf(ctx context.Context, stream string) (uint32, error) {
	// The instant is the reanchor confirmation's business and not this
	// route's; the generation beside it is this node's own answer for the
	// stream, which is exactly what has to be stamped on the point.
	_, generation, err := a.capacity.ReanchorStatus(ctx, stream)
	if err != nil {
		return 0, err
	}
	return generation, nil
}

// mountRetention registers the write half of the retention surface, through
// the mount [App.mountDeployment] hands it — which is what decides the grant.
func (a *App) mountRetention(mount func(string, http.HandlerFunc)) {
	mount("POST /work/retention/ack", a.serveRetentionAck)
	mount("POST /work/retention/evict/{node}", a.gate(true))
	mount("POST /work/retention/readmit/{node}", a.gate(false))
}

// gate answers the eviction and readmission routes.
//
// ONE HANDLER FOR BOTH, because they are one gesture with a sign: a
// readmission is the INVERSE COMMIT rather than a delete, written by the same
// writer onto the same subject, so two handlers would be two copies of one
// refusal vocabulary.
func (a *App) gate(evict bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.nodes == nil {
			httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeNoTracker)
			return
		}
		node := r.PathValue("node")
		// THE CONFIRMATION ECHOES THE NODE ID, the same shape the
		// destructive CLI gestures already use: an eviction stops a
		// machine writing and a readmission lets it write again, and
		// neither is a value to get from a shell history.
		if node == "" || r.URL.Query().Get("confirm") != node {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
				"detail": "repeat the node id in ?confirm= — an eviction stops " +
					"that machine's records applying anywhere in the fleet",
			})
			return
		}
		caller, ok := auth.Caller(w, r)
		if !ok {
			return
		}
		operator := auth.OperatorID(caller)
		// A FRESH ID PER PRESS, unlike the chart apply's derived one:
		// every node computes the chart's id from the revision so the
		// losers collapse, but two operators evicting one node are two
		// decisions and the ledger should record both.
		opID := uuid.NewString()
		var result tracker.WriteResult
		var err error
		if evict {
			result, err = a.nodes.EvictNode(r.Context(), opID, node)
		} else {
			result, err = a.nodes.ReadmitNode(r.Context(), opID, node)
		}
		if err != nil {
			log.Warn("api_retention_gate_failed", "node", node,
				"evict", evict, "error", err)
			httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeGateFailed,
				map[string]string{"detail": err.Error()})
			return
		}
		log.Info("retention_gate", "operator", operator, "node", node, "evict", evict)
		writeJSON(w, http.StatusOK, map[string]any{
			"node": node, "evicted": evict,
			// THE THREE-VALUED OUTCOME, whole. A gate the caller
			// believes landed and which is only `pending` is the
			// difference between a node that has stopped writing and
			// one that is about to.
			"outcome":  result.Outcome,
			"position": result.Position,
			"op_id":    result.OpID,
		})
	}
}

// capacityRunner is the slice of the engine the capacity routes need.
//
// Declared here, by the consumer: a route that could also reach the seat host
// or the config surface would eventually be given a reason to.
type capacityRunner interface {
	Reanchor(ctx context.Context, req engine.ReanchorRequest) (uint32, error)
	ReanchorStatus(ctx context.Context, stream string) (time.Time, uint32, error)
	SetCapacity(ctx context.Context, req engine.CapacityRequest) (coord.MaintenanceOperation, error)
	AbandonCapacity(ctx context.Context, stream string) (coord.MaintenanceOperation, error)
	ExcludeParticipant(ctx context.Context, stream, node string) (coord.MaintenanceOperation, error)
	CapacityStatus(ctx context.Context, stream string) (
		coord.MaintenanceOperation, []coord.MaintenanceAck, []coord.Admission, bool, error)
	Mode() statelog.MaintenanceMode
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
func (a *App) mountCapacity(mount func(string, http.HandlerFunc)) {
	mount("POST /work/retention/capacity", a.serveSetCapacity)
	mount("GET /work/retention/maintenance", a.serveMaintenanceStatus)
	mount("POST /work/retention/maintenance/abandon", a.serveAbandon)
	mount("POST /work/retention/maintenance/exclude", a.serveExclude)
	mount("GET /work/retention/reanchor", a.serveReanchorStatus)
	mount("POST /work/retention/reanchor", a.serveReanchor)
}

// serveReanchorStatus answers GET /work/retention/reanchor: the stream's own
// creation instant, which is the value the confirmation has to echo.
//
// A SEPARATE READ, because the confirmation is meant to say "I looked at the
// thing I am re-anchoring": a verb that printed the value and accepted it back
// in one call would be confirming against its own output.
func (a *App) serveReanchorStatus(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeStreamRequired)
		return
	}
	createdAt, generation, err := a.capacity.ReanchorStatus(r.Context(), stream)
	if err != nil {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownStream,
			map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stream": stream, "created_at": createdAt, "generation": generation,
	})
}

// serveReanchor answers POST /work/retention/reanchor.
func (a *App) serveReanchor(w http.ResponseWriter, r *http.Request) {
	stream, confirm := r.URL.Query().Get("stream"), r.URL.Query().Get("confirm")
	if stream == "" || confirm == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "echo the stream's own created_at from GET " +
				"/work/retention/reanchor — a reanchor declares every position " +
				"below the new generation stale, and the confirmation is what " +
				"says you looked at the stream you are re-anchoring",
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	operator := auth.OperatorID(caller)
	gen, err := a.capacity.Reanchor(r.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: confirm, By: operator,
		Force: r.URL.Query().Get("force") == "true",
	})
	if err != nil {
		log.Warn("api_reanchor_refused", "stream", stream, "error", err)
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeReanchorRefused,
			map[string]string{"detail": err.Error()})
		return
	}
	log.Warn("reanchored", "operator", operator, "stream", stream, "generation", gen)
	writeJSON(w, http.StatusOK, map[string]any{
		"stream": stream, "generation": gen,
	})
}

// serveSetCapacity answers POST /work/retention/capacity.
func (a *App) serveSetCapacity(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	target, err := strconv.ParseUint(r.URL.Query().Get("bytes"), 10, 64)
	if stream == "" || err != nil || target == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeTargetRequired, map[string]string{
			"detail": "name the stream and the byte ceiling — a target is chosen " +
				"once for the life of an operation and never changed, so there " +
				"is no value to guess",
		})
		return
	}
	if confirm := r.URL.Query().Get("confirm"); confirm != strconv.FormatUint(target, 10) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "repeat the byte count in ?confirm= — this restarts the " +
				"whole fleet three times and changes what the broker will accept",
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	operator := auth.OperatorID(caller)
	op, err := a.capacity.SetCapacity(r.Context(), engine.CapacityRequest{
		Stream: stream, TargetMaxBytes: target, By: operator,
		Assert: r.URL.Query().Get("assert_excluded") == "true",
	})
	if err != nil {
		// THE OPERATION IS RETURNED WITH THE REFUSAL where there is one:
		// a caller told only that something failed cannot tell an
		// operation that never opened from one that is open and stuck,
		// and those have opposite next steps.
		log.Warn("api_set_capacity_refused", "stream", stream, "error", err)
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeCapacityRefused, httpjson.Detail{
			"detail":    err.Error(),
			"operation": operationOrNil(op),
		})
		return
	}
	log.Info("capacity_requested", "operator", operator, "stream", stream,
		"target", target, "phase", op.Phase)
	writeJSON(w, http.StatusOK, map[string]any{
		"operation": operationOrNil(op), "mode": a.capacity.Mode(),
	})
}

// serveMaintenanceStatus answers GET /work/retention/maintenance.
func (a *App) serveMaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeStreamRequired)
		return
	}
	op, acks, admissions, found, err := a.capacity.CapacityStatus(r.Context(), stream)
	if err != nil {
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeMaintenanceUnreadable,
			map[string]string{"detail": err.Error()})
		return
	}
	body := map[string]any{
		"stream": stream, "open": found, "mode": a.capacity.Mode(),
		"acks": acks, "admissions": admissions,
	}
	if found {
		body["operation"] = op
		// WHAT IS MISSING, computed here rather than left to a reader:
		// the whole question an operator runs this for is why the seal
		// has not held, and a list of acknowledgements is that answer
		// only if you already know the rule.
		held, missing := statelog.SealHolds(op, acks)
		body["sealed"] = held
		body["blocking"] = missing
		body["admissions_blocking"] = statelog.AdmissionBlocks(admissions, op.Excluded)
	}
	writeJSON(w, http.StatusOK, body)
}

// serveAbandon answers POST /work/retention/maintenance/abandon.
func (a *App) serveAbandon(w http.ResponseWriter, r *http.Request) {
	a.capacityGesture(w, r, "abandon", func(ctx context.Context, stream string) (
		coord.MaintenanceOperation, error) {
		return a.capacity.AbandonCapacity(ctx, stream)
	})
}

// serveExclude answers POST /work/retention/maintenance/exclude.
func (a *App) serveExclude(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if node == "" || r.URL.Query().Get("confirm") != node {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConfirmRequired, map[string]string{
			"detail": "repeat the node id in ?confirm= — excluding a participant " +
				"asserts that its process is stopped and holds no outstanding " +
				"request, which is the only thing that waives its acknowledgement",
		})
		return
	}
	a.capacityGesture(w, r, "exclude", func(ctx context.Context, stream string) (
		coord.MaintenanceOperation, error) {
		return a.capacity.ExcludeParticipant(ctx, stream, node)
	})
}

// capacityGesture is the shared shape of the two operator writes.
func (a *App) capacityGesture(w http.ResponseWriter, r *http.Request, what string,
	run func(context.Context, string) (coord.MaintenanceOperation, error)) {

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
	operator := auth.OperatorID(caller)
	op, err := run(r.Context(), stream)
	if err != nil {
		log.Warn("api_capacity_gesture_refused", "gesture", what,
			"stream", stream, "operator", operator, "error", err)
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeCapacityRefused, httpjson.Detail{
			"detail":    err.Error(),
			"operation": operationOrNil(op),
		})
		return
	}
	log.Info("capacity_gesture", "operator", operator, "gesture", what, "stream", stream)
	writeJSON(w, http.StatusOK, map[string]any{"operation": operationOrNil(op)})
}

// operationOrNil renders an operation, or nil where there is none — so a
// reader can tell "no window" from "a window in phase opened", which are
// different facts with different next steps.
func operationOrNil(op coord.MaintenanceOperation) any {
	if op.OperationID == "" {
		return nil
	}
	return op
}
