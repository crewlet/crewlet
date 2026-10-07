package eventfan

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// EVERY QUESTION THAT IS FLOORED CARRIES THE ASKER'S INSTANT, AND ONE THAT
// CARRIES NONE IS REFUSED.
//
// Every node floors the history at the instant its asker sends, so a node that
// answered a question carrying none as of its own clock would answer a
// different question from its peers — a union of horizons, merged as one. No
// asker on this protocol sends one, so a node refuses it and is named in the
// coverage rather than answering around it. The one question asked at no
// instant is [QuestionKept], which names rows a floored question already
// found.
//
// Mutation: drop the instant check from any one question in [answer], and that
// case is answered.
func TestAQuestionWithNoInstantIsRefused(t *testing.T) {
	t.Parallel()
	floored := map[Question]any{
		QuestionEvents:               listParams{Limit: 10},
		QuestionSeries:               seriesParams{Bucket: store.BucketHour},
		QuestionEvent:                idParams{ID: "e"},
		QuestionTrace:                idParams{ID: "t"},
		QuestionTurn:                 idParams{ID: "t"},
		QuestionTurns:                turnsParams{SinceDays: 7},
		QuestionPhases:               phasesParams{Limit: 10},
		QuestionSeatPhases:           phasesParams{Role: "PM"},
		QuestionTraceRows:            traceRowsParams{TraceIDs: []string{"t"}, Limit: 10},
		QuestionPhaseTokens:          phaseTokenParams{},
		QuestionNotificationOutcomes: outcomeParams{},
	}
	for _, q := range Questions {
		if _, ok := floored[q]; !ok && q != QuestionKept {
			t.Errorf("question %q is not in this case: decide whether it carries an instant", q)
		}
	}
	for q, params := range floored {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		// NO STORE: a question refused for its instant never reaches one.
		if _, err := answer(t.Context(), nil, q, raw, nil); err == nil ||
			!strings.Contains(err.Error(), "no instant") {
			t.Errorf("%s with no instant answered %v, want it refused", q, err)
		}
	}
}

// THE TURNS CURSOR TRAVELS WHOLE: its start and its turn id go out together and
// come back as the cursor that was sent, because a start alone puts the second
// of two turns at one microsecond behind the cursor and on no page.
func TestATurnCursorCrossesTheWireWhole(t *testing.T) {
	t.Parallel()
	cursor := &store.TurnCursor{Start: time.Now().UTC().Truncate(time.Microsecond), TurnID: "t-1"}
	wire := turnsParamsOf(store.TurnQuery{SinceDays: 7, Before: cursor, At: time.Now()})
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var back turnsParams
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.query(nil).Before; got == nil || *got != *cursor {
		t.Errorf("the cursor %+v came back off the wire as %+v", cursor, got)
	}
}
