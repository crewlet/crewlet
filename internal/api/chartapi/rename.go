package chartapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/chart"
)

// A RENAME IS ITS OWN ROUTE, and the route says so rather than hiding it
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
// IT IS A ONE-OPERATION STRUCTURAL BATCH, published exactly as `POST
// /chart/batch` would publish `{"kind":"rename"}`: a rename arbitrates on the
// tree's one subject with every create and every removal, so a create of the
// same address is decided against it and never the other way round. The
// domain's refusals — an address that is taken, reserved, removed, somebody's
// identity or the one the object already answers to — come back through the
// same answer every write here gives, so this route checks nothing a batch
// would not.
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
	op, ok := s.operation(w, r, "chart-rename", body)
	if !ok {
		return
	}
	result, err := s.writerFor(r).WriteBatch(r.Context(), op.id, chart.Batch{
		Operations: []chart.Operation{{
			Kind:   chart.OpRename,
			Object: chart.ObjectRef{Kind: kind, ID: former},
			To:     body.To,
		}},
	})
	s.answerWrite(w, op, result, err)
}
