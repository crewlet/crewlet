package executor_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// skillsProbeScript drives the skill-load plugin under node:
// opencode_skills_probe.mjs, beside the plugin it drives.
const skillsProbeScript = "opencode_skills_probe.mjs"

// skillsPlugin is the plugin under test, where the engine embeds it from.
const skillsPlugin = "opencode_skills.js"

// skillReport is one request the plugin made of the skill route, held until
// the case answers it or the plugin lets go of it.
type skillReport struct {
	method      string
	contentType string
	body        string
	gone        <-chan struct{}
	answer      chan<- int
}

// skillRoute is a stand-in of the bridge's skill route (internal/api/mcpbridge,
// SkillPattern): every request is handed to the case, which answers it.
type skillRoute struct {
	t       *testing.T
	srv     *httptest.Server
	reports chan *skillReport
}

func newSkillRoute(t *testing.T) *skillRoute {
	t.Helper()
	r := &skillRoute{t: t, reports: make(chan *skillReport, 16)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		answer := make(chan int, 1)
		r.reports <- &skillReport{method: req.Method, contentType: req.Header.Get("Content-Type"),
			body: string(body), gone: req.Context().Done(), answer: answer}
		select {
		case status := <-answer:
			w.WriteHeader(status)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// url is the route as a run's configuration hands it to the plugin.
func (r *skillRoute) url() string { return r.srv.URL + "/mcp/run-token/skill" }

// next is the plugin's next report, failing the case after ten seconds.
func (r *skillRoute) next(what string) *skillReport {
	r.t.Helper()
	select {
	case rep := <-r.reports:
		return rep
	case <-time.After(10 * time.Second):
		r.t.Fatalf("waited 10s for the plugin to report %s", what)
		return nil
	}
}

// none fails the case if the plugin reports anything within d.
func (r *skillRoute) none(d time.Duration, why string) {
	r.t.Helper()
	select {
	case rep := <-r.reports:
		r.t.Fatalf("%s, and the plugin reported %s %q", why, rep.method, rep.body)
	case <-time.After(d):
	}
}

// skillsProbe is the plugin running under node, driven over the probe's stdin.
type skillsProbe struct {
	t       *testing.T
	stdin   io.WriteCloser
	reports chan map[string]any
	mu      sync.Mutex
	stderr  bytes.Buffer
}

func startSkillsProbe(t *testing.T) *skillsProbe {
	t.Helper()
	node := nodeForProbe(t)
	script, err := filepath.Abs(skillsProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := filepath.Abs(skillsPlugin)
	if err != nil {
		t.Fatal(err)
	}
	p := &skillsProbe{t: t, reports: make(chan map[string]any, 64)}
	cmd := exec.Command(node, script, plugin) //nolint:gosec // the suite's own probe, of the plugin under test
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = lockedWriter{&p.mu, &p.stderr}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the probe: %v", err)
	}
	go func() {
		defer close(p.reports)
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			var r map[string]any
			if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
				r = map[string]any{"op": "error", "message": "an unreadable report: " + scan.Text()}
			}
			p.reports <- r
		}
	}()
	t.Cleanup(func() {
		_ = p.stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return p
}

func (p *skillsProbe) send(command map[string]any) {
	p.t.Helper()
	line, err := json.Marshal(command)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.t.Fatalf("writing to the probe: %v\n%s", err, p.stderrText())
	}
}

func (p *skillsProbe) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

// expect is the probe's next report, which must be op, within d.
func (p *skillsProbe) expect(op string, d time.Duration) map[string]any {
	p.t.Helper()
	select {
	case r, ok := <-p.reports:
		if !ok {
			p.t.Fatalf("the probe ended waiting for %s:\n%s", op, p.stderrText())
		}
		if r["op"] != op {
			p.t.Fatalf("the probe reported %v, want %s\n%s", r, op, p.stderrText())
		}
		return r
	case <-time.After(d):
		p.t.Fatalf("waited %s for the probe to report %s\n%s", d, op, p.stderrText())
		return nil
	}
}

// pending fails the case if the probe reports anything within d.
func (p *skillsProbe) pending(d time.Duration, why string) {
	p.t.Helper()
	select {
	case r := <-p.reports:
		p.t.Fatalf("%s, and the probe reported %v", why, r)
	case <-time.After(d):
	}
}

// setup sets the plugin up against url and returns the hooks it registered.
func (p *skillsProbe) setup(url string) []string {
	p.t.Helper()
	p.send(map[string]any{"op": "setup", "url": url})
	r := p.expect("setup", 10*time.Second)
	var hooked []string
	for _, name := range r["hooked"].([]any) {
		hooked = append(hooked, name.(string))
	}
	return hooked
}

// after runs the plugin's execute.after hook on the event OpenCode raises once
// tool has answered: completed with input, or an error where status says so.
func (p *skillsProbe) after(tool, status string, input map[string]any) {
	p.t.Helper()
	event := map[string]any{"tool": tool, "sessionID": "ses_run", "agent": "build",
		"messageID": "msg_1", "id": "call_1", "input": input, "status": status}
	p.send(map[string]any{"op": "after", "event": event})
}

// skillLoaded is the event of OpenCode's skill tool having loaded id.
func skillLoaded(id string) (string, string, map[string]any) {
	return "skill", "completed", map[string]any{"id": id}
}

// A LOAD IS ON RECORD BEFORE THE MODEL GOES ON: the plugin posts the skill's id
// to the route once OpenCode's skill tool has answered, and its hook — which
// OpenCode waits for before it hands the tool's answer back — resolves only
// once the route has answered, so a call the skill unlocks, made in the
// model's next request, finds the load recorded.
//
// Mutation: fire the report and resolve at once, and the hook resolves while
// the route still holds the report.
func TestTheSkillsPluginReportsALoadBeforeTheModelGoesOn(t *testing.T) {
	t.Parallel()
	route, pr := newSkillRoute(t), startSkillsProbe(t)
	if hooked := pr.setup(route.url()); !slices.Equal(hooked, []string{"execute.after"}) {
		t.Fatalf("the plugin registered %v, want execute.after alone", hooked)
	}
	pr.after(skillLoaded("crewlet:mcp-github"))
	rep := route.next("the load")
	if rep.method != http.MethodPost || rep.contentType != "application/json" ||
		rep.body != `{"skill":"crewlet:mcp-github"}` {
		t.Fatalf("the plugin reported %s %q (%s), want a POST naming the skill", rep.method, rep.body,
			rep.contentType)
	}
	pr.pending(300*time.Millisecond, "the route still holds the report")
	rep.answer <- http.StatusNoContent
	pr.expect("after", 10*time.Second)
}

// ONLY A LOAD THAT HAPPENED IS REPORTED: another tool's answer, a skill that
// failed to load, and a load that names no skill post nothing, and hold
// nothing up.
func TestTheSkillsPluginReportsOnlyALoadThatHappened(t *testing.T) {
	t.Parallel()
	route, pr := newSkillRoute(t), startSkillsProbe(t)
	pr.setup(route.url())
	for _, event := range []struct {
		tool, status string
		input        map[string]any
	}{
		{"read", "completed", map[string]any{"filePath": "/src/main.go"}},
		{"skill", "error", map[string]any{"id": "crewlet:mcp-github"}},
		{"skill", "completed", map[string]any{}},
		{"skill", "completed", map[string]any{"id": 7}},
	} {
		pr.after(event.tool, event.status, event.input)
		pr.expect("after", 10*time.Second)
	}
	route.none(500*time.Millisecond, "nothing was loaded")
}

// A ROUTE THAT REFUSES THE RUN ENDS ITS REPORTS: a token the bridge no longer
// resolves (401), or a run with no skill route (404), will not be answered
// otherwise later, so no later load is posted. A route that failed any other
// way — a node stopping, a relay cut — is asked again at the next load.
func TestARouteThatRefusesTheRunEndsItsReports(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		route, pr := newSkillRoute(t), startSkillsProbe(t)
		pr.setup(route.url())
		pr.after(skillLoaded("crewlet:mcp-github"))
		route.next("the first load").answer <- status
		pr.expect("after", 10*time.Second)
		pr.after(skillLoaded("crewlet:getting-unstuck"))
		pr.expect("after", 10*time.Second)
		route.none(500*time.Millisecond, "the route refused the run")
	}

	route, pr := newSkillRoute(t), startSkillsProbe(t)
	pr.setup(route.url())
	pr.after(skillLoaded("crewlet:mcp-github"))
	route.next("the first load").answer <- http.StatusServiceUnavailable
	pr.expect("after", 10*time.Second)
	pr.after(skillLoaded("crewlet:mcp-github"))
	route.next("the load again").answer <- http.StatusNoContent
	pr.expect("after", 10*time.Second)
}

// A REPORT NOBODY ANSWERS LETS THE MODEL GO ON: the hook gives the route five
// seconds and resolves, letting go of its request, so a bridge that hangs costs
// the run a pause rather than its next request.
func TestAReportNobodyAnswersLetsTheModelGoOn(t *testing.T) {
	t.Parallel()
	route, pr := newSkillRoute(t), startSkillsProbe(t)
	pr.setup(route.url())
	pr.after(skillLoaded("crewlet:mcp-github"))
	rep := route.next("the load")
	pr.expect("after", 10*time.Second)
	select {
	case <-rep.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook resolved and the plugin still holds its report")
	}
}

// A RUN WIRED TO NO SKILL ROUTE HOOKS NOTHING, and the cleanup a wired one
// returns lets its hook go.
func TestTheSkillsPluginHooksOnlyARunWiredToARoute(t *testing.T) {
	t.Parallel()
	pr := startSkillsProbe(t)
	if hooked := pr.setup(""); len(hooked) != 0 {
		t.Errorf("a plugin wired to no route registered %v", hooked)
	}

	route, wired := newSkillRoute(t), startSkillsProbe(t)
	wired.setup(route.url())
	wired.send(map[string]any{"op": "cleanup"})
	if r := wired.expect("disposed", 10*time.Second); r["name"] != "execute.after" {
		t.Errorf("the cleanup let go of %v, want the execute.after hook", r["name"])
	}
	wired.expect("cleaned", 10*time.Second)
}
