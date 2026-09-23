package iamapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// auditView is one entry of the identity estate's own trail.
type auditView struct {
	ID         string                 `json:"id"`
	Class      iamdomain.HistoryClass `json:"class"`
	ObjectKind iamdomain.ObjectKind   `json:"object_kind"`
	ObjectID   string                 `json:"object_id,omitempty"`
	Person     string                 `json:"person,omitempty"`
	Op         iamdomain.OpKind       `json:"op"`
	Actor      string                 `json:"actor,omitempty"`
	ActorKind  iam.Kind               `json:"actor_kind,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
	Summary    string                 `json:"summary,omitempty"`
	At         time.Time              `json:"at,omitzero"`
	Position   uint64                 `json:"position"`
}

// GetAudit is `GET /iam/audit`.
//
// # It pages by POSITION and never by time
//
// Two nodes' clocks are compared nowhere in this engine, so `since` is a
// position. A caller holding a timestamp passes `at=` instead and the route
// resolves it ONCE, against the rows' own instants, into a position — after
// which every page is a comparison between numbers the applier wrote.
//
// THE ANSWER CARRIES THE NODE'S OWN POSITION, so a reader can tell a quiet
// directory from a lagging node. Without it an empty page is ambiguous in the
// one direction that matters: nothing happened, or this node has not seen it.
func (s *Service) GetAudit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := iamdomain.HistoryQuery{
		Person: query.Get("person"),
		Op:     iamdomain.OpKind(query.Get("event")),
	}
	if q.Op != "" && !q.Op.Valid() {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": strconv.Quote(string(q.Op)) +
				" is not an operation this estate records"})
		return
	}
	for _, field := range []struct {
		name string
		into *uint64
	}{{"since", &q.Since}, {"before", &q.Before}} {
		raw := query.Get(field.name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
				map[string]string{"detail": field.name + " is a log POSITION " +
					"rather than a time; pass at= to resolve an instant to one"})
			return
		}
		*field.into = value
	}
	if raw := query.Get("at"); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
				map[string]string{"detail": "at is an RFC 3339 instant"})
			return
		}
		resolved, err := s.directory.PositionAt(r.Context(), at)
		if err != nil {
			s.unavailable(w, r, "resolve an instant to a position", err)
			return
		}
		q.Since = resolved
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
				map[string]string{"detail": "limit is not a number"})
			return
		}
		q.Limit = limit
	}
	page, err := s.directory.History(r.Context(), q)
	if err != nil {
		s.unavailable(w, r, "read the identity trail", err)
		return
	}
	events := make([]auditView, 0, len(page.Entries))
	for _, row := range page.Entries {
		events = append(events, auditView{
			ID: row.ID, Class: row.Class, ObjectKind: row.ObjectKind,
			ObjectID: row.ObjectID, Person: row.PersonID, Op: row.Op,
			Actor: row.Actor, ActorKind: row.ActorKind, Reason: row.Reason,
			Summary: row.Summary, At: row.At, Position: row.Version,
		})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"events":   events,
		"next":     page.Next,
		"position": page.At.String(),
	})
}
