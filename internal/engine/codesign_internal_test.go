package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// signerChart is a company whose seats state every kind of address a role may:
// none, one git would cut (`a<b@c`), a `${VAR}` reference whose variable this
// test sets, and a valid one — beside a role name git would cut.
const signerChart = `name: Acme
code:
  native: true
%s
roles:
  - name: No Address
    handle: plain
  - name: Odd Address
    handle: odd
    email: a<b@example.com
  - name: Referenced
    handle: referenced
    email: ${JANE_EMAIL}
  - name: Writer
    handle: writer
    email: writer@example.com
  - name: Ana <ops>
    handle: ana
  - name: Pat
    handle: pat
    kind: human
`

// THE SIGNER FALLS BACK TO THE IDENTITY DOMAIN for every seat whose role
// states no address a commit may name as written: none, one git would cut, and
// a `${VAR}` reference — read AS WRITTEN, never resolved through this node's
// environment, which a retry on another git node need not share, so the
// variable set here changes nothing — while a valid address is the author's
// own. A role name git would cut falls back to the handle. The committer is
// the engine at the identity domain, its default seats.invalid; a seat the
// chart does not hold, and a node holding no epoch, sign nothing, about this
// node.
//
// Mutation: read the role's ResolvedEmail — the referenced seat signs with the
// variable's value, red.
func TestTheSignerFallsBackToTheIdentityDomain(t *testing.T) {
	t.Setenv("JANE_EMAIL", "jane@example.com")
	for _, tc := range []struct {
		domain string // the code block's identity_domain line, "" for none
		want   string // the domain addresses fall back to
	}{
		{"", "seats.invalid"},
		{"  identity_domain: example.org", "example.org"},
	} {
		e := &Engine{}
		c, err := NewCompany(parseCompany(t, strings.Replace(signerChart, "%s", tc.domain, 1)))
		if err != nil {
			t.Fatalf("build the epoch: %v", err)
		}
		e.epoch.current.Store(c)
		s := codeSigner{e: e}
		for _, row := range []struct {
			seat       string
			kind       code.PrincipalKind
			name, mail string
		}{
			{"plain", code.PrincipalAgent, "No Address", "plain@" + tc.want},
			{"odd", code.PrincipalAgent, "Odd Address", "odd@" + tc.want},
			{"referenced", code.PrincipalAgent, "Referenced", "referenced@" + tc.want},
			{"writer", code.PrincipalAgent, "Writer", "writer@example.com"},
			{"ana", code.PrincipalAgent, "ana", "ana@" + tc.want},
			{"pat", code.PrincipalOperator, "Pat", "pat@" + tc.want},
		} {
			sig, err := s.Sign(code.Principal{Kind: row.kind, Seat: row.seat})
			if err != nil {
				t.Errorf("%s: %v", row.seat, err)
				continue
			}
			if sig.AuthorName != row.name || sig.AuthorEmail != row.mail {
				t.Errorf("%s signs as %s <%s>, want %s <%s>", row.seat, sig.AuthorName, sig.AuthorEmail, row.name,
					row.mail)
			}
			if sig.CommitterName != "Crewlet" || sig.CommitterEmail != "engine@"+tc.want {
				t.Errorf("%s's committer is %s <%s>, want Crewlet <engine@%s>", row.seat, sig.CommitterName,
					sig.CommitterEmail, tc.want)
			}
		}
		if _, err := s.Sign(code.Principal{Kind: code.PrincipalAgent, Seat: "nobody"}); !errors.Is(err,
			code.ErrNotServing) {
			t.Errorf("a seat the chart lacks signs: %v, want ErrNotServing", err)
		}
	}
	if _, err := (codeSigner{e: &Engine{}}).Sign(code.Principal{Kind: code.PrincipalAgent, Seat: "plain"}); !errors.Is(
		err, code.ErrNotServing) {
		t.Errorf("a node with no epoch signs: %v, want ErrNotServing", err)
	}
}
