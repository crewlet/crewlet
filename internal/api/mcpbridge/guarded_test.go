package mcpbridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

// whoami answers who the call it is running under acts as. It is registered
// UNGATED, so it reports the identity the bridge handed the surface rather
// than an authority decision about it.
type whoami struct{}

func (whoami) Name() string        { return "whoami" }
func (whoami) Description() string { return "says who this call acts as" }
func (whoami) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (whoami) Call(ctx context.Context, _ map[string]any) (tools.Result, error) {
	p, how := iam.From(ctx)
	return tools.Result{Output: string(how) + " " + string(p.Kind) + " seat=" +
		p.Seat + " login=" + p.Login}, nil
}

// A BRIDGED CALL IS THE RUN'S SEAT, BEHIND THE REAL GUARD.
//
// The route is exempt from the credential guard — the box holds no API token —
// but the guard still RESOLVES every request, so a box presenting nothing
// arrives ANONYMOUS, and on a development build started with -dev-principal it
// arrives as the development principal. The MCP SDK keeps the context of the
// request that opened a session for that session's whole life, and the turn's
// own principal defers to any answer already on the context. So every gated
// builtin a coding agent called over the bridge was refused as needing a
// credential: agent mode could not read the board, look a colleague up or
// submit its work.
//
// Mounted here exactly as the app mounts it — the bridge's handler inside the
// guard's middleware — because a bridge tested on a bare mux never meets the
// answer the guard attaches, which is the whole of the defect.
func TestABridgedCallActsAsTheRunsSeatBehindTheGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// dev is the -dev-principal login, or empty for an ordinary
		// posture — the one where every box resolves anonymous.
		dev string
	}{
		{name: "an ordinary posture", dev: ""},
		{name: "a development principal", dev: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.API.Host = "127.0.0.1"
			b.API.Auth.MaxGrants = iam.AllGrants
			guard := auth.New(&b)
			if tc.dev != "" {
				dev, err := auth.NewDevPrincipal(tc.dev, &b)
				if err != nil {
					t.Fatalf("NewDevPrincipal: %v", err)
				}
				guard = guard.WithDevPrincipal(dev)
			}

			// THE SEAT AND A COLLEAGUE, so lookup_colleague has somebody
			// to find and the answer cannot be the caller themselves.
			seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
			colleague := &org.Role{Name: "Designer", DeclaredHandle: "ana"}
			turn := &turnctx.Turn{
				RunID: "run-1", Seat: seat,
				Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat, colleague}},
			}

			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, builtin.Deps{
				Authorize: builtin.Decide(authz.NoChart{}),
			}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if err := reg.Register(whoami{}, tools.OriginBuiltin); err != nil {
				t.Fatalf("Register(whoami): %v", err)
			}
			surface := tools.NewSurface("execute", reg.Snapshot(),
				[]string{builtin.LookupColleagueTool, "whoami"}).ForTurn(turn)

			server := httptest.NewServer(nil)
			t.Cleanup(server.Close)
			bridge := mcpbridge.New(mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
			})
			mux := http.NewServeMux()
			mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
			// THE CONTROL: a handler behind the same guard on the same
			// exempt prefix sees the answer the bridge must replace. Without
			// it this case could pass on a guard that attached nothing.
			var guardSaw iam.Resolution
			var guardLogin string
			mux.HandleFunc("GET "+mcpbridge.PathPrefix+"probe/{x}",
				func(_ http.ResponseWriter, r *http.Request) {
					p, how := iam.From(r.Context())
					guardSaw, guardLogin = how, p.Login
				})
			server.Config.Handler = guard.Middleware(mux)

			probe, err := server.Client().Get(server.URL + mcpbridge.PathPrefix + "probe/x")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			_ = probe.Body.Close()
			switch {
			case tc.dev == "" && guardSaw != iam.Anonymous:
				t.Fatalf("the guard attached %q to an exempt request that presented "+
					"nothing, want anonymous — the control is not the posture it names",
					guardSaw)
			case tc.dev != "" && guardLogin != auth.DevPrincipalLoginPrefix+tc.dev:
				t.Fatalf("the guard attached %q (%s), want the development "+
					"principal — the control is not the posture it names",
					guardLogin, guardSaw)
			}

			url := bridge.Open(&mcpbridge.Session{
				RunID: "run-1", Handle: "dev", Role: "Engineer", Surface: surface,
			})
			if url == "" {
				t.Fatal("no endpoint was minted")
			}
			sess := dial(t, url)

			// A GATED BUILTIN, decided by the real authority table.
			res := callTool(t, sess, builtin.LookupColleagueTool,
				map[string]any{"query": "Designer"})
			if res.IsError {
				t.Fatalf("the seat's own lookup_colleague was refused over the "+
					"bridge: %s", text(res))
			}
			if !strings.Contains(text(res), "ana") {
				t.Errorf("lookup_colleague answered %q, want the colleague", text(res))
			}

			// AND WHO IT RAN AS: the seat, never the guard's answer.
			got := text(callTool(t, sess, "whoami", map[string]any{}))
			if want := "resolved seat seat=dev "; !strings.HasPrefix(got, want) {
				t.Errorf("a bridged call ran as %q, want the run's seat (%q…)", got, want)
			}
			if strings.Contains(got, auth.DevPrincipalLoginPrefix) {
				t.Errorf("a bridged call ran as the development principal: %q — "+
					"a laptop's no-credential identity rode a seat's tool call", got)
			}
		})
	}
}
