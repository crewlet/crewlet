package iamapi

import (
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// `GET /iam/node-tokens`: THIS NODE'S TIER A TOKENS, as the directory sees them.
//
// # The gap it closes
//
// A Tier A token acts under its own login, `token:<id>`, and a service
// account's directory row holding that login is what binds it to a seat — a
// HUMAN seat, as every binding names one. The two are written in two
// places by two people — the label in a node's Tier A file, the row through
// this surface — so a label somebody mistyped on one of them names a login
// nobody holds, and the token goes on working as itself, unbound, while its
// operator believes it acts as a seat. `/iam/check` names a binding whose
// SEAT is gone, and nothing named a token whose ROW was never there: the label
// list is a fact about one node's configuration that no directory read can
// reach. So this answers it, per node, beside the rows it joins.
//
// # What the row holds, not a verdict on it
//
// A held row is reported as what it holds — its person, stage and seat — and
// never judged here. Whether that seat is one the company still holds is
// `/iam/check`'s finding about the same row (its login is `token:<id>`): two
// surfaces judging one binding are two answers that can disagree, and the
// report is the one place a dangling binding is said.
//
// # Labels, never values
//
// A token's value is a credential and this is a read; the label is how the
// value is named everywhere else, and it is all the join needs.
//
// # And which of them alone may change the company document
//
// A managed company document (ADR-0030) names the Tier A token ids that alone
// may change it (`api.auth.company_writers`), and every other credential — a
// person's session, a machine token, this node's other tokens — is refused
// `config_managed`. Which ids those are is the same kind of fact as the labels
// beside it: one node's Tier A, unreachable from any directory read. So the
// answer carries the list as this node states it (`company_writers`, empty
// where the document is not managed) and marks each of this node's own tokens
// it names (`company_writer`) — the very list the config surface enforces
// (internal/config's APIAuth.MayWriteCompany reads it), handed in whole rather
// than restated. It used to be its own question, `access`, beside the
// directory's reads, which answered it to a grant the directory never asked.
//
// # The same rule as the directory's own reads
//
// Who can reach this company, and as whom, is an administrator's question, so
// it is decided by [authz.ActionDirectoryRead] like every other listing here:
// admin. It names no person, so the self arm that rule carries has nothing to
// compare against, and nobody else reads it.

// NodeToken is one of this node's Tier A tokens, joined to the directory row
// holding its login.
type NodeToken struct {
	// ID is the token's label in this node's Tier A — never its value.
	ID string `json:"id"`

	// Login is the name the token acts under, `token:<id>`.
	Login string `json:"login"`

	// Row is what the directory holds under the login: `none` when nobody
	// does — the token acts as itself, bound to no seat — and `held` for a
	// row.
	Row TokenRow `json:"row"`

	// Person, Stage and Seat are the row's, where there is one: the seat
	// by its handle, as every other binding here is named, and absent for a
	// row that binds the token to none — which then writes under its own
	// login.
	Person string    `json:"person,omitempty"`
	Stage  iam.Stage `json:"stage,omitempty"`
	Seat   string    `json:"seat,omitempty"`

	// CompanyWriter marks a token the managed company document names as
	// one of its writers — the one credential, with the others listed in
	// `company_writers`, that may change it. Absent for every other token,
	// and for every token where the document is not managed.
	CompanyWriter bool `json:"company_writer,omitempty"`
}

// TokenRow is what the directory holds under a Tier A token's login.
type TokenRow string

// The two answers a login has.
const (
	TokenRowNone TokenRow = "none"
	TokenRowHeld TokenRow = "held"
)

// GetNodeTokens is `GET /iam/node-tokens`.
func (s *Service) GetNodeTokens(w http.ResponseWriter, r *http.Request) {
	labels := s.tokenIDs()
	tokens := make([]NodeToken, 0, len(labels))
	for _, id := range labels {
		token := NodeToken{ID: id, Login: iam.TokenLogin(id), Row: TokenRowNone}
		held, err := s.directory.PersonByLogin(r.Context(), token.Login)
		if err != nil {
			s.unavailable(w, r, "read a token's directory row", err)
			return
		}
		if held.ID != "" {
			token.Row, token.Person, token.Stage, token.Seat =
				TokenRowHeld, held.ID, held.Stage, held.Seat
		}
		token.CompanyWriter = slices.Contains(s.writers, id)
		tokens = append(tokens, token)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"tokens": tokens, "company_writers": s.writers,
	})
}

// tokenIDs is this node's Tier A labels, sorted.
func (s *Service) tokenIDs() []string {
	labels := slices.Clone(s.tokens())
	slices.Sort(labels)
	return labels
}
