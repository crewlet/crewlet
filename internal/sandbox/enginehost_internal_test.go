package sandbox

import (
	"maps"
	"slices"
	"testing"
)

// A LOOPBACK ADDRESS IN THE ENGINE'S NETWORK NAMES THE ENGINE HOST IN THE BOX'S.
// Inside a container 127.0.0.1 is the container itself, so a loopback host
// moves; a host both sides can see, and anything that is not a URL, reaches
// the box exactly as written.
func TestALoopbackHostIsTheEngineHostInsideTheBox(t *testing.T) {
	t.Parallel()
	const alias = "host.docker.internal"
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"an IPv4 loopback with a port, a path and a query",
			"http://127.0.0.1:8000/mcp/crewlet_rt1.x?a=b", "http://host.docker.internal:8000/mcp/crewlet_rt1.x?a=b"},
		{"any address in 127/8", "http://127.5.0.1:9/otlp", "http://host.docker.internal:9/otlp"},
		{"localhost", "https://localhost/v1", "https://host.docker.internal/v1"},
		{"localhost in another case", "http://LocalHost:8000", "http://host.docker.internal:8000"},
		{"a name under localhost", "http://engine.localhost:8000", "http://host.docker.internal:8000"},
		{"the IPv6 loopback", "http://[::1]:8000/mcp", "http://host.docker.internal:8000/mcp"},
		{"user information is kept", "http://u:p@127.0.0.1:8000/x", "http://u:p@host.docker.internal:8000/x"},
		{"a LAN address", "http://192.168.1.20:8000/mcp", "http://192.168.1.20:8000/mcp"},
		{"a public name", "https://engine.example.com/mcp", "https://engine.example.com/mcp"},
		{"the alias itself", "http://host.docker.internal:8000", "http://host.docker.internal:8000"},
		{"a name that merely contains localhost", "http://notlocalhost:8000", "http://notlocalhost:8000"},
		{"a host and port with no scheme", "127.0.0.1:8000", "127.0.0.1:8000"},
		{"a value that is not a URL", "00-trace-span-01", "00-trace-span-01"},
		{"nothing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rehost(tc.raw, alias); got != tc.want {
				t.Fatalf("rehost(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A BOX ON THE HOST'S OWN NETWORK IS HANDED THE ADDRESS AS WRITTEN. A direct
// box shares the engine's loopback, and a box that names no host alias has
// none to take, so neither may be given a container runtime's alias.
func TestOnlyABoxWithANetworkOfItsOwnIsHandedTheAlias(t *testing.T) {
	t.Parallel()
	const raw = "http://127.0.0.1:8000/mcp"
	for _, box := range []Sandbox{&directBox{}, NewFakeSandbox("unaliased")} {
		if got := EngineAddress(box, raw); got != raw {
			t.Errorf("%T: EngineAddress = %q, want %q unchanged", box, got, raw)
		}
	}
	docker := &containerBox{cli: containerCLI{path: "/usr/local/bin/docker"}}
	if got := EngineAddress(docker, raw); got != "http://host.docker.internal:8000/mcp" {
		t.Errorf("docker: EngineAddress = %q", got)
	}
	podman := &containerBox{cli: containerCLI{path: "/usr/bin/podman"}}
	if got := EngineAddress(podman, raw); got != "http://host.containers.internal:8000/mcp" {
		t.Errorf("podman: EngineAddress = %q", got)
	}
}

// THE ENGINE HOST'S NAME IN A CELL IS KNOWN BEFORE ANY BOX OF IT EXISTS: what a
// box is made with is written then, so the manager answers a cell's name from
// its backend, exactly as the cell's boxes answer it. A container cell's is its
// runtime's alias; a direct cell, on the host's network, has none, nor has a
// cell no backend serves.
//
// Mutation: answer the runtime's alias whatever the backend's cell — the
// direct cell names host.docker.internal, red.
func TestTheEngineHostsNameInACellIsKnownBeforeItsBox(t *testing.T) {
	t.Parallel()
	docker := &Local{opts: LocalOptions{Placement: Container}, cli: containerCLI{path: "/usr/local/bin/docker"}}
	podman := &Local{opts: LocalOptions{Placement: Container}, cli: containerCLI{path: "/usr/bin/podman"}}
	direct := &Local{opts: LocalOptions{Placement: Direct}}
	if got, box := docker.EngineHost(), (&containerBox{cli: docker.cli}).EngineHost(); got != box ||
		got != "host.docker.internal" {
		t.Errorf("docker's cell names the engine host %q and its box %q, want host.docker.internal", got, box)
	}
	if got := podman.EngineHost(); got != "host.containers.internal" {
		t.Errorf("podman's cell names the engine host %q, want host.containers.internal", got)
	}
	m, err := NewManager(ManagerOptions{Providers: map[Placement]Provider{Direct: direct, Container: docker},
		DefaultPlacement: Direct, Runners: map[string]Runner{"claude-code": NewFakeRunner("claude-code")}})
	if err != nil {
		t.Fatal(err)
	}
	for cell, want := range map[Placement]EngineHost{Container: "host.docker.internal", Direct: "", "": ""} {
		if got := m.EngineHost(cell); got != want {
			t.Errorf("the manager names the engine host in cell %q %q, want %q", cell, got, want)
		}
	}
	unconfigured, err := NewManager(ManagerOptions{Providers: map[Placement]Provider{Direct: direct},
		Runners: map[string]Runner{"claude-code": NewFakeRunner("claude-code")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := unconfigured.EngineHost(Container); got != "" {
		t.Errorf("a cell no backend serves names the engine host %q", got)
	}
}

// THE SERVERS ARE TRANSLATED IN A COPY. The map a launch hands a runner is the
// seat's rendered servers, read again by its next run, which may be in a
// different cell; an alias written into it would follow the seat there.
func TestTheSeatsServersAreTranslatedInACopy(t *testing.T) {
	t.Parallel()
	box := &containerBox{cli: containerCLI{path: "docker"}}
	in := map[string]MCPServer{
		"crewlet": {Name: "crewlet", Transport: TransportHTTP, URL: "http://127.0.0.1:8000/mcp/t",
			Headers: map[string]string{"H": "v"}},
		"files": {Name: "files", Transport: TransportStdio, Command: "mcp-files",
			Args: []string{"--url", "http://127.0.0.1:1"}},
		"remote": {Name: "remote", Transport: TransportHTTP, URL: "https://mcp.example.com"},
	}
	before := maps.Clone(in)
	out := EngineServers(box, in)

	if got := out["crewlet"].URL; got != "http://host.docker.internal:8000/mcp/t" {
		t.Errorf("the bridge's URL = %q, want the engine host by its alias", got)
	}
	if got := out["remote"].URL; got != "https://mcp.example.com" {
		t.Errorf("a remote server's URL moved: %q", got)
	}
	// A stdio server runs in the box and dials nothing of the engine's:
	// its arguments are its own business.
	if got := out["files"].Args; !slices.Equal(got, []string{"--url", "http://127.0.0.1:1"}) {
		t.Errorf("a stdio server's arguments were rewritten: %v", got)
	}
	if in["crewlet"].URL != before["crewlet"].URL {
		t.Errorf("the caller's map was rewritten: %q", in["crewlet"].URL)
	}
	out["crewlet"].Headers["H"] = "changed"
	if in["crewlet"].Headers["H"] != "v" {
		t.Error("the copy shares its headers with the caller's map")
	}
}

// THE ALIAS RESOLVES IN THE CONTAINER. Docker Engine on Linux defines
// host.docker.internal only for a container started with it, so the run line
// carries it, before the operator's run_args, which win; Podman writes its own
// name into every container, so nothing is added there.
func TestTheContainerIsStartedKnowingTheEngineHostsName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		runtime, format, info string
		want                  []string
	}{
		{"docker", dockerRootlessFormat, `[name=seccomp]`, []string{"--add-host", "host.docker.internal:host-gateway"}},
		{"podman", podmanRootlessFormat, `true`, nil},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			t.Parallel()
			l := &Local{
				opts: LocalOptions{Placement: Container, Image: "img", RunArgs: []string{"--add-host", "x:1.2.3.4"}},
				cli:  containerCLI{path: fakeRuntime(t, tc.runtime, tc.format, tc.info, 0)},
			}
			argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b")
			first := slices.Index(argv, "--add-host")
			if tc.want == nil {
				if argv[first+1] != "x:1.2.3.4" {
					t.Fatalf("argv = %v, want only the operator's --add-host", argv)
				}
				return
			}
			if first < 0 || !slices.Equal(argv[first:first+2], tc.want) {
				t.Fatalf("argv = %v, want %v", argv, tc.want)
			}
			if operator := slices.Index(argv, "x:1.2.3.4"); operator < first {
				t.Fatalf("argv = %v: the operator's run_args must come after the backend's", argv)
			}
		})
	}
}
