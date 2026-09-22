package chartapi

import (
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/chart"
)

// A RENAME IS ITS OWN GESTURE, and the route says so rather than hiding it
// inside a content write.
//
// internal/chart makes the same separation and for a reason a surface
// inherits: one record has one subject, and a record carrying both an address
// change and a content change could arbitrate only one of them. What this
// surface adds is the direction — the route names the address the object
// answers to TODAY, and the body names what it should answer to next, because
// a caller that had to address the new key would be addressing something that
// does not exist yet.
//
// THE FORMER ADDRESS GOES ON RESOLVING. A key is not an identity: the
// object's row is, so the old address keeps resolving until something else
// claims it, and a `manages:` entry somebody wrote last year still finds the
// seat it named. That is why the read views carry `former_keys` and
// `former_handles` — a client rendering a stale reference can say why it
// still works rather than reporting it broken.

// renameBody is what a rename states.
type renameBody struct {
	// To is the address this object should answer to. The one this route
	// was addressed by becomes a former address.
	To string `json:"to"`
}

// renameUnit and renameSeat publish one address change each.
func (s *Service) renameUnit(w http.ResponseWriter, r *http.Request) {
	s.rename(w, r, chart.KindUnit, r.PathValue("key"))
}

func (s *Service) renameSeat(w http.ResponseWriter, r *http.Request) {
	s.rename(w, r, chart.KindSeat, r.PathValue("handle"))
}

// rename is both routes' body.
func (s *Service) rename(w http.ResponseWriter, r *http.Request,
	kind chart.ObjectKind, former string) {

	body, ok := readBody[renameBody](w, r)
	if !ok {
		return
	}
	to := chart.NormalizeKey(body.To)
	if to == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "this rename names no new address: " +
				"send {\"to\": \"...\"}"})
		return
	}
	// THE SAME ADDRESS IS REFUSED HERE rather than published and refused
	// there, because it is the one case the caller can fix from the message
	// alone and the one the domain's own wording is least clear about: a
	// normalised key that happens to equal the current one is what a client
	// sends when its form did nothing.
	if to == chart.NormalizeKey(former) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": strconv.Quote(to) +
				" is the address this object already answers to"})
		return
	}
	result, err := s.writerFor(r).WriteRekey(r.Context(), s.opID(r),
		chart.ObjectRef{Kind: kind, ID: to}, former)
	s.answerWrite(w, result, err)
}
