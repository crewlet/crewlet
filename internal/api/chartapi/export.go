package chartapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// THE WHOLE COMPANY, AS A DOCUMENT SOMEBODY CAN EDIT AND SEND BACK.
//
// # Why it is /company/export rather than /chart?runtime=true
//
// The two answer different questions. `GET /chart` is a VIEW: it renders what
// a screen draws, with the answer's own position beside it and the runtime
// half stripped unless the caller may read it. This is a ROUND TRIP — the
// document an operator authors, in the shape `crewlet validate` reads and
// `crewlet config import` divides — so it is whole or it is useless, and a
// stripped export would be a file that silently deletes half of every seat
// the moment somebody imports it back.
//
// So it takes the grant that reads the company document, always, and there is
// no stripped posture here to get wrong.
//
// # What it deliberately does not carry
//
// The company's SETTINGS. They are a stored revision with their own route,
// their own history and their own validation, and an export that merged the
// two would hand back a file no route accepts: /config refuses a body
// carrying a chart, by name. What this returns is the chart's half of the
// file an operator authors, and the CLI is what puts the two together.

// getExport answers the chart as an authored document.
func (s *Service) getExport(w http.ResponseWriter, r *http.Request) {
	if key := refuseUnknownParams(r); key != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "this route does not read " + key})
		return
	}
	fresh, err := s.freshness(r)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	}
	got, err := s.reader.Read(r.Context(), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	units := make([]unitView, 0, len(got.Units))
	for _, u := range got.Units {
		units = append(units, viewOfUnit(u, true))
	}
	seats := make([]seatView, 0, len(got.Seats))
	for _, seat := range got.Seats {
		seats = append(seats, viewOfSeat(seat, true))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"units": units, "seats": seats,
		// THE POSITION THIS DOCUMENT IS TRUE AS OF, which is what makes
		// an edit-and-send-back safe to reason about: an import states
		// the revision it came from, and an operator holding two exports
		// can say which is older without opening them.
		"answer": answerOf(got.Answer),
	})
}
