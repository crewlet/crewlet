package appstoreconnect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
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

// signRig is a product whose company connects team main and holds no key,
// and whose seat growth is signed as signer says.
func signRig(t *testing.T, signer seataccess.Signer, err error) *Product {
	t.Helper()
	sealed := testKey(t)
	on := true
	company := &config.Company{
		Integrations: config.Integrations{AppStoreConnect: &config.AppStoreConnect{Enabled: true,
			Connections: map[string]config.AppStoreConnectTeam{"main": {IssuerID: "57246542-96fe-1a63-e053-0824d011072a"}}}},
		Roles: []config.Role{{Name: "Growth", Handle: "growth", Integrations: config.RoleIntegrations{
			AppStoreConnect: &config.RoleAppStoreConnect{Enabled: &on, Apps: []string{"111"}}}}},
	}
	return New(Deps{
		Company: func() *config.Company { return company },
		Resolve: func(string) string { return sealed },
		Identity: func(context.Context, string) (seataccess.Signer, error) {
			return signer, err
		},
	})
}

func own(role, team string) seataccess.Signer {
	return ownChecked(role, team, seataccess.IdentityReady)
}

func ownChecked(role, team string, status seataccess.IdentityStatus) seataccess.Signer {
	return seataccess.Signer{Own: true, Record: &seataccess.IdentityRecord{ID: "7KQ4M2ZX9P",
		Credential: &seataccess.Credential{Variable: "APP_STORE_CONNECT_KEY_GROWTH"},
		Check:      &seataccess.IdentityCheck{Status: status, Role: role, Team: team}}}
}

// WHO SIGNS EACH CALL (ADR-0179): the seat's own verified key, for whatever
// its role may make, and nothing else: a call its role may not make, an act
// only Admin may make, a seat with no key, a key Apple refused and a key of a
// team the company no longer connects are each refused before Apple is asked,
// in words naming what to do. Crewlet holds no key of the company's to fall to.
//
// Mutation: let keyOf answer a key whose check failed, or let sign pass a call
// its role may not make, and an agent's call reaches Apple unsigned by a
// working key of its own.
func TestASeatsCallsGoAsItsOwnKeyOrAreRefused(t *testing.T) {
	t.Parallel()
	op := func(id string, class growth.Class) made {
		o, _ := Catalog().Operation(id)
		return made{op: o, class: class}
	}
	for _, c := range []struct {
		name   string
		signer seataccess.Signer
		err    error
		level  growth.Level
		m      made
		said   string
	}{
		{"own key, a read its role makes", own("Marketing", "main"), nil, growth.LevelRead,
			op("apps_customerReviews_getToManyRelated", growth.ClassRead), ""},
		{"own key, a change its role makes", own("App Manager", "main"), nil, growth.LevelOperate,
			op("builds_updateInstance", growth.ClassOperate), ""},
		{"own key, a read no fitting role makes", own("Marketing", "main"), nil, growth.LevelRead,
			op("financeReports_getCollection", growth.ClassRead), "never holds"},
		{"own key, a change its role may not make", own("Marketing", "main"), nil, growth.LevelOperate,
			op("builds_updateInstance", growth.ClassOperate), "App Manager"},
		{"an act only Admin may make", own("App Manager", "main"), nil, growth.LevelAdminister,
			op("alternativeDistributionDomains_createInstance", growth.ClassAdmin), "holds no Admin key"},
		{"no key of its own", seataccess.Signer{Own: true}, nil, growth.LevelRead,
			op("apps_customerReviews_getToManyRelated", growth.ClassRead), "Set up identity"},
		{"a key Apple refused", ownChecked("Marketing", "main", seataccess.IdentityRefused), nil, growth.LevelRead,
			op("apps_customerReviews_getToManyRelated", growth.ClassRead), "Set up identity"},
		{"a key of a team no longer connected", own("Marketing", "other"), nil, growth.LevelRead,
			op("apps_customerReviews_getToManyRelated", growth.ClassRead), "no longer connects"},
		{"the ledger unread", seataccess.Signer{}, errors.New("coord: unavailable"), growth.LevelRead,
			op("apps_customerReviews_getToManyRelated", growth.ClassRead), "could not read"},
	} {
		p := signRig(t, c.signer, c.err)
		m := c.m
		key, err := p.keyOf(t.Context(), "growth")
		if err == nil {
			m.key, m.team = key, key.team
			err = sign(&m, c.level)
		}
		var r *growth.Refusal
		switch {
		case c.said == "" && err != nil:
			t.Errorf("%s: refused %v, want signed with its own key", c.name, err)
		case c.said == "":
			if m.key.client == nil || m.team == nil || m.team.id != "main" {
				t.Errorf("%s: signed with %+v", c.name, m.key)
			}
		case !errors.As(err, &r) || !strings.Contains(r.Error(), c.said):
			t.Errorf("%s: answered %v, want a refusal saying %q", c.name, err, c.said)
		}
	}
}
