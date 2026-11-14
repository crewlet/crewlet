package expo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE CONNECT FORM NEEDS THE ACCOUNT NAME ALONE: its one credential, the
// atlas's robot token, is optional, and the account name itself has nothing
// a Test button could check.
func TestTheExpoConnectFormNeedsTheAccountAlone(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	reqs := expo.Requirements(nil, none)
	var fields []string
	for _, r := range reqs {
		fields = append(fields, r.Field)
		if (r.Field != expo.AtlasField && (r.Kind == setup.KindSecret || r.Checkable)) || r.Present ||
			(r.Field == expo.AtlasField && r.Required) {
			t.Errorf("%s = %+v", r.Field, r)
		}
	}
	if strings.Join(fields, ",") != "account_name,enabled,atlas_token" {
		t.Fatalf("fields = %v", fields)
	}
	reqs = expo.Requirements(&config.Expo{Enabled: true,
		Provisioning: &config.ExpoProvisioning{AccountName: testAccountName}}, none)
	for _, r := range reqs {
		if r.Present == (r.Field == expo.AtlasField) {
			t.Errorf("%s: present is %v on a company connected with no atlas token", r.Field, r.Present)
		}
	}
	_, err := expo.Probe(nil)(context.Background(), setup.Check{Field: "account_name",
		Value: func(string) string { return testAccountName }})
	if !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("the probe checked %v", err)
	}
}
