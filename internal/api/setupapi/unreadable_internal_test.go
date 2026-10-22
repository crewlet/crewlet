package setupapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// A SETUP WRITE THAT MEETS A REVISION THIS NODE CANNOT OPEN is the config
// surface's own 409, told by where the revision came from, with what the
// submission sealed before it: the write's apply opens the fleet's revision
// it is built on, and a body this node cannot open reached the default arm as
// the 500 that says the engine broke.
//
// Mutation: drop the arm from refuse and this answers 500.
func TestASetupWriteOnARevisionThisNodeCannotOpenSaysWhatFixesIt(t *testing.T) {
	t.Parallel()
	unreadable := &configapi.UnreadableRevisionError{ID: "the-fleets", Fleet: true,
		Err: secrets.ErrDecrypt}
	sealed := setup.Result{Secrets: []string{"SLACK_BOT_TOKEN"}}
	rec := httptest.NewRecorder()
	(&Service{}).refuse(rec, httptest.NewRequest(http.MethodPost,
		"/setup/integrations/slack/disconnect", nil), unreadable, sealed)
	var body struct {
		Error        string   `json:"error"`
		RevisionID   string   `json:"revision_id"`
		Hint         string   `json:"hint"`
		WroteSecrets []string `json:"wrote_secrets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body.Error != "unreadable_fleet_revision" ||
		body.RevisionID != "the-fleets" || body.Hint == "" {
		t.Errorf("a setup write on a fleet revision this node cannot open answered %d %s, "+
			"want 409 unreadable_fleet_revision naming it with a hint", rec.Code, rec.Body)
	}
	if !slices.Equal(body.WroteSecrets, sealed.Secrets) {
		t.Errorf("wrote_secrets = %v, want what the submission sealed, %v", body.WroteSecrets,
			sealed.Secrets)
	}
}
