package setupapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
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
// Test connection with it; cancel drops the staged one. The engine never signs
// with an authority it has not promoted.

// The rotation's steps.
const (
	rotationStage   = "stage"
	rotationPromote = "promote"
	rotationCancel  = "cancel"
)

type rotationRequest struct {
	Action string `json:"action"`
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
	current := s.heldAuthority(prov.CertificateAuthority)
	staged := strings.TrimSpace(prov.NextCertificateAuthority) != ""
	conflict := func(detail string) {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{"detail": detail})
	}
	switch req.Action {
	case rotationStage:
		if staged {
			conflict("a new certificate authority is already staged; promote it or cancel it first")
			return
		}
		value, err := setup.GenerateSSHCertificateAuthority()
		var next *setup.SSHCertificateAuthority
		if err == nil {
			next, err = setup.ParseSSHCertificateAuthority(value)
		}
		if err == nil {
			err = s.secrets.Set(r.Context(), sshint.NextCertificateAuthoritySecret, value,
				authorOf(attributionOf(r)), secrets.SourceSetup, s.now())
		}
		if err != nil {
			s.refuse(w, r, err, setup.Result{})
			return
		}
		s.writeRotation(w, r, fc.revision, "stage a new SSH certificate authority",
			"${"+sshint.NextCertificateAuthoritySecret+"}", map[string]any{"next_ca_public_key": next.PublicLine()})
	case rotationPromote:
		next := s.heldAuthority(prov.NextCertificateAuthority)
		name, isRef := envref.Whole(strings.TrimSpace(prov.CertificateAuthority))
		nextName, _ := envref.Whole(strings.TrimSpace(prov.NextCertificateAuthority))
		nextValue, held := s.resolve(nextName)
		switch {
		case next == nil || !held:
			conflict("no new certificate authority is staged, or the staged one does not resolve; stage one first")
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
			passed := false
			for _, t := range tests {
				passed = passed || (t.Host == h.ID && t.Authority == next.Fingerprint() && t.OK)
			}
			if !passed {
				untested = append(untested, h.ID)
			}
		}
		if len(untested) > 0 {
			conflict("promote waits until every server has passed Test connection with the staged " +
				"authority, and " + words.And(untested) + " has not; run each server's setup again, then test it " +
				"with the staged authority")
			return
		}
		// THE NEW KEY REPLACES THE OLD ONE, sealed under the current name,
		// before the staged reference is cleared: a promote that stops
		// between the two signs with the new authority, which every server
		// trusts, and runs again to the same end.
		if err := s.secrets.Set(r.Context(), name, nextValue, authorOf(attributionOf(r)), secrets.SourceSetup,
			s.now()); err != nil {
			s.refuse(w, r, err, setup.Result{})
			return
		}
		s.writeRotation(w, r, fc.revision, "promote the staged SSH certificate authority", nil, map[string]any{
			"ca_public_key": next.PublicLine(), "retired_ca_public_key": current.PublicLine(),
			"hint": "run each server's setup again, so it stops trusting the retired line"})
	case rotationCancel:
		if !staged {
			conflict("no new certificate authority is staged")
			return
		}
		s.writeRotation(w, r, fc.revision, "cancel the staged SSH certificate authority", nil, nil)
	default:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"field": "action", "detail": "action is stage, promote or cancel"})
	}
}

// writeRotation writes the staged reference, or clears it where ref is nil, as
// one revision on base, and answers with extra beside the revision.
func (s *Service) writeRotation(w http.ResponseWriter, r *http.Request, base, summary string, ref any,
	extra map[string]any) {
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
	for k, v := range extra {
		out[k] = v
	}
	httpjson.Write(w, http.StatusOK, out)
}
