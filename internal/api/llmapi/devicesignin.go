package llmapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// startDevice serves POST /llm/providers/{key}/device-sign-in: the harness's
// device-code sign-in, run on this node, answered as soon as the harness printed
// its address and code. The person approves at the vendor; nothing is pasted
// back through the engine, and the login the approval makes is sealed as the
// entry's bundle and re-activated by the node running it.
func (s *Service) startDevice(w http.ResponseWriter, r *http.Request, _ *types.ModelActed) {
	key := r.PathValue("key")
	_, active, ok := s.activeEntry(w, r, key)
	if !ok {
		return
	}
	if active == nil {
		notFound(w, key)
		return
	}
	started, err := s.engine.StartDeviceSignIn(r.Context(), key, actor(r))
	var running *SignInInProgressError
	var mode *SignInModeError
	var unavailable *DeviceSignInUnavailableError
	switch {
	case err == nil:
		httpjson.Write(w, http.StatusCreated, queries.DeviceOf(started))
	case errors.Is(err, ErrNoSuchModel):
		notFound(w, key)
	case errors.As(err, &running):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeSignInInProgress,
			httpjson.Detail{"running": queries.DeviceOf(running.Running)})
	case errors.Is(err, ErrNoDeviceSignIn):
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeNoDeviceSignIn,
			map[string]string{"detail": key + "'s harness has no device sign-in"})
	case errors.As(err, &mode):
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeNoDeviceSignIn,
			map[string]string{"detail": key + " signs in with auth.mode " + string(mode.Mode) +
				"; change how it signs in first"})
	case errors.As(err, &unavailable):
		httpjson.FailWith(w, http.StatusBadGateway, httpjson.CodeDeviceSignInUnavailable,
			map[string]string{"detail": unavailable.Line})
	default:
		s.fail(w, "start the device sign-in", err)
	}
}

// cancelDevice serves DELETE /llm/device-sign-ins/{id}: the record is marked
// for cancelling, which the node running the sign-in watches and acts on.
func (s *Service) cancelDevice(w http.ResponseWriter, r *http.Request, rec *types.ModelActed) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	cancelled, err := s.engine.CancelDeviceSignIn(r.Context(), id, actor(r))
	switch {
	case errors.Is(err, ErrNoSuchSignIn):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	case err != nil:
		s.fail(w, "cancel the device sign-in", err)
		return
	}
	rec.ProviderKey = cancelled.Entry
	httpjson.Write(w, http.StatusOK, map[string]any{"id": cancelled.ID, "status": coord.DeviceCancelled})
}
