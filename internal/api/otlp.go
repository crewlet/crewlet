package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/otlpenv"
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
// What authenticates a request instead is the token minted for one run in its
// own PATH — scoped to that run's trace and expiring (see [sandbox.OtelTokens]
// for what the scope holds) — and the auth package exempts `/otlp/` by prefix
// for exactly that reason. So this handler verifies before it does anything,
// on the same terms as the webhook edge beside it.
//
// # Traces only
//
// The route is `/otlp/{token}/v1/traces` and nothing else: a box exports its
// traces here, and a metrics or a logs path under a valid token is the mux's
// own 404, never read and never forwarded. No runner the engine ships exports
// either through a box's endpoint, so serving them only gave a token copied
// out of a box a channel the runners never use (internal/sandbox/otelwire.go
// says why).

// mountOTLP registers the receiver, or says why it did not.
//
// A nil receiver is an ordinary configuration — most deployments run no
// sandbox telemetry — and the route is then ABSENT rather than answering
// 503: an endpoint that exists and refuses everything reads to an operator
// as broken, while one that is not there matches what the config says.
func (a *App) mountOTLP(mux httpjson.Router, receiver *sandbox.OtelReceiver) {
	if receiver == nil {
		return
	}
	mux.Handle("POST "+auth.OTLPPrefix+"{token}"+otlpenv.Traces.Path(), http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			a.serveOTLP(w, r, receiver)
		}))
	log.Info("sandbox_otlp_receiver_mounted", "forwards", receiver.Forwards())
}

func (a *App) serveOTLP(w http.ResponseWriter, r *http.Request, receiver *sandbox.OtelReceiver) {
	// VERIFIED BEFORE THE BODY IS READ, so an unauthenticated caller
	// cannot make this process buffer four megabytes per request.
	traceID := receiver.Validate(r.PathValue("token"))
	if traceID == "" {
		// NO DETAIL. Forged, malformed and expired are three different
		// facts, and telling the caller which one it was tells an
		// attacker the same. The exporter's only move is identical for
		// all three.
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, sandbox.MaxOtelPayload+1))
	if err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeUnreadableBody)
		return
	}
	if len(body) > sandbox.MaxOtelPayload {
		// The SHARED spelling of a 413. This route answered "payload too
		// large" as text/plain while two others answered JSON with two
		// different codes.
		httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
		return
	}

	// FORWARDED SYNCHRONOUSLY, AND ACCEPTED WHATEVER THE BACKEND DID.
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
	// operator can see it, and is never the box's problem. What IS the
	// box's is an export the receiver cannot read, which is answered as
	// the OTLP/HTTP specification asks: 415 for a media type or an encoding
	// it does not speak, 413 for one that inflates past the bound, 400 for
	// one that does not decode — each final, so the exporter drops the
	// batch rather than sending it again.
	reply, err := receiver.Accept(r.Context(), traceID, sandbox.OtelExport{
		Body:            body,
		ContentType:     r.Header.Get("Content-Type"),
		ContentEncoding: r.Header.Get("Content-Encoding"),
	})
	switch {
	case errors.Is(err, sandbox.ErrOtelUnsupported):
		httpjson.Fail(w, http.StatusUnsupportedMediaType, httpjson.CodeInvalidBody)
		return
	case errors.Is(err, sandbox.ErrOtelTooLarge):
		httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
		return
	case err != nil:
		log.Debug("sandbox_otlp_refused", "trace_id", traceID, "error", err.Error())
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	w.Header().Set("Content-Type", reply.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply.Body)
	log.Debug("sandbox_otlp_received", "trace_id", traceID, "bytes", len(body))
}
