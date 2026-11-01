package engine

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
)

// codeSigner names an engine-built commit (host.Signer, ADR-0085) from the
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
//     built a commit. A coding box's pack may hold no commit under it that
//     the repository does not already hold (Committer, the same address,
//     spelled once: engineCommitter).
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

// engineCommitter is the address the engine commits as on the identity
// domain domain: the ONE spelling of it, which every commit Sign names and
// every box push Committer holds carry alike.
func engineCommitter(domain string) string { return codeCommitterLocal + "@" + domain }

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
	name, email := authorOf(p.Seat, role.Name, role.Email, domain)
	return host.Signature{AuthorName: name, AuthorEmail: email, CommitterName: codeCommitterName,
		CommitterEmail: engineCommitter(domain)}, nil
}

// Author is how a commit names seat handle as its author where the engine did
// not act as it — a squash's owner, its other authors' trailers (U43 a): as
// Sign names an acting seat, from the CURRENT epoch's chart, and for a seat
// that chart no longer holds, by its handle at the identity domain — a seat
// that left still wrote what it wrote, and refusing its name would leave a
// change it opened unmergeable by squash. A node with no epoch names nobody:
// code.ErrNotServing.
func (s codeSigner) Author(handle string) (string, string, error) {
	c := s.e.Company()
	if c == nil || c.Org == nil || c.Config == nil {
		return "", "", fmt.Errorf("%w: this node holds no configuration to name a commit's author by",
			code.ErrNotServing)
	}
	domain := codeInForceAt(c).IdentityDomainResolved()
	if role := c.Org.SeatByHandle(handle); role != nil {
		name, email := authorOf(handle, role.Name, role.Email, domain)
		return name, email, nil
	}
	name, email := authorOf(handle, "", "", domain)
	return name, email, nil
}

// authorOf is the name and address a commit names seat handle by: its role's
// name where git writes it as given, else the handle; its role's address
// where it is one, else `<handle>@<domain>`.
func authorOf(handle, roleName, roleEmail, domain string) (string, string) {
	name := roleName
	if name == "" || gitproc.IdentityRefusal(name) != "" {
		name = handle
	}
	email := roleEmail
	if !identityEmail(email) {
		email = handle + "@" + domain
	}
	return name, email
}

// Committer is the address Sign names every commit's committer by, off the
// CURRENT epoch as Sign reads it — so a revision that moves the identity
// domain holds a box's push to the new address from its activation on, and a
// commit the engine built under the old one, which a box brings back, is no
// longer matched (the repository holds it in any case). A node with no epoch
// names none: code.ErrNotServing.
func (s codeSigner) Committer() (string, error) {
	c := s.e.Company()
	if c == nil || c.Config == nil {
		return "", fmt.Errorf("%w: this node holds no configuration to name the engine's committer by",
			code.ErrNotServing)
	}
	return engineCommitter(codeInForceAt(c).IdentityDomainResolved()), nil
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
