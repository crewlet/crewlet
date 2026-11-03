package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE TEST BUTTON SAYS WHAT TO DO: an input it cannot check names what is
// missing, a stack as rendered works, and an older or edited stack is refused
// with the CloudShell block as its fix.
func TestTheProbeSaysWhatToDo(t *testing.T) {
	t.Parallel()
	prov := *testProvisioning(t)
	for name, mutate := range map[string]func(*config.AWSProvisioning){
		"no certificate": func(p *config.AWSProvisioning) { p.CertificateAuthority = "" },
		"no output":      func(p *config.AWSProvisioning) { p.Connection = "" },
		"another slug":   func(p *config.AWSProvisioning) { p.Slug = "other" },
	} {
		p := prov
		mutate(&p)
		if _, err := probeCheck(context.Background(), p, nil); !errors.Is(err, setup.ErrUnusableInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	regions := prov.RegionsOrDefault()
	none := func(string, string, *obj, *string) {}
	for _, tc := range []struct {
		name string
		edit func(action, name string, doc *obj, extra *string)
		want setup.VerifyOutcome
		says string
	}{
		{"as rendered", none, setup.VerifyWorks, "every Crewlet policy and role matches"},
		{"an older stack", func(a, _ string, _ *obj, x *string) {
			if a == "GetRole" {
				*x = ""
			}
		}, setup.VerifyRefused, "version untagged"},
		{"a widened level", func(a, n string, d *obj, _ *string) {
			if a == "GetPolicyVersion" && n == levelOperate {
				*d = doc(stmt("CrewletOperate", "Allow", []string{"*"}))
			}
		}, setup.VerifyRefused, "crewlet-acme-level-operate"},
	} {
		f := fakeStack(t, regions, tc.edit)
		got, err := probeCheck(context.Background(), prov, func(*Connection) *Client { return f.client(t) })
		if err != nil || got.Outcome != tc.want || !strings.Contains(got.Detail, tc.says) {
			t.Errorf("%s: %+v %v", tc.name, got, err)
		}
		if tc.want == setup.VerifyRefused && !strings.Contains(got.Detail, "CloudShell block") {
			t.Errorf("%s: the refusal names no fix: %s", tc.name, got.Detail)
		}
	}
}

// THE FORM ASKS IN ORDER: the stack output is required only once the
// certificate authority is held, which is minted and never pasted.
func TestTheConnectFormAsksForTheOutputOnceTheCAIsHeld(t *testing.T) {
	t.Parallel()
	resolve := func(ref string) (string, bool) { return "bundle", ref != "" }
	field := func(reqs []setup.Requirement, name string) setup.Requirement {
		for _, r := range reqs {
			if r.Field == name {
				return r
			}
		}
		t.Fatalf("no field %s", name)
		return setup.Requirement{}
	}
	before := Requirements(nil, "Acme Robotics, Inc.", resolve)
	if field(before, "connection").Required || field(before, "slug").Default != "acme-robotics-inc" {
		t.Errorf("before the mint: %+v %+v", field(before, "connection"), field(before, "slug"))
	}
	ca := field(before, "certificate_authority")
	if !ca.Mintable || ca.Shape != setup.ShapeCertificateAuthority || ca.SecretName != CertificateAuthoritySecret {
		t.Errorf("the certificate authority is not minted: %+v", ca)
	}
	after := Requirements(&config.AWS{Enabled: true, Provisioning: &config.AWSProvisioning{
		CertificateAuthority: "${" + CertificateAuthoritySecret + "}"}}, "Acme", resolve)
	if !field(after, "connection").Required {
		t.Error("the stack output is not required once the certificate authority is held")
	}
	if got := len(field(after, "home_region").Choices); got != len(homeRegions) {
		t.Errorf("%d home Regions offered", got)
	}
}

// WHAT SETUP MINTS IS A CERTIFICATE AUTHORITY THIS PACKAGE SIGNS WITH, and its
// certificate is all the stack is rendered with.
func TestTheMintedCertificateAuthoritySignsLeaves(t *testing.T) {
	t.Parallel()
	bundle, err := setup.ShapeCertificateAuthority.Mint(nil)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ParseCA(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || !cnPattern.MatchString(ca.Cert.Subject.CommonName) || hasPEMPrivateKey(ca.CertificatePEM()) {
		t.Fatalf("%+v", ca.Cert.Subject)
	}
	leaf, _, err := ca.Leaf(seatCN("acme", SeatSlug("sre")), ca.Cert.NotBefore.Add(time.Hour), LeafECDSA)
	if err != nil || leaf.CheckSignatureFrom(ca.Cert) != nil {
		t.Fatalf("a leaf of the minted authority: %v", err)
	}
}
