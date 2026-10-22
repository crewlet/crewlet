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
// ([builtin.ConditionSentence]), the busy surface in a sentence of its own, and
// anything else in a fixed one; each error goes to the log.
//
// Mutation: answer err.Error() as the detail again and every row goes red.
func TestADisconnectNotRecordedIsToldInAPersonsWords(t *testing.T) {
	t.Parallel()
	blink := fmt.Errorf("setupapi: this node could not take slack to record the "+
		"disconnect: dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)
	blinkSaid, _ := builtin.ConditionSentence(blink)
	for name, c := range map[string]struct {
		err  error
		code string
		says string
	}{
		"a coordination store that blinked": {blink, "unavailable", blinkSaid},
		"a pass holding the surface": {fmt.Errorf("%w: slack is being provisioned "+
			"right now", errSurfaceBusy), "surface_busy", "This integration is being " +
			"set up right now, so the disconnect didn't start. Try again in a moment."},
		"anything else": {fmt.Errorf("setupapi: the fleet's record of slack could " +
			"not be read: decode: unexpected EOF"), "unavailable",
			"This node couldn't start the disconnect. Try again in a moment."},
	} {
		rec := httptest.NewRecorder()
		refuseDisconnect(rec, httptest.NewRequest(http.MethodDelete,
			"/setup/integrations/slack", nil), integration.KindSlack, c.err)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body["error"] != c.code ||
			body["detail"] != c.says {
			t.Errorf("%s answered %d %v, want 503 %s with %q", name, rec.Code, body,
				c.code, c.says)
		}
		if text := rec.Body.String(); strings.Contains(text, "setupapi") ||
			strings.Contains(text, "10.0.0.7") || strings.Contains(text, "EOF") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s carried no Retry-After", name)
		}
	}
}
