package chartapi

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/authz"
)

// Routes registers the surface on a mux.
//
// # Every route carries its own policy, stated where it is mounted
//
// Through [authz.Router], which is the only reader of the matched pattern: a
// middleware runs BEFORE the mux matches, so anything it decided from would
// be a second router that has to agree with this one about which handler
// runs. A route mounted here with no policy, or naming a verb the authority
// table has no rule for, is refused at MOUNT — which is why this returns an
// error rather than panicking or logging: the caller is a route table that
// can name every mistake at once.
//
// THE WRITES RE-ASK, and that is not a second gate. The router decides the
// route's own verb; a content write then asks again with the verb its BODY
// turned out to need, because whether a payload carries the runtime half is
// not visible from the pattern. Both questions go to one function over one
// table, which is what stops them answering differently.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.Guard(s.guard))
	var failures []error
	mount := func(pattern string, p authz.Policy, h http.HandlerFunc) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	// at is a policy with no object: every read here and every operator
	// verb, whose rules decide from a capability rather than a relation.
	at := func(a authz.Action) authz.Policy { return authz.Policy{Action: a} }
	mount("GET /chart", at(authz.ActionChartRead), s.getChart)
	mount("GET /chart/units", at(authz.ActionChartRead), s.getUnits)
	mount("GET /chart/units/{key}", at(authz.ActionChartRead), s.getUnit)
	mount("GET /chart/seats", at(authz.ActionChartRead), s.getSeats)
	mount("GET /chart/seats/{handle}", at(authz.ActionChartRead), s.getSeat)
	mount("GET /chart/history", at(authz.ActionChartRead), s.getHistory)
	// THE PATTERN'S VERB IS THE PUBLIC ONE, and a body carrying the
	// runtime half re-asks for the operator verb before it writes. The
	// other order — mount at the operator verb and relax for a public
	// body — would refuse a lead editing their own team at the door,
	// before anything had looked at what they sent.
	//
	// EACH NAMES THE OBJECT ITS PATTERN CAN NAME, and they are different
	// objects: a unit is decided by who leads THE UNIT and a seat by who
	// leads THE SEAT. Both relations live in the chart and neither
	// substitutes for the other — a unit lead leads the seats inside it,
	// but a seat's manager need not lead the unit it sits in.
	mount("PATCH /chart/units/{key}", authz.Policy{
		Action: authz.ActionChartContent,
		Object: func(r *http.Request) authz.Object {
			return authz.Object{Kind: authz.KindUnit, Container: r.PathValue("key")}
		},
	}, s.patchUnit)
	mount("PATCH /chart/seats/{handle}", authz.Policy{
		Action: authz.ActionChartContent,
		Object: func(r *http.Request) authz.Object {
			return authz.Object{Kind: authz.KindPerson, Owner: r.PathValue("handle")}
		},
	}, s.patchSeat)
	mount("POST /chart/batch", at(authz.ActionChartStructure), s.postBatch)
	mount("POST /chart/units/{key}/rename", at(authz.ActionChartRename), s.renameUnit)
	mount("POST /chart/seats/{handle}/rename", at(authz.ActionChartRename), s.renameSeat)
	mount("POST /chart/import", at(authz.ActionChartImport), s.postImport)
	// THE IMPORT LEDGER IS READ UNDER ITS OWN VERB: the importer's grant,
	// and no step-up — a client polling whether its import landed is
	// reading, and a poll that began inside the hour must not start
	// failing when the hour ends.
	mount("GET /chart/imports", at(authz.ActionChartImportRead), s.getImports)
	mount("GET /chart/imports/{revision}", at(authz.ActionChartImportRead), s.getImport)
	mount("GET /chart/check", at(authz.ActionChartRead), s.getCheck)
	// THE WHOLE AUTHORED DOCUMENT, at the grant that reads the company's
	// configuration rather than the one that reads its chart. It is a
	// ROUND TRIP rather than a view: whole or useless, so there is no
	// stripped posture here and nothing to get wrong about one.
	mount("GET /company/export", at(authz.ActionChartReadRuntime), s.getExport)
	return errors.Join(failures...)
}
