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

// THE CONNECT FORM IS THE ACCOUNT NAME ALONE, with no credential and nothing
// its Test button could check.
func TestTheExpoConnectFormIsTheAccountAlone(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	reqs := expo.Requirements(nil, none)
	var fields []string
	for _, r := range reqs {
		fields = append(fields, r.Field)
		if r.Kind == setup.KindSecret || r.Checkable || r.Present {
			t.Errorf("%s = %+v", r.Field, r)
		}
	}
	if strings.Join(fields, ",") != "account_name,enabled" {
		t.Fatalf("fields = %v", fields)
	}
	reqs = expo.Requirements(&config.Expo{Enabled: true,
		Provisioning: &config.ExpoProvisioning{AccountName: testAccountName}}, none)
	for _, r := range reqs {
		if !r.Present {
			t.Errorf("%s is absent on a connected company", r.Field)
		}
	}
	_, err := expo.Probe(nil)(context.Background(), setup.Check{Field: "account_name",
		Value: func(string) string { return testAccountName }})
	if !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("the probe checked %v", err)
	}
}
