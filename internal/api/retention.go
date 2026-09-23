package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	Evict(ctx context.Context, req engine.GateRequest) (engine.GateResult, error)

	// Readmit answers a [*statelog.ReadmissionRefusal], wrapped, for a node
	// below a trim floor it would be counted against — likewise a refusal.
	Readmit(ctx context.Context, req engine.GateRequest) (engine.GateResult, error)
}

// TaskPurger is the purge half, and it is a SEPARATE seam rather than a third
// method on [NodeGate].
//
// The gate is a decision about a MACHINE, written to every identity-claiming
// log from one judgement; a purge destroys one task's rows on one log, and has
// nothing to judge but the confirmation. Both records carry the PERSON who
// asked, so the identity is an argument on each rather than a property of the
// writer this process happens to hold. Keeping it apart is also what lets this
// route be exercised without a broker: the alternative signature hands back a
// concrete `*tracker.Writer`, which no test can supply without a publisher, a
// store and a log.
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
		// 404 AND NOT 503, which is the classification GET
		// /work/retention/reanchor already gives the same refusal from
		// the same call: nothing here is transient, and a 503 tells an
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
func (a *App) generationOf(stream string) (uint32, error) {
	// FROM MEMORY, never through the reanchor's status read: that one reads
	// the stream's creation instant LIVE from the broker, which this route
	// has no use for — and a broker that did not answer would then have
	// been reported as a stream name the operator mistyped.
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
		// A REASON IS REQUIRED, unlike every other gesture here. It is
		// the only thing that survives: the rows are gone, and the
		// deletion marker's reason is what a person reads a year later
		// when they ask what used to be at this key.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reason_required",
			"detail": "state why in ?reason= — the rows are destroyed and the " +
				"marker's reason is the only account of them that survives",
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

	// THE CALLER MAY BRING ITS OWN OPERATION ID. A purge that answers
	// `unknown` has no acknowledgement and no position — it may have
	// landed and it may not — so the only correct response is to retry,
	// and a retry with a FRESH id would append a second purge of a task the
	// first one may already have destroyed. The gate routes beside this
	// take one for the same reason.
	opID, ok := callerOpID(w, r, "purge-"+id)
	if !ok {
		return
	}
	result, err := a.purger.PurgeAs(r.Context(), operator, opID, id, project, reason)
	if err != nil {
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

// callerOpID is the operation id a caller brought in `?op_id=`, or a fresh one
// named name where it brought none.
//
// A retry is only a retry under the SAME id: a gesture that came back `unknown`
// or partial is finished by sending the id it answered with, and one sent with
// a fresh id is a second gesture.
//
// THE ID CARRIES THE INSTANT IT WAS MINTED, which is what the state log reads
// to decide whether its ledger can vouch for the retry — so the one minted here
// is [statelog.NewOpID]'s, and the id a caller brings back is the one the route
// answered with, instant and all.
//
// AN ID THE ENGINE DID NOT MINT IS REFUSED rather than passed on, and false is
// returned with the 400 already written. It carries no instant, so the state
// log reads it as older than every loss its ledger has had — an adopted
// snapshot, or a retention sweep that deleted anything, which every deployment
// older than the ledger's retention has had — and answers such a write
// `unknown` without publishing it, on the first attempt as on every retry.
// Passed on, it would be a gesture that can never run and never says why.
func callerOpID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	opID := strings.TrimSpace(r.URL.Query().Get("op_id"))
	if opID == "" {
		return statelog.NewOpID(time.Now(), name), true
	}
	if _, minted := statelog.OpMintedAt(opID); !minted {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "op_id_invalid",
			"detail": "?op_id= is for finishing a gesture that came back " +
				"`unknown` or partial, and takes back the op_id its answer " +
				"returned, unchanged — this one is not an id this engine " +
				"minted, so no node could tell whether it already ran. Omit " +
				"it to start the gesture afresh",
		})
		return "", false
	}
	return opID, true
}

// gateVerb names a gate gesture in the fresh operation id it is minted under.
func gateVerb(evict bool) string {
	if evict {
		return "evict"
	}
	return "readmit"
}

// gate answers the eviction and readmission routes.
//
// ONE HANDLER FOR BOTH, because they are one gesture with a sign: a
// readmission is the INVERSE COMMIT rather than a delete, written onto the same
// subjects of the same logs, so two handlers would be two copies of one refusal
// vocabulary.
//
// # One gesture, every identity-claiming log, and each log's own answer
//
// The engine judges the gesture once and then writes its record to every log
// the trim counts nodes on — see [engine.NodeGate]. A refusal is a 409 with
// nothing written anywhere: an eviction of a node still holding a live
// presence lease (unless `force=true`), a readmission of one below a trim
// floor it would be counted against, each carrying the numbers the refusal is
// about. Past the judgement the answer is 200 and PER LOG — each with its own
// three-valued outcome, or the refusal that stopped that log — and `complete`
// says whether every log now holds the record. An incomplete answer is not a
// failure to report as one: the logs that answered hold their record, and the
// same request sent again with the `op_id` it answered with writes only what
// is missing.
func (a *App) gate(evict bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.nodes == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "no_state_log",
				"detail": "this node runs no state log, so there is no log to " +
					"write an eviction to — ask a node that runs the native " +
					"tracker or knowledge base",
			})
			return
		}
		node := r.PathValue("node")
		// THE CONFIRMATION ECHOES THE NODE ID, the same shape the
		// destructive CLI gestures already use: an eviction stops a
		// machine's records applying and a readmission lets them apply
		// again, and neither is a value to get from a shell history.
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
			// this is the build with no operator identity on the
			// context at all — and every log's record names who ran
			// the gesture, which is what `retention status` prints
			// beside an eviction.
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "operator_required",
				"detail": "an eviction and a readmission are operator gestures " +
					"and this request carries no operator identity",
			})
			return
		}
		opID, ok := callerOpID(w, r, gateVerb(evict)+"-"+node)
		if !ok {
			return
		}
		req := engine.GateRequest{
			Node: node, OpID: opID, By: operator,
			Force: evict && r.URL.Query().Get("force") == "true",
		}
		var result engine.GateResult
		var err error
		if evict {
			result, err = a.nodes.Evict(r.Context(), req)
		} else {
			result, err = a.nodes.Readmit(r.Context(), req)
		}
		var readmission *statelog.ReadmissionRefusal
		var eviction *statelog.EvictionRefusal
		switch {
		case errors.As(err, &readmission):
			// A REFUSAL IS AN ANSWER, NOT A FAULT, and it is the one
			// this route promises: the node the operator asked for is
			// below a floor it would be counted against, and nothing
			// was written. 409 with the numbers, because the inequality
			// is the reason and the CLI prints the detail and the hint
			// beneath the status — an operator told only "500
			// gate_failed" would read an engine problem where there is
			// a node still catching up.
			log.Info("retention_readmission_refused", "operator", operator,
				"node", node, "domain", readmission.Domain,
				"position", readmission.Seq, "floor", readmission.Bound.Held())
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "readmission_refused",
				"detail": readmission.Error(),
				"hint":   readmission.Remedy(),
				"node":   node, "domain": readmission.Domain,
				"published":        readmission.Published,
				"position":         readmission.Seq,
				"generation":       readmission.Generation,
				"floor":            readmission.Bound.Floor,
				"first_seq":        readmission.Bound.First,
				"floor_generation": readmission.Bound.Generation,
			})
			return
		case errors.As(err, &eviction):
			// THE SAME FOR AN EVICTION: a node still renewing its
			// presence lease is still reaching the fleet, and nothing
			// was written anywhere.
			log.Info("retention_eviction_refused", "operator", operator,
				"node", node, "detail", eviction.Detail)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "eviction_refused",
				"detail": eviction.Error(),
				"hint":   eviction.Remedy(),
				"node":   node,
			})
			return
		case err != nil:
			log.Warn("api_retention_gate_failed", "node", node,
				"evict", evict, "error", err)
			writeJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "gate_failed", "detail": err.Error()})
			return
		}
		domains := make([]map[string]any, 0, len(result.Domains))
		for _, d := range result.Domains {
			entry := map[string]any{
				"domain": d.Domain, "stream": d.Stream, "op_id": d.OpID,
			}
			if d.Err != nil {
				// NO OUTCOME, AND THE REASON WHY — a refusal names
				// its reason in the vocabulary every write refusal
				// uses, so a caller can tell `log_full` from
				// `evicted` without reading the sentence.
				entry["error"] = d.Err.Error()
				var refused *statelog.Unavailable
				if errors.As(d.Err, &refused) {
					entry["reason"] = refused.Reason
				}
			} else {
				// THE THREE-VALUED OUTCOME, whole. A gate the caller
				// believes landed and which is only `pending` is the
				// difference between a node that has stopped writing
				// and one that is about to.
				entry["outcome"] = d.Outcome
				entry["position"] = d.Position
			}
			domains = append(domains, entry)
		}
		complete := result.Complete()
		log.Info("retention_gate", "operator", operator, "node", node,
			"evict", evict, "op_id", result.OpID, "complete", complete)
		writeJSON(w, http.StatusOK, map[string]any{
			"node": node, "evicted": evict, "op_id": result.OpID,
			"complete": complete, "domains": domains,
		})
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
// creation instant, which is the value the confirmation has to echo, and the
// case a reanchor would answer now — `recreated` (followed from its first
// surviving record) or `restored` (followed from its end), with the sequence
// the checkpoint would go to — or, with no case, why there is nothing to
// re-anchor.
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
	view, err := a.capacity.ReanchorStatus(r.Context(), stream)
	switch {
	case errors.Is(err, engine.ErrUnknownStream):
		writeJSON(w, http.StatusNotFound,
			map[string]string{"error": "unknown_stream", "detail": err.Error()})
		return
	case err != nil:
		// THE LOG EXISTS AND THE BROKER DID NOT ANSWER. The instant is read
		// live, so this is the one failure here worth retrying — and
		// reporting it as an unknown stream sent an operator looking for a
		// typo in a name that was right.
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "stream_unreadable", "detail": err.Error()})
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
	writeJSON(w, http.StatusOK, answer)
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
	operator, _ := auth.OperatorFrom(r.Context())
	plan, err := a.capacity.Reanchor(r.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: confirm, By: operator,
		Force: r.URL.Query().Get("force") == "true",
	})
	if err != nil {
		log.Warn("api_reanchor_refused", "stream", stream, "error", err)
		writeJSON(w, http.StatusConflict,
			map[string]string{"error": "reanchor_refused", "detail": err.Error()})
		return
	}
	log.Warn("reanchored", "operator", operator, "stream", stream,
		"generation", plan.Generation, "case", string(plan.Case), "cursor", plan.Cursor)
	writeJSON(w, http.StatusOK, map[string]any{
		"stream": stream, "generation": plan.Generation, "case": plan.Case,
		"cursor": plan.Cursor,
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
