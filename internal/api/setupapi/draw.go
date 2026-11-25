package setupapi

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/setup"
)

// draw serves POST /setup/integrations/{kind}/draw: a live view
// ([setup.PublicView.Live]) redrawn from what the form holds, nothing stored.
// Only the fields the view is drawn from may be typed, each held to its own
// rule as a save holds it, so a malformed value never reaches the block; the
// rest is read as a check reads it ([Service.formValues]).
//
// A read: the view holds public names and public halves alone.
func (s *Service) draw(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	state, ok := s.stateFor(r.Context(), fc.company, fc.roster, kind)
	drawer := s.drawers[kind]
	if !ok || drawer == nil {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownKind, map[string]string{
			"detail": "this build draws no live view for " + string(kind),
		})
		return
	}
	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var req struct {
		Field  string            `json:"field"`
		Values map[string]string `json:"values"`
	}
	if derr := decode(body, &req); derr != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": plainjson.Unreadable("the body", derr),
		})
		return
	}
	against := state.Requirements
	i := slices.IndexFunc(against, func(q setup.Requirement) bool { return q.Field == req.Field })
	if i < 0 || against[i].Public == nil || !against[i].Public.Live {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": req.Field + " has no view drawn from what the form holds.",
		})
		return
	}
	from := against[i].Public.DrawnFrom
	for _, field := range slices.Sorted(maps.Keys(req.Values)) {
		at := slices.IndexFunc(against, func(q setup.Requirement) bool { return q.Field == field })
		if !slices.Contains(from, field) || at < 0 || against[at].Kind == setup.KindSecret {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
				"detail": "The view of " + req.Field + " is not drawn from " + field + ".",
			})
			return
		}
		value := strings.TrimSpace(req.Values[field])
		if value == "" {
			continue
		}
		if refused := against[at].Refusal(value); refused != nil {
			refuseValue(w, refused)
			return
		}
	}
	view, err := drawer(setup.Draw{
		Field: req.Field, Company: fc.company.Name, Value: s.formValues(req.Values, against),
	})
	switch {
	case errors.Is(err, setup.ErrUnusableInput):
		httpjson.Write(w, http.StatusOK, map[string]any{"public": nil, "needs": err.Error()})
	case err != nil:
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeInternalError, map[string]string{
			"detail": "Crewlet could not draw the view of " + req.Field + ".",
		})
	default:
		httpjson.Write(w, http.StatusOK, map[string]any{"public": view})
	}
}
