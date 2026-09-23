package chartapi

import (
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/chart"
)

// IMPORTING A COMPANY'S AUTHORED STRUCTURE, and the ledger that makes doing it
// twice a no-op.
//
// # What an import is, and why it is not a batch
//
// A batch is somebody's gesture — hire this person, move that team — and it
// states only what changes. An import is a REVISION's complete authored
// placement: this is where every object sits, according to the document
// version it came from. The domain publishes them as different records so
// that an operator reading the log can tell "somebody moved a seat" from "a
// config revision rewrote the chart" without guessing from the size of a
// payload.
//
// # The ledger, and why a re-import is routine rather than exceptional
//
// Re-activating an UNCHANGED revision is the credential-rotation gesture, so
// it happens on a schedule in healthy companies. Keyed on the revision, the
// import ledger makes the second landing a no-op every node reaches the same
// way; without it that gesture would rewrite every row in the chart and wake
// everybody a second time. That is why the check is at the APPLY and not in
// the decide, and why this route publishes without asking first: asking would
// be a read whose answer is stale by the time the record lands.
//
// # What it deliberately does not carry
//
// Content. A chart of five hundred seats at this domain's prose bound is
// megabytes, and an external NATS cluster's default max_payload is one
// mebibyte — so an import that carried content would be refused by the broker
// on exactly the companies large enough to need it. Content travels as one
// content write per object, on that object's own subject, which is also what
// lets two of them be written concurrently. `crewlet config import` is what
// sequences the two halves.

// importBody is one revision's complete authored placement.
type importBody struct {
	// Revision is the company configuration revision this structure was
	// read from, and it is the ledger's key. Required: an import with no
	// revision is one nothing can recognise a second landing of.
	Revision string `json:"revision"`

	Edges []importEdge `json:"edges"`
}

type importEdge struct {
	Object struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"object"`

	// Parent is the unit key this object sits under. EMPTY IS THE ORG
	// ROOT, which is a real placement rather than an unstated one.
	Parent string `json:"parent,omitempty"`

	// Lead is the handle leading a unit, and empty is a unit whose lead
	// comes from an ancestor — also a real state, and the common one.
	Lead string `json:"lead,omitempty"`
}

// postImport publishes one revision's authored structure.
func (s *Service) postImport(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[importBody](w, r)
	if !ok {
		return
	}
	if body.Revision == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "this import names no revision, and the " +
				"revision is what the ledger is keyed on — without it a second " +
				"landing of the same structure would rewrite every row in the " +
				"chart and wake everybody again"})
		return
	}
	if len(body.Edges) == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "this import states no placements. An " +
				"import is a revision's COMPLETE authored structure, so an " +
				"empty one is not 'change nothing' — it is a company with no " +
				"chart, which is refused rather than guessed at"})
		return
	}
	// THE FLEET FIRST, before anything is published. An import rewrites
	// every placement in the chart in one record, and every node applies
	// it — including one running an older build, under its own reading of
	// what a placement means. See [Fleet].
	if !s.fleetReady(w, r) {
		return
	}
	edges := make([]chart.Edge, 0, len(body.Edges))
	for _, e := range body.Edges {
		edges = append(edges, chart.Edge{
			Object: chart.ObjectRef{
				Kind: chart.ObjectKind(e.Object.Kind), ID: e.Object.ID,
			},
			Parent: e.Parent, Lead: e.Lead,
		})
	}
	result, err := s.writerFor(r).WriteImport(r.Context(), s.opID(r),
		body.Revision, edges)
	s.answerWrite(w, result, err)
}

// getImport answers whether one revision's structure has landed, and where.
//
// A REVISION THE CHART HAS NEVER SEEN IS 404, not an empty body: "this
// company is not running that structure" and "I could not tell you" are
// different answers, and a client polling an import needs the first one to be
// unambiguous.
func (s *Service) getImport(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, found, answer, err := s.reader.Import(r.Context(),
		r.PathValue("revision"), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	if !found {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "this chart has never applied revision " +
				strconv.Quote(r.PathValue("revision"))})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"import": viewOfImport(got), "answer": answerOf(answer),
	})
}

// getImports answers the ledger, newest first.
func (s *Service) getImports(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, answer, err := s.reader.Imports(r.Context(), limitOf(r), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	out := make([]map[string]any, 0, len(got))
	for _, one := range got {
		out = append(out, viewOfImport(one))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"imports": out, "answer": answerOf(answer),
	})
}

// viewOfImport renders one ledger row.
//
// THE POSITION AS A STRING, like every other position this surface writes: it
// is a composed (generation, sequence) pair and a JSON number would round the
// large half of it on a client that parses into a float.
func viewOfImport(one chart.Import) map[string]any {
	return map[string]any{
		"revision": one.Revision,
		"position": one.Position.String(),
		"at":       one.At,
		"by":       one.By,
		"objects":  one.Objects,
		// RecordID is what ties a ledger row to the record in the log,
		// which is the only way to ask the log what actually landed.
		"record_id": one.RecordID,
	}
}

// limitOf is how many rows a listing route was asked for, bounded by what the
// domain will serve.
//
// A MISSING OR UNREADABLE VALUE TAKES THE DOMAIN'S OWN BOUND rather than
// failing: the parameter is a convenience, and the reader clamps anything out
// of range anyway — so two places would be deciding one limit, and the
// stricter one would win silently.
func limitOf(r *http.Request) int {
	got, err := strconv.Atoi(r.URL.Query().Get(LimitParam))
	if err != nil {
		return chart.HistoryLimit
	}
	return got
}

// fleetReady answers the request when a rolling upgrade is still in progress.
//
// 409 RATHER THAN 503, because this is not a node that is busy: the fleet is
// in a state an import must not land in, and the remedy is to finish the
// upgrade rather than to retry. A node that could not TELL answers 503, which
// is the retryable one.
func (s *Service) fleetReady(w http.ResponseWriter, r *http.Request) bool {
	if s.fleet == nil {
		return true
	}
	reason, err := s.fleet.ImportReady(r.Context())
	switch {
	case err != nil:
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			map[string]string{"detail": err.Error()})
		return false
	case reason != "":
		// ITS OWN CODE, which the CLI and the fleet guide both name. It
		// used to be spelled into the DETAIL beside `bad_params`, where
		// the envelope's reserved `error` overwrote it — so every client
		// was told the parameters were bad, and nothing could branch on
		// the one refusal whose remedy is finishing an upgrade.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeFleetMixedVersion,
			map[string]string{"detail": reason})
		return false
	}
	return true
}
