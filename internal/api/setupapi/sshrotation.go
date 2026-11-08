package setupapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/words"
)

// The certificate authority's rotation. Stage mints a second authority, sealed
// beside the current one, which the rendered server setup trusts beside it;
// promote makes it the one the engine signs with, once every server passed
// Test connection with it as the server is now; cancel drops the staged one.
// The engine never signs with an authority it has not promoted, and no sealed
// key outlives its authority.

// The rotation's steps.
const (
	rotationStage   = "stage"
	rotationPromote = "promote"
	rotationCancel  = "cancel"
)

type rotationRequest struct {
	Action string `json:"action"`
}

// SealedRemover removes a sealed value: the fleet store's Unset, which a
// rotation uses to drop the staged authority's key.
type SealedRemover interface {
	Unset(ctx context.Context, name string) (bool, error)
}

// sshRotation serves POST /setup/integrations/ssh/rotation.
func (s *Service) sshRotation(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	var req rotationRequest
	if !readSSHBody(w, r, &req) {
		return
	}
	block := fc.company.Integrations.SSH
	prov := block.Provisioning
	staged := strings.TrimSpace(prov.NextCertificateAuthority) != ""
	conflict := func(detail string) {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{"detail": detail})
	}
	// NO KEY IS SEALED THAT COULD NOT BE REMOVED: a store with no removal
	// moves no authority at all.
	remover, removes := s.secrets.(SealedRemover)
	if !removes {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds, httpjson.Detail{
			"detail": "this node cannot remove a sealed value, so it moves no authority; use a node that serves the dashboard"})
		return
	}
	switch req.Action {
	case rotationStage:
		if staged {
			conflict("a new certificate authority is already staged; promote it or cancel it first")
			return
		}
		value, err := setup.GenerateSSHCertificateAuthority()
		if err == nil {
			err = s.secrets.Set(r.Context(), sshint.NextCertificateAuthoritySecret, value,
				authorOf(attributionOf(r)), secrets.SourceSetup, s.now())
		}
		if err != nil {
			s.refuse(w, r, err, setup.Result{})
			return
		}
		// THE ANSWER IS THE SEALED KEY'S, read back after the revision: of
		// two stages at once, the one whose revision lands answers the key
		// the store holds, whichever of them sealed it.
		s.writeRotation(w, r, fc.revision, "stage a new SSH certificate authority",
			"${"+sshint.NextCertificateAuthoritySecret+"}", func() map[string]any {
				next := s.heldAuthority("${" + sshint.NextCertificateAuthoritySecret + "}")
				if next == nil {
					return nil
				}
				return map[string]any{"next_ca_public_key": next.PublicLine()}
			})
	case rotationPromote:
		s.sshPromote(w, r, fc, remover, conflict)
	case rotationCancel:
		if !staged {
			conflict("no new certificate authority is staged")
			return
		}
		// THE KEY GOES BEFORE THE REFERENCE: a cancel that stops between
		// the two leaves a reference to nothing, which the engine never
		// signs with and a second cancel clears.
		if _, err := remover.Unset(r.Context(), sshint.NextCertificateAuthoritySecret); err != nil {
			s.refuse(w, r, err, setup.Result{})
			return
		}
		s.writeRotation(w, r, fc.revision, "cancel the staged SSH certificate authority", nil, nil)
	default:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"field": "action", "detail": "action is stage, promote or cancel"})
	}
}

// sshPromote makes the staged authority the current one, once every server
// passed Test connection with it as the server is now.
func (s *Service) sshPromote(w http.ResponseWriter, r *http.Request, fc fleetCompany, remover SealedRemover,
	conflict func(string)) {

	block := fc.company.Integrations.SSH
	prov := block.Provisioning
	current := s.heldAuthority(prov.CertificateAuthority)
	next := s.heldAuthority(prov.NextCertificateAuthority)
	name, isRef := envref.Whole(strings.TrimSpace(prov.CertificateAuthority))
	nextName, _ := envref.Whole(strings.TrimSpace(prov.NextCertificateAuthority))
	nextValue, held := s.resolve(nextName)
	switch {
	case next == nil || !held:
		conflict("no new certificate authority is staged, or the staged one does not resolve; stage one " +
			"first, or cancel the stale one")
		return
	case !isRef || current == nil:
		conflict("the current certificate authority is not a sealed value the engine holds; disconnect " +
			"SSH and connect it again")
		return
	}
	tests, err := s.sshTests.ListSSHTests(r.Context())
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	var untested []string
	for _, h := range block.Hosts {
		if test := latestTest(tests, h, next.Fingerprint()); test == nil || !test.OK {
			untested = append(untested, h.ID)
		}
	}
	if len(untested) > 0 {
		conflict("promote waits until every server has passed Test connection with the staged authority " +
			"as the server is now, and " + words.And(untested) + " has not; run each server's setup again, " +
			"then test it with the staged authority")
		return
	}
	retiring := current.Fingerprint() != next.Fingerprint()
	extra := map[string]any{"ca_public_key": next.PublicLine(),
		"hint": "run each server's setup again, so it stops trusting the retired line"}
	by := authorOf(attributionOf(r))
	// THE NEW KEY REPLACES THE OLD ONE under the current name, the retired
	// line is kept, and the staged copy goes, all before the reference is
	// cleared: a promote that stops part way signs with the new authority,
	// which every server trusts, and runs again to the same end.
	if retiring {
		extra["retired_ca_public_key"] = current.PublicLine()
		err = s.secrets.Set(r.Context(), sshint.RetiredCertificateAuthoritiesSecret,
			strings.Join(appendRetired(s.retiredAuthorities(), current.PublicLine()), "\n"), by,
			secrets.SourceSetup, s.now())
	}
	if err == nil {
		err = s.secrets.Set(r.Context(), name, nextValue, by, secrets.SourceSetup, s.now())
	}
	if err == nil {
		_, err = remover.Unset(r.Context(), nextName)
	}
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	s.writeRotation(w, r, fc.revision, "promote the staged SSH certificate authority", nil,
		func() map[string]any { return extra })
}

// latestTest is host's latest test with authority as the host is now, or nil.
func latestTest(tests []coord.SSHTest, h config.SSHHost, authority string) *coord.SSHTest {
	digest := sshint.HostDigest(h)
	for i := range tests {
		if tests[i].Host == h.ID && tests[i].Authority == authority && tests[i].HostDigest == digest {
			return &tests[i]
		}
	}
	return nil
}

// retiredAuthorities is the public lines a rotation retired, newest last.
func (s *Service) retiredAuthorities() []string {
	held, ok := s.resolve(sshint.RetiredCertificateAuthoritiesSecret)
	if !ok {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(held, "\n") {
		if pk, err := sshint.ParsePublicLine(strings.TrimSpace(line)); err == nil && pk != nil {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// appendRetired is lines with line last, once, and at most the newest few.
func appendRetired(lines []string, line string) []string {
	lines = slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return l == line })
	lines = append(lines, line)
	return lines[max(0, len(lines)-sshint.MaxRetiredAuthorities):]
}

// writeRotation writes the staged reference, or clears it where ref is nil, as
// one revision on base, and answers with what extra says beside the revision,
// asked once the revision landed.
func (s *Service) writeRotation(w http.ResponseWriter, r *http.Request, base, summary string, ref any,
	extra func() map[string]any) {
	patch, err := json.Marshal(map[string]any{"integrations": map[string]any{"ssh": map[string]any{
		"provisioning": map[string]any{"next_certificate_authority": ref}}}})
	if err != nil {
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	applied, err := s.config.Apply(r.Context(), configapi.ApplyRequest{
		Patch: patch, Summary: summary, By: attributionOf(r), Expect: base,
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	out := map[string]any{"key": integration.KindSSH, "revision_id": applied.RevisionID, "epoch": applied.Epoch}
	if extra != nil {
		for k, v := range extra() {
			out[k] = v
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}
