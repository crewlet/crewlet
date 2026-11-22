package setupapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// appStoreKeyRemove serves DELETE /setup/integrations/app_store_connect/keys/
// {connection}/{slot}: one action takes a key out of the company document,
// which the next call already reads, and then removes its sealed .p8.
func (s *Service) appStoreKeyRemove(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	id, slot := r.PathValue("connection"), r.PathValue("slot")
	notFound := func(detail string) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{"detail": detail})
	}
	block := fc.company.Integrations.AppStoreConnect
	if block == nil {
		notFound("App Store Connect is not connected")
		return
	}
	team, named := block.Connections[id]
	if !named {
		notFound(fmt.Sprintf("no App Store Connect connection is named %q", id))
		return
	}
	var key *config.AppStoreConnectKey
	slots := team.Keys.Slots()
	for _, sl := range slots {
		if sl.Name == slot {
			key = sl.Key
		}
	}
	switch {
	case key == nil:
		notFound(fmt.Sprintf("the %s connection holds no %s key", id, slot))
		return
	case len(slots) == 1:
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{"detail": fmt.Sprintf(
			"the %s key is the %s connection's last: add another key first, or disconnect App Store Connect", slot, id)})
		return
	}
	patch, err := json.Marshal(map[string]any{"integrations": map[string]any{string(integration.KindAppStoreConnect): map[string]any{
		"connections": map[string]any{id: map[string]any{"keys": map[string]any{slot: nil}}}}}})
	if err != nil {
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	applied, err := s.config.Apply(r.Context(), configapi.ApplyRequest{Patch: patch, By: attributionOf(r),
		Summary: fmt.Sprintf("remove the %s connection's %s key", id, slot), Expect: fc.revision})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	// THE REFERENCE GOES FIRST: a sealed key nothing names any more is
	// harmless, where a reference to a removed one fails every call.
	sealedLeft := ""
	if name, whole := envref.Whole(strings.TrimSpace(key.PrivateKey)); whole {
		remover, removes := s.secrets.(SealedRemover)
		if !removes {
			sealedLeft = name
		} else if _, err := remover.Unset(r.Context(), name); err != nil {
			log.WarnContext(r.Context(), "app_store_connect_key_unremoved", "secret", name, "error", err.Error())
			sealedLeft = name
		}
	}
	out := map[string]any{"key": integration.KindAppStoreConnect, "revision_id": applied.RevisionID,
		"epoch": applied.Epoch, "removed": slot}
	if sealedLeft != "" {
		out["sealed_left"] = sealedLeft
	}
	httpjson.Write(w, http.StatusOK, out)
}
