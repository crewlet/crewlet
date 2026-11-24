package executor_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// deadProxy is a proxy nothing listens on: a call routed through it fails.
const deadProxy = "http://127.0.0.1:1"

// harnessProxyKeys is, by runner, the harness's own proxy setting a home may
// hold, planted at a dead proxy beside the environment's: Copilot's
// `proxyUrl` and Pi's `httpProxy`, each measured to carry the bridge's calls
// on the real harness (1.0.114, 1.6.0) until NO_PROXY named the bridge's host.
var harnessProxyKeys = map[string]map[string]string{
	executor.CopilotName: {".copilot/settings.json": `{"proxyUrl":"` + deadProxy + `"}`},
	executor.PiName:      {".pi/agent/settings.json": `{"httpProxy":"` + deadProxy + `"}`},
}

// EVERY RUN REACHES CREWLET'S OWN ROUTES PAST ANY PROXY. Each registered
// runner's whole run in a real box ([creditBoxes]), under an environment whose
// HTTP_PROXY and HTTPS_PROXY (and the harness's own proxy key, where it has
// one) name a proxy nothing listens on: a proxy-honouring client in the
// harness's process (git, through libcurl) still reaches the bridge's host
// directly, and the operator's NO_PROXY and the profile's no_proxy entries
// stand beside the bridge's host in both spellings.
//
// Mutations, each red: write no statement (the bridge is never reached); set
// the hosts alone (the operator's and the profile's entries are lost); merge
// one spelling only (the other lacks an entry); drop a runner from
// creditBoxes (named).
func TestEveryRunReachesItsBridgePastAProxy(t *testing.T) {
	t.Parallel()
	git := gittest.Require(t)
	for _, name := range executor.Names() {
		boxed, ok := creditBoxes[name]
		if !ok {
			t.Errorf("runner %s has no box in creditBoxes, so nothing shows its bridge reached past a proxy", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var reached atomic.Int32
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				http.NotFound(w, r)
			}))
			t.Cleanup(bridge.Close)
			runner, b := boxed(t)
			plant(t, b, harnessProxyKeys[name])
			// The scenario dials only under the run's environment: a guard's own
			// call of the stand-in before the run, without it, reaches nothing.
			plant(t, b, map[string]string{
				".profile": "export no_proxy=home.example.internal\n",
				"scenario.sh": "printf 'NO_PROXY=%s\\nno_proxy=%s\\n' \"$NO_PROXY\" \"$no_proxy\"\n" +
					"[ -z \"${HTTP_PROXY:-}\" ] || " + shellword.Quote(git) + " ls-remote " +
					shellword.Quote(bridge.URL+"/r.git") + " 2>&1\n",
			})
			proxied := map[string]string{"NO_PROXY": "operator.example.internal"}
			for _, v := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
				proxied[v] = deadProxy
			}
			// Named as the OpenCode stand-in reports its server connected.
			handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "go", Env: proxied,
				MCPServers: map[string]sandbox.MCPServer{"crewlet-bridge": {Transport: sandbox.TransportHTTP,
					URL: bridge.URL + "/mcp/run-token-placeholder", AlwaysLoaded: true}}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			awaitOver(t, runner, b, handle, time.Minute, "a stand-in that asks once takes a second")
			observed := boxFile(t, b, b.Home()+"/observed")
			report := "\n--- observed\n" + observed + "--- harness\n" + boxFile(t, b, executor.PathsFor(b).Err())
			if reached.Load() == 0 {
				t.Errorf("the harness's process never reached the bridge's host past the proxy:%s", report)
			}
			for _, spelling := range []string{"NO_PROXY=", "no_proxy="} {
				line := ""
				for l := range strings.SplitSeq(observed, "\n") {
					if strings.HasPrefix(l, spelling) {
						line = l
					}
				}
				for _, entry := range []string{"operator.example.internal", "home.example.internal", "127.0.0.1"} {
					if !strings.Contains(line, entry) {
						t.Errorf("the harness's %s does not hold %s:%s", spelling, entry, report)
					}
				}
			}
		})
	}
}
