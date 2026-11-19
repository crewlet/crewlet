package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tokens"
)

// A PRINCIPAL WHOSE STAGE MAY NOT ACT READS NO MIRROR OF THE SNAPSHOT, the
// overview included.
//
// The four REST mirrors (`/agents`, `/org`, `/tools`, `/stream/snapshot`) are
// everybody's, so only the stage check stands in front of them. The guard
// refuses such a principal first, so the handler is asked directly. Every stage
// is walked: the rule is an allowlist of one. The refusal names `stage` and no
// permission, and nothing is BUILT for a refused principal.
//
// Mutation: drop the stage check from [App.serveFrom], and every stage but
// active is answered 200 with the roster built.
func TestAMirrorRefusesAPrincipalWhoseStageMayNotAct(t *testing.T) {
	t.Parallel()
	a := &App{stream: mirrorService(t)}
	for _, stage := range iam.Stages {
		built := false
		handler := a.serveFrom(stream.KindAgents, func(stream.Audience) any {
			built = true
			return stream.AgentRows{}
		})
		p := iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson,
			Seat: "analyst", Stage: stage, Grants: []iam.Grant{iam.GrantAdmin}}
		req := httptest.NewRequest(http.MethodGet, "/agents", nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if stage.MayAct() {
			// THE CONTROL: the same admin, active, is served — so the
			// refusals below are the stage's doing and nothing else's.
			if rec.Code != http.StatusOK || !built {
				t.Errorf("an admin whose stage is %s was answered %d (built "+
					"%v), want the roster: %s", stage, rec.Code, built,
					rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("an admin whose stage is %s reading /agents answered "+
				"%d, want 403", stage, rec.Code)
			continue
		}
		if built {
			t.Errorf("the roster was built for a principal whose stage is "+
				"%s, which the mirror then refused", stage)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the refusal of stage %s is not the engine's envelope: %v",
				stage, err)
		}
		if body[authz.DetailReason] != string(authz.ReasonStage) {
			t.Errorf("the refusal of stage %s names reason %v, want %q", stage,
				body[authz.DetailReason], authz.ReasonStage)
		}
		if grants, present := body[authz.DetailGrants]; present && grants != nil {
			if list, ok := grants.([]any); !ok || len(list) != 0 {
				t.Errorf("the refusal of stage %s names grants %v — no "+
					"permission admits a stage that may not act", stage, grants)
			}
		}
	}
}

// mirrorService is a stream service answering every surface function with an
// empty value: the case is about who a mirror answers, not what it carries.
func mirrorService(t *testing.T) *stream.Service {
	t.Helper()
	svc, err := stream.NewService(livestate.New(), stream.Options{
		Health:    func() stream.Health { return Health{Status: StatusOK} },
		Posture:   func(stream.Health) stream.FramePosture { return stream.FrameLive },
		Seats:     func() tokens.Seats { return tokens.Seats{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return map[string]any{} },
		Tools:     func() any { return nil },
		Schedules: func() any { return []any{} },
		Chart:     authz.NoChart{},
		Holders:   holdsNobody{},
		SeatOf: func(string) (stream.SeatState, bool) {
			return stream.SeatState{Human: true}, true
		},
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
	})
	if err != nil {
		t.Fatalf("stream.NewService: %v", err)
	}
	t.Cleanup(svc.Stop)
	return svc
}

// holdsNobody is a directory in which no login is held.
type holdsNobody struct{}

func (holdsNobody) HolderRecord(context.Context, string) (string, error) {
	return "", iam.ErrNoHolder
}
