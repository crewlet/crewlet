package seataccess_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/seataccess"

	_ "github.com/crewlet/crewlet/internal/aws"
	_ "github.com/crewlet/crewlet/internal/cloudflare"
	_ "github.com/crewlet/crewlet/internal/digitalocean"
	_ "github.com/crewlet/crewlet/internal/expo"
	_ "github.com/crewlet/crewlet/internal/googlecloud"
	_ "github.com/crewlet/crewlet/internal/scaleway"
)

// EVERY LANE THAT TAKES GRANTS SAYS HOW AN AGENT READS THEM: what the vendor
// refuses a missing permission with, what a grant names, and where one naming
// no entry applies, so the request tool and a seat's access note are written
// for every such lane from its own vendor's words and never one vendor's.
// A lane taking none says nothing.
func TestEveryGrantLaneSaysHowAnAgentReadsItsGrants(t *testing.T) {
	t.Parallel()
	grantLanes := 0
	for _, v := range seataccess.Registered() {
		for _, spec := range v.Catalog().Lanes {
			terms := spec.GrantTerms
			if !spec.Grants {
				if terms != (seataccess.GrantTerms{}) {
					t.Errorf("%s takes no grants but says %+v", spec.Lane, terms)
				}
				continue
			}
			grantLanes++
			if terms.Refusal == "" || terms.Permission == "" {
				t.Errorf("%s takes grants but names no refusal or permission: %+v", spec.Lane, terms)
			}
			if terms.Whole == "" && terms.Account == "" {
				t.Errorf("%s takes grants but says nowhere a grant naming no entry applies", spec.Lane)
			}
			if got := seataccess.GrantTermsOf(spec.Lane); got != terms {
				t.Errorf("%s reads %+v, its catalogue says %+v", spec.Lane, got, terms)
			}
		}
	}
	if grantLanes < 4 {
		t.Fatalf("%d lanes take grants, want Cloudflare, Scaleway, AWS and Google Cloud at least", grantLanes)
	}
}
