package api

import (
	"io"
	"net/http"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The OTLP receive-and-forward edge.
//
// A coding agent inside a sandbox exports its telemetry HERE rather than to
// the real backend, so the backend's ingest credential never enters a box
// running generated code. The receiver adds that credential on the way out —
// see [sandbox.OtelReceiver] for why the hop exists at all.
//
// # This route is deliberately reachable without the API's own auth
//
// It has to be: the exporter inside the box holds no API token, and giving it
// one would be handing a sandbox the credential that reads the whole company.
// What authenticates a request instead is the per-run, trace-scoped, expiring
// token in its own PATH, and the auth package exempts `/otlp/` by prefix for
// exactly that reason. So this handler verifies before it does anything, on
// the same terms as the webhook edge beside it.

// maxOtelBody bounds one exported payload.
//
// An OTLP batch is small — spans and metric points, not artefacts — and the
// exporter inside the box splits its own batches. A cap keeps a box that
// exports garbage from allocating the engine's memory on an endpoint that
// needs no other credential to reach.
const maxOtelBody = 4 << 20

// mountOTLP registers the receiver, or says why it did not.
//
// A nil receiver is an ordinary configuration — most deployments run no
// sandbox telemetry — and the route is then ABSENT rather than answering
// 503: an endpoint that exists and refuses everything reads to an operator
// as broken, while one that is not there matches what the config says.
func (a *App) mountOTLP(mux *http.ServeMux, receiver *sandbox.OtelReceiver) {
	if receiver == nil {
		return
	}
	mux.Handle("POST /otlp/{token}/v1/{signal}", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			a.serveOTLP(w, r, receiver)
		}))
	log.Info("sandbox_otlp_receiver_mounted", "forwards", receiver.Forwards())
}

func (a *App) serveOTLP(w http.ResponseWriter, r *http.Request, receiver *sandbox.OtelReceiver) {
	signal := r.PathValue("signal")
	// THE SIGNAL IS CHECKED AGAINST A CLOSED SET, because it is
	// concatenated into the upstream URL — an unchecked segment lets a
	// caller choose part of the address the engine's OWN credential is
	// sent to.
	if !sandbox.ValidSignal(signal) {
		http.Error(w, "unknown signal", http.StatusNotFound)
		return
	}
	// VERIFIED BEFORE THE BODY IS READ, so an unauthenticated caller
	// cannot make this process buffer four megabytes per request.
	traceID := receiver.Validate(r.PathValue("token"))
	if traceID == "" {
		// NO DETAIL. Forged, malformed and expired are three different
		// facts, and telling the caller which one it was tells an
		// attacker the same. The exporter's only move is identical for
		// all three.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxOtelBody+1))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(body) > maxOtelBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	// FORWARDED SYNCHRONOUSLY, AND ACCEPTED WHATEVER HAPPENS.
	//
	// The forward is bounded by the receiver's own timeout, so the caller
	// waits at most that long — which is the right trade against the two
	// alternatives. Answering before forwarding would need a goroutine
	// outliving the request, on an endpoint reachable without any other
	// credential; and reporting the upstream's failure would make an
	// exporter that gets a 5xx retry, turning one slow backend into a
	// retry storm from every running sandbox.
	//
	// So a failed forward costs a gap in a trace, is logged where an
	// operator can see it, and is never the box's problem.
	receiver.Forward(r.Context(), signal, body, r.Header.Get("Content-Type"))
	w.WriteHeader(http.StatusOK)
	log.Debug("sandbox_otlp_received", "signal", signal, "trace_id", traceID,
		"bytes", len(body))
}
