package setupapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
)

// A DISCONNECT THIS NODE COULD NOT RECORD IS TOLD IN A PERSON'S WORDS.
//
// Its detail was the error: "setupapi: this node could not take slack to record
// the disconnect: " and the coordination store's own transport error, hosts and
// ports, for the coordination store blinking; a pass holding the surface read
// the same way under its sentinel. A condition is told in its own sentence
// ([builtin.ConditionSentence]) and the busy surface in a sentence of its own,
// each under a Retry-After; anything else is this node's own fault, `500` with
// no Retry-After, which it was not — a 503 telling a person to try again in a
// moment, for a fault no wait clears. Each error goes to the log.
//
// Mutation: answer err.Error() as the detail again and every row goes red;
// answer a fault as a 503 and the last one does.
func TestADisconnectNotRecordedIsToldInAPersonsWords(t *testing.T) {
	t.Parallel()
	blink := fmt.Errorf("setupapi: this node could not take slack to record the "+
		"disconnect: dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)
	blinkSaid, _ := builtin.ConditionSentence(blink)
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
		says   any
	}{
		"a coordination store that blinked": {blink, http.StatusServiceUnavailable,
			"unavailable", blinkSaid},
		"a pass holding the surface": {fmt.Errorf("%w: slack is being provisioned "+
			"right now", errSurfaceBusy), http.StatusServiceUnavailable, "surface_busy",
			"This integration is being set up right now, so the disconnect didn't " +
				"start. Try again in a moment."},
		"anything else": {fmt.Errorf("setupapi: the fleet's record of slack could " +
			"not be read: decode: unexpected EOF"), http.StatusInternalServerError,
			"internal_error", nil},
	} {
		rec := httptest.NewRecorder()
		refuseDisconnect(rec, httptest.NewRequest(http.MethodDelete,
			"/setup/integrations/slack", nil), integration.KindSlack, c.err)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body["error"] != c.code || body["detail"] != c.says {
			t.Errorf("%s answered %d %v, want %d %s with %v", name, rec.Code, body,
				c.status, c.code, c.says)
		}
		if text := rec.Body.String(); strings.Contains(text, "setupapi") ||
			strings.Contains(text, "10.0.0.7") || strings.Contains(text, "EOF") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
		if retry := rec.Header().Get("Retry-After") != ""; retry != (c.status ==
			http.StatusServiceUnavailable) {
			t.Errorf("%s: Retry-After %q", name, rec.Header().Get("Retry-After"))
		}
	}
}
