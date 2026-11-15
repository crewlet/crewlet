package setupapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// for a reader of one agent's access: with its grants, open requests, catalogue
// count, and the entries `permission` matches.
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
	lookup := strings.TrimSpace(r.URL.Query().Get("permission"))
	if n := utf8.RuneCountInString(lookup); n > seataccess.MaxLookupChars {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery, map[string]string{
			"detail": "permission is " + strconv.Itoa(n) + " characters; look up at most " +
				strconv.Itoa(seataccess.MaxLookupChars) + ", part of the permission's name",
		})
		return
	}
	requests, err := s.access.GrantRequests(r.Context(), kind)
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_requests_unread", err)
		return
	}
	view, found := seataccess.ViewOf(answer, handle, lane, seataccess.ViewQuery{Requests: requests,
		Lookup: lookup, Now: time.Now()})
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
