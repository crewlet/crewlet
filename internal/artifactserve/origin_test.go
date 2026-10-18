package artifactserve_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactserve"
)

// TestAnOriginIsRefusedUnlessItCanNameWhereAnArtifactRuns: the shapes Tier A
// refuses are refused here too, by name.
func TestAnOriginIsRefusedUnlessItCanNameWhereAnArtifactRuns(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]artifactserve.OriginsConfig{
		"no external URL":        {Mode: artifacts.OriginShared, Dashboard: []string{dashboard}},
		"not http":               {External: "ftp://artifacts.example.net", Mode: artifacts.OriginShared, Dashboard: []string{dashboard}},
		"a path":                 {External: "https://artifacts.example.net/app", Mode: artifacts.OriginShared, Dashboard: []string{dashboard}},
		"a query":                {External: "https://artifacts.example.net?x=1", Mode: artifacts.OriginShared, Dashboard: []string{dashboard}},
		"the placeholder twice":  {External: "https://{artifact}.{artifact}.example.net", Mode: artifacts.OriginShared, Dashboard: []string{dashboard}},
		"per artifact, no label": {External: "https://artifacts.example.net", Mode: artifacts.OriginPerArtifact, Dashboard: []string{dashboard}},
		"a label inside a label": {External: "https://x{artifact}.example.net", Mode: artifacts.OriginPerArtifact, Dashboard: []string{dashboard}},
		"a label with no domain": {External: "https://{artifact}", Mode: artifacts.OriginPerArtifact, Dashboard: []string{dashboard}},
		"no dashboard":           {External: template, Mode: artifacts.OriginPerArtifact},
		"a dashboard that is not an origin": {External: template, Mode: artifacts.OriginPerArtifact,
			Dashboard: []string{"crewlet.example.com"}},
		"an unknown mode": {External: template, Mode: "isolated", Dashboard: []string{dashboard}},
	} {
		if _, err := artifactserve.NewOrigins(c); !errors.Is(err, artifactserve.ErrOrigin) {
			t.Errorf("%s: NewOrigins = %v, want ErrOrigin", name, err)
		}
	}
}

// TestAnArtifactIsServedOnTheOneHostItsTicketNames: the whole host, any case,
// any port, and no other host.
func TestAnArtifactIsServedOnTheOneHostItsTicketNames(t *testing.T) {
	t.Parallel()
	o, err := artifactserve.NewOrigins(artifactserve.OriginsConfig{
		External: "https://{artifact}--acme.example-usercontent.net:8443",
		Mode:     artifacts.OriginPerArtifact, Dashboard: []string{dashboard, dashboard + "/"},
	})
	if err != nil {
		t.Fatalf("NewOrigins: %v", err)
	}
	id := artifacts.IDFor("op")
	host := id.Label().String() + "--acme.example-usercontent.net"
	if o.Host(id) != host || o.Origin(id) != "https://"+host+":8443" {
		t.Fatalf("host %q origin %q", o.Host(id), o.Origin(id))
	}
	for _, served := range []string{host, host + ":8443", host + ":443", host + ".",
		strings.ToUpper(host)} {
		if !o.Serves(served, id) {
			t.Errorf("%q does not serve its own artifact", served)
		}
	}
	other := artifacts.IDFor("other")
	for _, refused := range []string{o.Host(other), host + ".evil.test", "example-usercontent.net",
		"--acme.example-usercontent.net", ""} {
		if o.Serves(refused, id) {
			t.Errorf("%q serves an artifact whose host it is not", refused)
		}
	}
	if got := o.Policy(id).Ancestors; len(got) != 1 || got[0] != dashboard {
		t.Errorf("the dashboard origins are %v, want one, normalised", got)
	}
}

// TestTheDashboardFramesExactlyTheArtifactOrigin: a template frames the hosts it
// names, a plain origin itself, and no URL nothing.
func TestTheDashboardFramesExactlyTheArtifactOrigin(t *testing.T) {
	t.Parallel()
	for external, want := range map[string]string{
		"https://{artifact}.example-usercontent.net":       "https://*.example-usercontent.net",
		"https://{artifact}--acme.crewletusercontent.com/": "https://*.crewletusercontent.com",
		"http://{artifact}.localhost:8001":                 "http://*.localhost:8001",
		"https://artifacts.example.com":                    "https://artifacts.example.com",
		"http://127.0.0.1:8001":                            "http://127.0.0.1:8001",
		"":                                                 "",
	} {
		got, err := artifactserve.FrameSource(external)
		if err != nil || got != want {
			t.Errorf("FrameSource(%q) = %q, %v; want %q", external, got, err, want)
		}
	}
	if _, err := artifactserve.FrameSource("https://{artifact}"); err == nil {
		t.Error("a template with no domain under its label framed something")
	}
}
