package engine

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/host"
)

// codeSigner names an engine-built commit (host.Signer, ADR-0092) from the
// CURRENT epoch, read once a signature. The AUTHOR is the acting seat (a person
// writes only through a credential bound to their human seat), by its role's
// name and email AS WRITTEN in the document, else by [authorOf]'s fallback;
// NEVER org.Role.ResolvedEmail, which reads this node's environment (a retry
// elsewhere would build another commit) and would put a kept-out address in
// history for good. The COMMITTER is [engineCommitter], a CONVENTION marking
// host-built commits (Committer holds a box's pack to none the repository
// lacks; history reads mark engine_committed by gitlet.EngineCommitterName and
// EngineCommitterLocal), never an authentication: only the push row's `via
// engine` says who built one. No epoch or seat signs nothing: ErrNotServing.
type codeSigner struct{ e *Engine }

// maxIdentityEmailBytes is the most an address a commit names may be: RFC
// 5321's 254 bytes, past which no mail system takes it.
const maxIdentityEmailBytes = 254

// engineCommitter is the address the engine commits as on the identity
// domain domain: the ONE spelling Sign and Committer share.
func engineCommitter(domain string) string { return gitlet.EngineCommitterLocal + "@" + domain }

func (s codeSigner) Sign(p gitlet.Principal) (host.Signature, error) {
	c := s.e.Company()
	if c == nil || c.Org == nil || c.Config == nil {
		return host.Signature{}, fmt.Errorf("%w: this node holds no configuration to sign a commit with",
			gitlet.ErrNotServing)
	}
	domain := codeInForceAt(c).IdentityDomainResolved()
	role := c.Org.SeatByHandle(p.Seat)
	if p.Seat == "" || role == nil {
		return host.Signature{}, fmt.Errorf("%w: the chart this node serves holds no seat %q to sign the "+
			"commit as", gitlet.ErrNotServing, p.Seat)
	}
	name, email := authorOf(p.Seat, role.Name, role.Email, domain)
	return host.Signature{AuthorName: name, AuthorEmail: email, CommitterName: gitlet.EngineCommitterName,
		CommitterEmail: engineCommitter(domain)}, nil
}

// Author is how a commit names seat handle as its author where the engine did
// not act as it (a squash's owner, its other authors' trailers), as Sign does.
// A seat the chart no longer holds is named by its handle at the identity
// domain: refusing it would leave a change it opened unmergeable by squash. A
// node with no epoch names nobody: gitlet.ErrNotServing.
func (s codeSigner) Author(handle string) (string, string, error) {
	c := s.e.Company()
	if c == nil || c.Org == nil || c.Config == nil {
		return "", "", fmt.Errorf("%w: this node holds no configuration to name a commit's author by",
			gitlet.ErrNotServing)
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

// Committer is the address Sign names every committer by, off the CURRENT
// epoch: once the identity domain moves, a box's push is held to the new
// address, and an engine commit under the old one is no longer matched (the
// repository holds it anyway). A node with no epoch names none:
// gitlet.ErrNotServing.
func (s codeSigner) Committer() (string, error) {
	c := s.e.Company()
	if c == nil || c.Config == nil {
		return "", fmt.Errorf("%w: this node holds no configuration to name the engine's committer by",
			gitlet.ErrNotServing)
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
