package engine_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
)

// A LINUX HOST WHOSE API BINDS LOOPBACK CANNOT BE REACHED FROM ITS CONTAINER
// BOXES, AND SAYS SO.
//
// A container box dials the engine host by its runtime's name for it, which on
// Linux arrives from the container's bridge network, never on 127.0.0.1. With
// the API bound to loopback every bridged call and every telemetry export
// failed as a connection the box could not make, with nothing on the engine
// side to say why. The advisory names the field to change, and stays quiet
// wherever the bind does answer or is not the one a box dials: another OS, a
// wider bind, a public listener that is the one boxes dial, a node without
// the ingress role, which serves its probes alone, a node without the seats
// role, which starts no box on its host for its bind to cut off, or a company
// with no container cell.
func TestALoopbackBindOnLinuxWarnsAContainerCompany(t *testing.T) {
	t.Parallel()
	container := sandboxCompany(t, "container")
	direct := sandboxCompany(t, "direct")
	for _, tc := range []struct {
		name     string
		api      config.API
		roles    []string
		goos     string
		cfg      *config.Company
		wantPath string
	}{
		{"loopback on linux", config.API{Host: "127.0.0.1", Port: 8000}, nil, "linux", container, "api.host"},
		{"localhost on linux", config.API{Host: "localhost", Port: 8000}, nil, "linux", container, "api.host"},
		{"the IPv6 loopback on linux", config.API{Host: "::1", Port: 8000}, nil, "linux", container, "api.host"},
		{"a loopback public listener", config.API{Host: "0.0.0.0", Port: 8000,
			Public: config.APIPublic{Host: "127.0.0.1", Port: 8080}}, nil, "linux", container, "api.public.host"},
		{"loopback on macOS, where the runtime forwards it", config.API{Host: "127.0.0.1", Port: 8000}, nil, "darwin", container, ""},
		{"every interface", config.API{Host: "", Port: 8000}, nil, "linux", container, ""},
		{"the bridge address", config.API{Host: "172.17.0.1", Port: 8000}, nil, "linux", container, ""},
		{"a public listener on every interface", config.API{Host: "127.0.0.1", Port: 8000,
			Public: config.APIPublic{Port: 8080}}, nil, "linux", container, ""},
		{"no listener at all", config.API{Host: "127.0.0.1"}, nil, "linux", container, ""},
		{"a node without the ingress role, which serves its probes alone",
			config.API{Host: "127.0.0.1", Port: 8000}, []string{"data", "seats"}, "linux", container, ""},
		{"an ingress node that runs no seats, so starts no box on its host",
			config.API{Host: "127.0.0.1", Port: 8000}, []string{"data", "ingress"}, "linux", container, ""},
		{"a node with the ingress role among others", config.API{Host: "127.0.0.1", Port: 8000},
			[]string{"data", "ingress", "seats"}, "linux", container, "api.host"},
		{"no container cell", config.API{Host: "127.0.0.1", Port: 8000}, nil, "linux", direct, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot := &config.Bootstrap{API: tc.api, Node: config.Node{Roles: tc.roles}}
			got := engine.ContainerReachWarnings(boot, tc.cfg, tc.goos)
			if tc.wantPath == "" {
				if len(got) != 0 {
					t.Fatalf("warned %+v where no container box is cut off by this bind", got)
				}
				return
			}
			if len(got) != 1 || got[0].Path != tc.wantPath {
				t.Fatalf("warnings = %+v, want one at %s", got, tc.wantPath)
			}
			if !strings.Contains(got[0].Message, "leave "+tc.wantPath+" empty") {
				t.Errorf("the advisory does not name its remedy: %s", got[0].Message)
			}
		})
	}
}

// sandboxCompany is a minimal company whose one engineer's code work runs in cell.
func sandboxCompany(t *testing.T, cell string) *config.Company {
	t.Helper()
	doc := `
name: Acme
providers:
  llm:
    default: {model: sonnet}
  sandbox:
    local: {` + map[bool]string{true: "image: example.com/coding:1", false: ""}[cell == "container"] + `}
    default_run_in: ` + cell + `
roles:
  - name: Engineer
    handle: eng
    goal: build
`
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	return cfg
}
