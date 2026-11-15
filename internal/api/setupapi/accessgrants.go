package setupapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A seat's extra grants ride the Agents tab's draft like its level: added in
// the draft, previewed, saved as one revision. What the draft cannot say is
// who granted them, when, or what an agent's request asked: the engine
// stamps those here, from the person saving and the stored request, so the
// document records what the engine saw rather than what a body claimed.

// settleGrants stamps every grant the draft adds ([seataccess.StampGrants])
// and builds each that answers an agent's request from the request as stored,
// refusing a grant edited in place or a request that is gone or asked for
// something else. It reports whether the route may go on.
func (s *Service) settleGrants(w http.ResponseWriter, r *http.Request, current seataccess.Answer,
	draft *accessDraft) bool {

	now := time.Now().UTC()
	by := attributionOf(r).Name
	for _, handle := range sortedHandles(draft.Seats) {
		seat, _ := seatOf(current, handle)
		for _, lane := range sortedLanes(draft.Seats[handle]) {
			after := draft.Seats[handle][lane]
			if len(after.Grants) == 0 {
				continue
			}
			before := seat.Lanes[lane].Desired
			stamped, err := seataccess.StampGrants(before, after, by, now)
			if err == nil {
				err = s.fromRequests(r, handle, lane, before, &stamped, now)
			}
			if err != nil {
				httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeAccessNotOffered,
					refusalFields(seataccess.OfferRefusal{Seat: handle, Lane: lane,
						Field: seataccess.FieldGrants, Detail: err.Error()}))
				return false
			}
			draft.Seats[handle][lane] = stamped
		}
	}
	return true
}

// fromRequests rebuilds every grant d adds that names an agent's request from
// the request itself: its reason, and its hours where the grant lapses, so
// the person's choice of option is the only thing the body decides.
func (s *Service) fromRequests(r *http.Request, handle string, lane seataccess.Lane,
	before seataccess.Desired, d *seataccess.Desired, now time.Time) error {

	for i, g := range d.Grants {
		if g.Request == "" || held(before, g.ID) {
			continue
		}
		req, err := s.access.GrantRequest(r.Context(), g.Request)
		switch {
		case errors.Is(err, seataccess.ErrNoRequest):
			return fmt.Errorf("the agent's request %s has lapsed or was already settled; "+
				"ask the agent to request it again", g.Request)
		case err != nil:
			return fmt.Errorf("the agent's request %s could not be read: %w", g.Request, err)
		case req.Handle != handle || req.Lane != lane:
			return fmt.Errorf("the agent's request %s was asked by another agent", g.Request)
		}
		built := req.Grant(g.ExpiresAt.IsZero(), now)
		if seataccess.GrantKey(built) != seataccess.GrantKey(g) {
			return fmt.Errorf("the agent's request %s asked for %s, not %s", g.Request,
				seataccess.GrantKey(built), seataccess.GrantKey(g))
		}
		built.ID, built.GrantedBy, built.GrantedAt = g.ID, g.GrantedBy, g.GrantedAt
		d.Grants[i] = built
	}
	return nil
}

// held reports whether before holds a grant with id.
func held(before seataccess.Desired, id string) bool {
	for _, g := range before.Grants {
		if g.ID == id {
			return true
		}
	}
	return false
}

// grantRefusal is why a lane's grants cannot be saved as the vendor's
// catalogue offers them now, or nil ([seataccess.CheckGrants]). A catalogue
// that was not read cannot vouch for a grant, so one added on it is refused
// rather than saved on a guess.
func grantRefusal(o seataccess.Options, before, after seataccess.Desired) *seataccess.OfferRefusal {
	if seataccess.SameStoredGrants(before.Grants, after.Grants) {
		return nil
	}
	if o.Grants == nil && len(seataccess.AddedGrants(before.Grants, after.Grants)) > 0 {
		return &seataccess.OfferRefusal{Field: seataccess.FieldGrants,
			Detail: seataccess.LaneName(o.Lane) + "'s catalogue of permissions could not be read just now, " +
				"so no grant can be added until it answers"}
	}
	if err := seataccess.CheckGrants(o, before, after, time.Now().UTC()); err != nil {
		return &seataccess.OfferRefusal{Field: seataccess.FieldGrants, Detail: err.Error()}
	}
	return nil
}

// grantChanges is what a save did to each changed lane's grants, for the
// audit trail and to settle the requests it answered.
func grantChanges(changed []accessChange) []seataccess.GrantChange {
	var out []seataccess.GrantChange
	for _, c := range changed {
		added := seataccess.AddedGrants(c.before.Grants, c.after.Grants)
		removed := seataccess.RemovedGrants(c.before.Grants, c.after.Grants)
		if len(added)+len(removed) == 0 {
			continue
		}
		out = append(out, seataccess.GrantChange{Handle: c.handle, Lane: c.lane, Added: added, Removed: removed})
	}
	return out
}

// accessRequest serves GET /setup/access/requests/{id}: one agent's stored
// request for a grant, which the Inbox draws a request's grant options from
// and builds the grant's draft with. A request granted, declined or lapsed is
// not found, and its question says so.
func (s *Service) accessRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, err := s.access.GrantRequest(r.Context(), id)
	switch {
	case errors.Is(err, seataccess.ErrNoRequest):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no open request " + id + ": it was granted, declined, or lapsed after " +
				"thirty days unanswered",
			"hint": "answer the question Not now, and the agent asks again if it still needs it",
		})
		return
	case err != nil:
		s.accessFault(w, r, req.Kind, "setup_access_request_unread", err)
		return
	}
	httpjson.Write(w, http.StatusOK, req)
}
