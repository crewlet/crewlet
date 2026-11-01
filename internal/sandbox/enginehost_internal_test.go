package sandbox

import (
	"maps"
	"slices"
	"testing"
)

// A LOOPBACK ADDRESS IN THE ENGINE'S NETWORK NAMES THE ENGINE HOST IN THE BOX'S.
//
// The tool bridge, the telemetry receiver and a seat's HTTP MCP servers are
// configured once for the node, in the engine host's network. Inside a
// container `127.0.0.1` is the container itself, so a single-host install
// whose bridge is `http://127.0.0.1:8000` reached its tools from a `direct` box
// and from no container. Only a loopback host moves; everything that already
// names a machine both sides can see, and everything that is not a URL at
// all, reaches the box exactly as written.
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
// box shares the engine's loopback, and a remote one has no name for it at
// all, so neither may be given a container runtime's alias.
func TestOnlyABoxWithANetworkOfItsOwnIsHandedTheAlias(t *testing.T) {
	t.Parallel()
	const raw = "http://127.0.0.1:8000/mcp"
	for _, box := range []Sandbox{&directBox{}, NewFakeSandbox("remote")} {
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
// carries it; Podman writes its own name into every container, so nothing is
// added there. Before the operator's run_args, which win as they always do.
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
			argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b", nil)
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

// EVERY NAME A BOX IS HANDED FOR THE ENGINE HOST IS ONE THE ENGINE ADMITS. The
// tool bridge admits a call to a session held to this host's loopback under a
// loopback name or an [EngineHostName], which is how Docker Desktop and
// Podman's machine deliver a container's call to the host; a runtime whose
// name it did not admit would leave every held run in that runtime's
// containers without its tools. And nothing else is one: a name the runtimes
// do not hand out, or one merely containing theirs, is not.
func TestEveryRuntimesNameForTheEngineHostIsAdmitted(t *testing.T) {
	t.Parallel()
	for _, runtime := range []string{"docker", "podman"} {
		alias := containerCLI{path: "/usr/bin/" + runtime}.hostAlias()
		if !EngineHostName(alias) {
			t.Errorf("%s hands a box %q for the engine host, which EngineHostName refuses", runtime, alias)
		}
	}
	for _, name := range []string{"", "localhost", "docker.internal", "host.docker.internal.example.com",
		"evil-host.docker.internal", "host.internal"} {
		if EngineHostName(name) {
			t.Errorf("EngineHostName(%q) = true, want only the runtimes' own names", name)
		}
	}
}
