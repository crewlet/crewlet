package engine

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
)

// codeSigner names an engine-built commit (host.Signer, ADR-0062) from the
// CURRENT epoch — read per write, so a revision that renames a seat or moves
// the identity domain signs from its activation on, with no restart, and read
// ONCE a signature, the seat's role and the domain off the same epoch:
//
//   - the AUTHOR is the acting principal's seat — an agent's, or the human
//     seat a person's credential is bound to, since a person writes only
//     through a credential bound to one (code.EngineWrite.Validate) and one
//     person holds many credentials (iam.ActorFor) — named by its role's name
//     and its role's email address AS WRITTEN in the company document. A
//     name or an address git would not write as given (gitproc's
//     IdentityRefusal), an address that is not one `<local>@<domain>` of at
//     most 254 bytes, and a `${VAR}` reference — which names no address
//     until something resolves it — fall back to the seat's handle and to
//     `<handle>@<code.identity_domain>`. NEVER org.Role.ResolvedEmail: it
//     reads THIS node's environment, which a retry on another git node need
//     not share — two nodes would build two commits of one write — and an
//     address a company kept out of its document would be written into
//     history for good.
//   - the COMMITTER is the engine, `Crewlet <engine@<code.identity_domain>>`:
//     a CONVENTION that marks the commits the host built in a cooperating
//     client's history, never an authentication — any git client can write
//     it, and only the push row's `via engine` and its principal say who
//     built a commit.
//
// A node with no epoch, or whose chart holds no such seat, signs nothing:
// code.ErrNotServing, about this node, so the write runs nothing here and the
// walk asks a git node serving a chart that holds it.
type codeSigner struct{ e *Engine }

// The engine's committer name and the local part of its address.
const (
	codeCommitterName  = "Crewlet"
	codeCommitterLocal = "engine"
)

// maxIdentityEmailBytes is the most an address a commit names may be: RFC
// 5321's 254 bytes, past which no mail system takes it.
const maxIdentityEmailBytes = 254

func (s codeSigner) Sign(p code.Principal) (host.Signature, error) {
	c := s.e.Company()
	if c == nil || c.Org == nil || c.Config == nil {
		return host.Signature{}, fmt.Errorf("%w: this node holds no configuration to sign a commit with",
			code.ErrNotServing)
	}
	domain := codeInForceAt(c).IdentityDomainResolved()
	role := c.Org.SeatByHandle(p.Seat)
	if p.Seat == "" || role == nil {
		return host.Signature{}, fmt.Errorf("%w: the chart this node serves holds no seat %q to sign the "+
			"commit as", code.ErrNotServing, p.Seat)
	}
	name := role.Name
	if gitproc.IdentityRefusal(name) != "" {
		name = p.Seat
	}
	email := role.Email
	if !identityEmail(email) {
		email = p.Seat + "@" + domain
	}
	return host.Signature{AuthorName: name, AuthorEmail: email, CommitterName: codeCommitterName,
		CommitterEmail: codeCommitterLocal + "@" + domain}, nil
}

// identityEmail reports an address a commit may name as it is written: one
// `@` between a local part and a domain, at most maxIdentityEmailBytes, and a
// part git writes as given. A `${VAR}` reference has no `@`, and is no
// address.
func identityEmail(s string) bool {
	local, domain, found := strings.Cut(s, "@")
	return found && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		len(s) <= maxIdentityEmailBytes && gitproc.IdentityRefusal(s) == ""
}
