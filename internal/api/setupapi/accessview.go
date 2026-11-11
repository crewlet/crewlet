package setupapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// accessSeatRead serves GET /setup/integrations/{kind}/access/seats/{handle}/{lane}:
// one agent's lane as its Agents tab row reads, in words ([seataccess.ViewOf]),
// for a reader that shows one agent's access rather than the whole tab.
func (s *Service) accessSeatRead(w http.ResponseWriter, r *http.Request) {
	kind, ok := accessKind(w, r)
	if !ok {
		return
	}
	lane := seataccess.Lane(r.PathValue("lane"))
	handle := strings.TrimSpace(r.PathValue("handle"))
	if !slices.Contains(seataccess.LanesOf(kind), lane) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": string(kind) + " grants no lane " + string(lane),
			"hint":   "one of " + laneList(kind),
		})
		return
	}
	q, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	answer, err := s.access.Answer(r.Context(), q)
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_unread", err)
		return
	}
	view, found := seataccess.ViewOf(answer, handle, lane)
	if !found {
		detail := map[string]string{"detail": "no agent " + strconv.Quote(handle) + " on " +
			string(kind)}
		var listed []string
		for _, seat := range answer.Seats {
			if !seat.Departed {
				listed = append(listed, seat.Handle)
			}
		}
		if len(listed) > 0 {
			detail["hint"] = "the agents on " + string(kind) + " are " + strings.Join(listed, ", ")
		}
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, detail)
		return
	}
	httpjson.Write(w, http.StatusOK, view)
}
