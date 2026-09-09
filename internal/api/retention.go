package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/coord"
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
	Positions(ctx context.Context) ([]coord.NodePositions, error)
	Floors(ctx context.Context) ([]coord.TrimFloor, error)
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
	if a.retention == nil {
		// A standalone API with no coordination store. 503 rather than
		// 404, on this surface's own rule: the route EXISTS on this
		// build, and a 404 sends an operator looking for a version
		// mismatch that is not there.
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "no_coordination_store"})
		return
	}
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
	generation, err := a.generationOf(r.Context(), stream)
	if err != nil {
		// A BARE SEQUENCE NAMES A NUMBER SPACE. Publishing one at the
		// wrong generation pins a position on a log that no longer
		// exists, which the trim reads as no acknowledgement at all —
		// so a generation nobody could read is refused rather than
		// assumed to be the current one.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":  "generation_unknown",
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

// generationOf is the generation this fleet's own records are at for one
// stream, read from the published floors and then from the register.
//
// TWO SOURCES BECAUSE EITHER CAN BE THE ONLY ONE. The floor is written by the
// trim duty and is absent on a fleet whose duty has not run; the register is
// written by every node's heartbeat and is keyed by DOMAIN rather than by
// stream, so it answers only once something has published a position carrying
// this stream's own name.
func (a *App) generationOf(ctx context.Context, stream string) (uint32, error) {
	// THE PUBLISHED FLOOR FIRST. The trim writes it from the running
	// applier's own generation, and it is the fleet's single answer — one
	// row per domain, written by whichever node holds the duty.
	floors, err := a.retention.Floors(ctx)
	if err != nil {
		return 0, err
	}
	if len(floors) > 0 {
		return floors[0].Generation, nil
	}

	// THEN ANY NODE'S OWN REPORT, for the fleet whose trim duty has not
	// ticked yet — a company in its first fifteen minutes, or one whose
	// nodes all declare `roles: [seats]`. Every node publishes the
	// generation it is applying at, and they agree: a generation changes
	// only by a reanchor, which is one committed record every node reads.
	rows, err := a.retention.Positions(ctx)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		for _, at := range row.Domains {
			return at.Generation, nil
		}
	}

	// NOT ZERO-BY-DEFAULT. Generation zero is a real value — a fleet that
	// has never reanchored — and it is indistinguishable here from "nobody
	// has published anything yet", which is a fleet whose acknowledgement
	// would name a log nothing is following.
	return 0, errors.New("no node has published a position and no trim floor " +
		"has been written, so this fleet's generation cannot be established")
}

// mountRetention registers the write half of the retention surface.
func (a *App) mountRetention(mux *http.ServeMux) {
	mux.Handle("POST /work/retention/ack", http.HandlerFunc(a.serveRetentionAck))
	mux.Handle("POST /work/retention/evict/{node}", a.gate(true))
	mux.Handle("POST /work/retention/readmit/{node}", a.gate(false))
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
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": "no_tracker"})
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
		operator, _ := auth.OperatorFrom(r.Context())
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
			writeJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "gate_failed", "detail": err.Error()})
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
