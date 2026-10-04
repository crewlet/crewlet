package iamapi

import (
	"errors"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// `GET /iam/node-tokens`: THIS NODE'S TIER A TOKENS, as the directory sees them.
//
// # The gap it closes
//
// A Tier A token acts under its own login, `token:<id>`, and a directory row
// holding that login is what binds it to a seat. The two are written in two
// places by two people — the label in a node's Tier A file, the row through
// this surface — so a label somebody mistyped on one of them names a login
// nobody holds, and the token goes on working as itself, unbound, while its
// operator believes it acts as a seat. `/iam/check` names a binding whose
// SEAT is gone, and nothing named a token whose ROW was never there: the label
// list is a fact about one node's configuration that no directory read can
// reach. So this answers it, per node, beside the rows it joins.
//
// # Labels, never values
//
// A token's value is a credential and this is a read; the label is how the
// value is named everywhere else, and it is all the join needs.
//
// # The same grant as the directory's own reads
//
// Who can reach this company, and as whom, is the audit question as much as
// the administrator's, so it is decided by [authz.ActionDirectoryRead] like
// every other listing here: `people:manage` or `audit:read`.

// NodeToken is one of this node's Tier A tokens, joined to the directory row
// holding its login.
type NodeToken struct {
	// ID is the token's label in this node's Tier A — never its value.
	ID string `json:"id"`

	// Login is the name the token acts under, `token:<id>`.
	Login string `json:"login"`

	// Row is what the directory holds under the login: `none` when nobody
	// does — the token acts as itself, bound to no seat — `reserved` for an
	// enrolment that stopped after claiming it, `held` for a row.
	Row TokenRow `json:"row"`

	// Person, Stage and Seat are the row's, where there is one: the seat
	// by its handle, as every other binding here is named.
	Person string    `json:"person,omitempty"`
	Stage  iam.Stage `json:"stage,omitempty"`
	Seat   string    `json:"seat,omitempty"`

	// Binding is whether the seat the row names is one the chart still
	// holds: `bound`, `dangling` with Detail saying why, `unbound` for a
	// row naming no seat, or `unknown` where this node could not tell —
	// the same judgement `/iam/check` reports a dangling binding from.
	Binding TokenBinding `json:"binding"`
	Detail  string       `json:"detail,omitempty"`
}

// TokenRow is what the directory holds under a Tier A token's login.
type TokenRow string

// The three answers a login has.
const (
	TokenRowNone     TokenRow = "none"
	TokenRowReserved TokenRow = "reserved"
	TokenRowHeld     TokenRow = "held"
)

// TokenBinding is whether a token's row names a seat the chart still holds.
type TokenBinding string

// The four answers, the last of which is this node being unable to say.
const (
	TokenBound    TokenBinding = "bound"
	TokenUnbound  TokenBinding = "unbound"
	TokenDangling TokenBinding = "dangling"
	TokenUnknown  TokenBinding = "unknown"
)

// GetNodeTokens is `GET /iam/node-tokens`.
func (s *Service) GetNodeTokens(w http.ResponseWriter, r *http.Request) {
	labels := s.tokenIDs()
	tokens := make([]NodeToken, 0, len(labels))
	for _, id := range labels {
		token := NodeToken{ID: id, Login: iam.TokenLogin(id), Binding: TokenUnbound}
		held, err := s.directory.PersonByLogin(r.Context(), token.Login)
		if err != nil {
			s.unavailable(w, r, "read a token's directory row", err)
			return
		}
		switch {
		case held.Reserved:
			token.Row = TokenRowReserved
		case held.ID == "":
			token.Row = TokenRowNone
		default:
			token.Row, token.Person, token.Stage, token.Seat =
				TokenRowHeld, held.ID, held.Stage, held.Seat
			if token.Binding, token.Detail, err = s.tokenBinding(r, held); err != nil {
				s.unavailable(w, r, "read a token's directory row", err)
				return
			}
		}
		tokens = append(tokens, token)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// tokenBinding judges a held row's seat by the same seam `/iam/check` asks.
func (s *Service) tokenBinding(r *http.Request, held iamdomain.Sighting) (
	TokenBinding, string, error) {

	if held.Seat == "" {
		return TokenUnbound, "", nil
	}
	if s.bindings == nil {
		return TokenUnknown, "", nil
	}
	row, err := s.directory.Person(r.Context(), held.ID)
	switch {
	case errors.Is(err, iamdomain.ErrNotFound):
		// THE ROW WENT between the two reads: what the login names now is
		// nobody's, which the next read will say.
		return TokenUnknown, "", nil
	case err != nil:
		return "", "", err
	}
	dangling, detail, err := s.bindings(r.Context(), row)
	switch {
	case err != nil:
		log.DebugContext(r.Context(), "api_iam_token_binding_unknown",
			"person", row.ID, "error", err)
		return TokenUnknown, "", nil
	case dangling:
		return TokenDangling, detail, nil
	}
	return TokenBound, "", nil
}

// tokenIDs is this node's Tier A labels, sorted.
func (s *Service) tokenIDs() []string {
	labels := slices.Clone(s.tokens())
	slices.Sort(labels)
	return labels
}
