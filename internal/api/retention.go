package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
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
// The anonymous-read posture a laptop deployment allows must never reach any
// of them. An acknowledgement moves the floor the trim deletes against, an
// eviction stops a machine writing, and a readmission lets it write again —
// none is a read, whatever the posture says.

// retentionWriter is the slice of the fleet these routes need.
//
// Declared here, by the consumer, and deliberately not [coord.Fleet]: a route
// that could also reach the activation pointer or the secret store would
// eventually be given a reason to.
type retentionWriter interface {
	PutBackupPoint(ctx context.Context, p coord.BackupPoint) error
}

// NodeGate is the eviction half, which is a RECORD on the log rather than a
// coordination write — one on every log whose applier installs the eviction
// gate, each answering with the same three-valued outcome every other write
// has.
//
// EXPORTED, unlike its neighbour, because the caller has to convert a node
// that runs no state log into a genuine nil before handing it over: a seam
// that answered "no log" on every press would be registered and broken.
type NodeGate interface {
	EvictNode(ctx context.Context, req engine.GateRequest) ([]engine.GateResult, error)
	ReadmitNode(ctx context.Context, req engine.GateRequest) ([]engine.GateResult, error)
}

// TaskPurger is the purge half, and it is a SEPARATE seam rather than a third
// method on [NodeGate].
//
// A gate is a decision about a machine and is recorded on every gated log; a
// purge destroys one task's rows on one log. Keeping it apart is also what lets
// this route be exercised without a broker: the alternative signature hands
// back a concrete `*tracker.Writer`, which no test can supply without a
// publisher, a store and a log.
type TaskPurger interface {
	// PurgeAs destroys a task as the named operator, reporting the same
	// three-valued outcome every other write has.
	PurgeAs(ctx context.Context, operator, opID, id, project, reason string) (tracker.WriteResult, error)
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
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "position_required",
			"detail": "name the stream and the sequence your copy reaches — " +
				"an acknowledgement moves the floor the trim deletes against, " +
				"so there is no value to guess",
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
		// 404 AND NOT 503, which is how both reanchor routes classify the
		// same refusal: all three reach [engine.ErrNotADomainLog] through
		// the engine's one lookup of a stream's running domain — this
		// route by way of StreamGeneration, those by way of ReanchorStatus
		// and Reanchor. Nothing here is transient, and a 503 tells an
		// operator who typed the stream name wrong to wait and try the
		// identical request again.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":  "unknown_stream",
			"detail": err.Error(),
		})
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
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
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "ack_failed"})
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
// too, and neither record carries a stream name at all. So there is no lookup
// to perform in them: a reanchor moves ONE domain's generation, and on an
// estate where the tracker has been re-anchored and the pages log has not, the
// two numbers differ.
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
// one, because the acknowledgement echoes back the generation it recorded. A
// stream this build does not run has no such cursor and is refused.
func (a *App) generationOf(stream string) (uint32, error) {
	return a.capacity.StreamGeneration(stream)
}

// mountRetention registers the write half of the retention surface.
func (a *App) mountRetention(mux *http.ServeMux) {
	mux.Handle("POST /work/retention/ack", http.HandlerFunc(a.serveRetentionAck))
	mux.Handle("POST /work/retention/evict/{node}", a.gate(true))
	mux.Handle("POST /work/retention/readmit/{node}", a.gate(false))
	// THE PURGE, and it lives beside the eviction because they are the
	// two gestures on this engine that DESTROY rather than change: one
	// stops a machine's records applying, the other removes a task and
	// every row it produced. Both are guarded, both echo their subject
	// back as a confirmation, and both answer the three-valued write
	// outcome whole.
	if a.purger != nil {
		mux.Handle("POST /work/{id}/purge", http.HandlerFunc(a.servePurge))
	}
}

// servePurge answers POST /work/{id}/purge.
//
// # Why this route exists at all
//
// `tracker.Writer.PurgeTask` is the one operation in this engine with no
// inverse, restricted in the write path to a person or an operator token — and
// nothing anywhere called it. No CLI verb, no route, no tool. So a company
// could not destroy a task under any circumstances: an erasure request had no
// mechanism, and a credential pasted into a task body stayed in the durable
// rows of every node for ever, where `remove` only hides it and `delete` only
// stops later records about it.
//
// # The confirmation is the task's KEY, not its id
//
// An id is a uuid nobody reads; the key is what a person sees on the board and
// in the ticket they were asked to act on. Echoing the id back would be a
// value copied from the same URL that carries it, which confirms nothing —
// where the key has to be looked up, which is the point of asking.
func (a *App) servePurge(w http.ResponseWriter, r *http.Request) {
	if a.purger == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "no_tracker"})
		return
	}
	id := r.PathValue("id")
	key := strings.TrimSpace(r.URL.Query().Get("confirm"))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	switch {
	case id == "" || key == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "repeat the task's KEY in ?confirm= — a purge removes the " +
				"task and every row it produced on every node, and there is " +
				"nothing that undoes it",
		})
		return
	case project == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "project_required",
			"detail": "name the task's project in ?project= — it is the " +
				"container the record arbitrates under, and a purge filed " +
				"under the wrong one blocks writes to a project it is not about",
		})
		return
	case reason == "":
		// A REASON IS REQUIRED, unlike every other gesture here: the rows
		// are destroyed, so the reason is the only account of why. It is
		// logged below beside the task's key, and when the project has a
		// lead it travels whole on the notification the lead receives
		// (tracker.purgeExcerpt).
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reason_required",
			"detail": "state why in ?reason= — the rows are destroyed and the " +
				"reason is the only account of why",
		})
		return
	}
	operator, ok := auth.OperatorFrom(r.Context())
	if !ok || operator == "" {
		// THE GUARD ALREADY REFUSED AN UNAUTHENTICATED CALLER, so this
		// is the build with no operator identity on the context at all.
		// The write path refuses it too, and refusing here names the
		// reason rather than surfacing the writer's own.
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "operator_required",
			"detail": "a purge is an operator gesture and this request carries " +
				"no operator identity",
		})
		return
	}

	// THE CALLER MAY BRING ITS OWN OPERATION ID, and this is the one
	// route where that matters. A purge that answers `unknown` has no
	// acknowledgement and no position — it may have landed and it may
	// not — so the only correct response is to retry, and a retry with a
	// FRESH id would append a second purge of a task the first one may
	// already have destroyed. A fresh id per press is right for the
	// eviction beside this (two operators evicting one node are two
	// decisions worth recording); it is wrong here.
	opID := strings.TrimSpace(r.URL.Query().Get("op_id"))
	if opID == "" {
		opID = uuid.NewString()
	}
	result, err := a.purger.PurgeAs(r.Context(), operator, opID, id, project, reason)
	var tooLong *tracker.ErrPurgeReasonTooLong
	switch {
	case errors.As(err, &tooLong):
		// THE CALLER'S TO FIX, so a 400 and not a server fault: nothing
		// was purged, and the same request with a shorter reason
		// succeeds. The reason is refused rather than cut because it
		// travels whole or not at all — see tracker.purgeReasonRoom — and
		// `room` is how many bytes fit, which differs from one task to the
		// next with the length of its key.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "reason_too_long",
			"detail": err.Error(),
			"bytes":  tooLong.Bytes,
			"room":   tooLong.Room,
		})
		return
	case err != nil:
		log.Warn("api_purge_failed", "task", id, "operator", operator,
			"error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "purge_failed", "detail": err.Error()})
		return
	}
	// LOGGED AT INFO WITH THE REASON, because this is the one gesture
	// whose subject no longer exists to be inspected afterwards.
	log.Info("task_purged", "task", id, "key", key, "project", project,
		"operator", operator, "reason", reason, "outcome", result.Outcome)
	writeJSON(w, http.StatusOK, map[string]any{
		"task": id, "key": key, "project": project,
		"outcome":  result.Outcome,
		"position": result.Position,
		"op_id":    result.OpID,
	})
}

// gate answers the eviction and readmission routes.
//
// ONE HANDLER FOR BOTH, because they are one gesture with a sign: a
// readmission is the INVERSE COMMIT rather than a delete, written onto the same
// subjects, so two handlers would be two copies of one refusal vocabulary.
func (a *App) gate(evict bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.nodes == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": "no_state_log"})
			return
		}
		node := r.PathValue("node")
		// THE CONFIRMATION ECHOES THE NODE ID, the same shape the
		// destructive CLI gestures already use: an eviction stops a
		// machine writing and a readmission lets it write again, and
		// neither is a value to get from a shell history.
		if node == "" || r.URL.Query().Get("confirm") != node {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "confirm_required",
				"detail": "repeat the node id in ?confirm= — an eviction stops " +
					"that machine's records applying anywhere in the fleet",
			})
			return
		}
		operator, ok := auth.OperatorFrom(r.Context())
		if !ok || operator == "" {
			// THE GUARD ALREADY REFUSED AN UNAUTHENTICATED CALLER, so
			// this is a build with no operator identity on the context.
			// Every log's record names who ran the gesture, and one
			// naming nobody is a removal nobody can account for.
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "operator_required",
				"detail": "an eviction is an operator gesture and this request " +
					"carries no operator identity",
			})
			return
		}
		req := engine.GateRequest{
			Node: node, By: operator,
			// A FRESH ID PER PRESS, shared by every log's record: two
			// operators evicting one node are two decisions and each
			// ledger records both, while one press is one decision an
			// audit can follow across the logs.
			OpID:  uuid.NewString(),
			Force: evict && r.URL.Query().Get("force") == "true",
		}
		var results []engine.GateResult
		var err error
		if evict {
			results, err = a.nodes.EvictNode(r.Context(), req)
		} else {
			results, err = a.nodes.ReadmitNode(r.Context(), req)
		}
		body := map[string]any{
			"node": node, "evicted": evict, "op_id": req.OpID,
			// THE THREE-VALUED OUTCOME, whole and per log. A gate the
			// caller believes landed and which is only `pending` is the
			// difference between a node that has stopped writing and
			// one that is about to — and a gesture that reached one log
			// and not the next is two different facts.
			"domains": gateDomains(results),
		}
		var refused *statelog.EvictionRefusal
		switch {
		case errors.As(err, &refused):
			log.Warn("api_retention_gate_refused", "node", node, "error", err)
			body["error"], body["detail"] = "eviction_refused", err.Error()
			writeJSON(w, http.StatusConflict, body)
			return
		case err != nil:
			log.Warn("api_retention_gate_failed", "node", node,
				"evict", evict, "error", err)
			body["error"], body["detail"] = "gate_failed", err.Error()
			writeJSON(w, http.StatusInternalServerError, body)
			return
		}
		log.Info("retention_gate", "operator", operator, "node", node, "evict", evict)
		writeJSON(w, http.StatusOK, body)
	}
}

// gateDomains renders each log's answer to a gate: the domain, its stream,
// the outcome and where its record landed.
func gateDomains(results []engine.GateResult) []map[string]any {
	out := make([]map[string]any, 0, len(results))
	for _, res := range results {
		out = append(out, map[string]any{
			"domain": res.Domain, "stream": res.Stream,
			"outcome": res.Outcome, "position": res.Position,
		})
	}
	return out
}

// capacityRunner is the slice of the engine the capacity routes need.
//
// Declared here, by the consumer: a route that could also reach the seat host
// or the config surface would eventually be given a reason to.
type capacityRunner interface {
	Reanchor(ctx context.Context, req engine.ReanchorRequest) (uint32, error)
	ReanchorStatus(ctx context.Context, stream string) (time.Time, uint32, error)
	StreamGeneration(stream string) (uint32, error)
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
func (a *App) mountCapacity(mux *http.ServeMux) {
	mux.Handle("POST /work/retention/capacity", http.HandlerFunc(a.serveSetCapacity))
	mux.Handle("GET /work/retention/maintenance", http.HandlerFunc(a.serveMaintenanceStatus))
	mux.Handle("POST /work/retention/maintenance/abandon", http.HandlerFunc(a.serveAbandon))
	mux.Handle("POST /work/retention/maintenance/exclude", http.HandlerFunc(a.serveExclude))
	mux.Handle("GET /work/retention/reanchor", http.HandlerFunc(a.serveReanchorStatus))
	mux.Handle("POST /work/retention/reanchor", http.HandlerFunc(a.serveReanchor))
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
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "stream_required"})
		return
	}
	createdAt, generation, err := a.capacity.ReanchorStatus(r.Context(), stream)
	switch {
	case errors.Is(err, engine.ErrNotADomainLog):
		writeJSON(w, http.StatusNotFound,
			map[string]string{"error": "unknown_stream", "detail": err.Error()})
		return
	case err != nil:
		// THE BROKER DID NOT ANSWER, which is worth coming back for: the
		// instant to confirm is the live stream's, and nothing else can
		// stand in for it.
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "stream_unreadable", "detail": err.Error()})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "echo the stream's own created_at from GET " +
				"/work/retention/reanchor — a reanchor declares every position " +
				"below the new generation stale, and the confirmation is what " +
				"says you looked at the stream you are re-anchoring",
		})
		return
	}
	operator, ok := auth.OperatorFrom(r.Context())
	if !ok || operator == "" {
		// THE GATE'S OWN REFUSAL, for its reason: the generation record
		// and the audit row both name who re-anchored, and a transition
		// naming nobody is one nobody can account for.
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "operator_required",
			"detail": "a reanchor is an operator gesture and this request " +
				"carries no operator identity",
		})
		return
	}
	gen, err := a.capacity.Reanchor(r.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: confirm, By: operator,
		Force: r.URL.Query().Get("force") == "true",
	})
	// THREE ANSWERS, because they send an operator three different ways: a
	// stream no domain runs on is a name to correct; a refusal is the
	// transition declining, having written nothing, with its reason; and
	// anything else is a failure, which the transition's own step order
	// makes safe to run again ([statelog.Reanchor]).
	//
	// A REFUSAL IS NOT "NEVER". Most clear, and this same call then lands:
	// a transition already running on this node, once it ends; a live
	// instant the broker would not give, once it answers; an unreadable
	// positions register, once it reads or with force; a confirmation
	// naming another instant, sent again with the live one; a node that is
	// not the most caught-up, with force. Two do not clear by calling again
	// — a peer hydrated on the live stream, and a generation another
	// reanchor's record holds — and each names what to adopt instead. The
	// detail carries which, in the engine's own words; the code says only
	// that nothing was written.
	switch {
	case errors.Is(err, engine.ErrNotADomainLog):
		writeJSON(w, http.StatusNotFound,
			map[string]string{"error": "unknown_stream", "detail": err.Error()})
		return
	case errors.Is(err, statelog.ErrReanchorRefused):
		log.Warn("api_reanchor_refused", "stream", stream, "error", err)
		writeJSON(w, http.StatusConflict,
			map[string]string{"error": "reanchor_refused", "detail": err.Error()})
		return
	case err != nil:
		log.Warn("api_reanchor_failed", "stream", stream, "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "reanchor_failed", "detail": err.Error()})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "target_required",
			"detail": "name the stream and the byte ceiling — a target is chosen " +
				"once for the life of an operation and never changed, so there " +
				"is no value to guess",
		})
		return
	}
	if confirm := r.URL.Query().Get("confirm"); confirm != strconv.FormatUint(target, 10) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "repeat the byte count in ?confirm= — this restarts the " +
				"whole fleet three times and changes what the broker will accept",
		})
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
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
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "capacity_refused", "detail": err.Error(),
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
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "stream_required"})
		return
	}
	op, acks, admissions, found, err := a.capacity.CapacityStatus(r.Context(), stream)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "maintenance_unreadable", "detail": err.Error()})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
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
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "stream_required"})
		return
	}
	op, err := run(r.Context(), stream)
	if err != nil {
		log.Warn("api_capacity_gesture_refused", "gesture", what,
			"stream", stream, "error", err)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "capacity_refused", "detail": err.Error(),
			"operation": operationOrNil(op),
		})
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
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
