package appstoreconnect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE SEVEN READS TELL EVERY ROLE APART, from the operation table's roles
// alone: a table change that makes two roles answer alike fails here before a
// key is ever read as the wrong one.
func TestTheRoleProbeTellsEveryRoleApart(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, role := range probeRoles {
		permitted := make([]bool, len(probeOps))
		key := ""
		for i, id := range probeOps {
			op, ok := Catalog().Operation(id)
			if !ok || op.Method != "GET" {
				t.Fatalf("probe %s is not a read of the table", id)
			}
			permitted[i] = growth.Permits(op, []string{role})
			if permitted[i] {
				key += "1"
			} else {
				key += "0"
			}
		}
		if other, dup := seen[key]; dup {
			t.Fatalf("%s and %s answer the probe alike", role, other)
		}
		seen[key] = role
		if got, ok := roleOf(permitted); !ok || got != role {
			t.Fatalf("roleOf(%s's answers) = %q, %v", role, got, ok)
		}
	}
	alive, _ := Catalog().Operation(aliveOp)
	for _, role := range probeRoles {
		if !growth.Permits(alive, []string{role}) {
			t.Fatalf("%s may not make the alive read, so a narrow key would read as a dead one", role)
		}
	}
}

func testKey(t *testing.T) string {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// signRig is a product whose team main holds the company's Marketing, Finance
// and Admin keys, and whose seat growth is signed as signer says.
func signRig(t *testing.T, signer seataccess.Signer, err error) (*Product, *team) {
	t.Helper()
	sealed := testKey(t)
	p := New(Deps{
		Company: func() *config.Company { return &config.Company{} },
		Resolve: func(string) string { return sealed },
		Identity: func(context.Context, string) (seataccess.Signer, error) {
			return signer, err
		},
	})
	tm := &team{id: "main", slots: []string{"marketing", "finance", "admin"},
		client: NewClient("https://example.invalid", "issuer", nil, nil)}
	return p, tm
}

func own(role, team string) seataccess.Signer {
	return seataccess.Signer{Own: true, Record: &seataccess.IdentityRecord{ID: "7KQ4M2ZX9P",
		Credential: &seataccess.Credential{Variable: "APP_STORE_CONNECT_KEY_GROWTH"},
		Check:      &seataccess.IdentityCheck{Status: seataccess.IdentityReady, Role: role, Team: team}}}
}

// WHO SIGNS EACH CALL (ADR-0179): the seat's own key for whatever its role may
// make; the company's least key for a read it may not, recorded as the
// company's; the company's Admin key only for an Admin-only act an
// administrator allowed; and a refusal, never the company's, for any other
// change, a call in another team's apps, or a seat whose key was refused.
//
// Mutation: let a change its role may not make fall to the company's key, and
// an agent's change reaches Apple under the company's name.
func TestASeatsCallsGoAsItsOwnKeyOrAreRefused(t *testing.T) {
	t.Parallel()
	read := func(id string) made {
		op, _ := Catalog().Operation(id)
		return made{op: op, class: op.Class}
	}
	write := func(id string, class growth.Class) made {
		op, _ := Catalog().Operation(id)
		return made{op: op, class: class}
	}
	refusedErr := &seataccess.IdentityRefusal{Kind: integration.KindAppStoreConnect, Handle: "growth",
		Status: seataccess.IdentityRefused, Said: "Apple refuses key 7KQ4M2ZX9P"}
	for _, c := range []struct {
		name    string
		signer  seataccess.Signer
		err     error
		m       made
		want    growth.Signer
		ownKey  bool
		refused bool
	}{
		{"company's identity", seataccess.Signer{}, nil, read("apps_customerReviews_getToManyRelated"),
			growth.SignerCompany, false, false},
		{"own key, a read its role makes", own("Marketing", "main"), nil,
			read("apps_customerReviews_getToManyRelated"), growth.SignerOwn, true, false},
		{"own key, a read its role may not make", own("Marketing", "main"), nil,
			read("financeReports_getCollection"), growth.SignerCompany, false, false},
		{"own key, a change its role makes", own("Marketing", "main"), nil,
			write("appStoreVersionLocalizations_updateInstance", growth.ClassOperate), growth.SignerOwn, true, false},
		{"own key, a change its role may not make", own("Marketing", "main"), nil,
			write("builds_updateInstance", growth.ClassOperate), "", false, true},
		{"own key, an Admin-only act allowed", own("App Manager", "main"), nil,
			write("alternativeDistributionDomains_createInstance", growth.ClassAdmin), growth.SignerCompany, false, false},
		{"own key of another team, a change", own("Marketing", "other"), nil,
			write("appStoreVersionLocalizations_updateInstance", growth.ClassOperate), "", false, true},
		{"own key refused by Apple", seataccess.Signer{}, refusedErr, read("apps_customerReviews_getToManyRelated"),
			"", false, true},
	} {
		p, tm := signRig(t, c.signer, c.err)
		m := c.m
		m.team = tm
		err := p.sign(t.Context(), "growth", &m)
		var r *growth.Refusal
		if refused := errors.As(err, &r); refused != c.refused {
			t.Errorf("%s: error %v, want refused %v", c.name, err, c.refused)
			continue
		}
		if c.refused {
			continue
		}
		if m.signer != c.want || (m.own != nil) != c.ownKey {
			t.Errorf("%s: signed as %q with own key %v, want %q and %v (slot %s)", c.name, m.signer, m.own != nil,
				c.want, c.ownKey, m.slot)
		}
		if c.ownKey && m.slot == "" {
			t.Errorf("%s: the access log names no role for the own key", c.name)
		}
	}
}
