package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/setup"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)

// The routes the atlas read lists.
const (
	monitorsPath   = "/api/v1/monitor"
	servicesPath   = "/api/v2/services/definitions"
	hostsPath      = "/api/v1/hosts"
	syntheticsPath = "/api/v1/synthetics/tests"
)

// Values the fake carries in fields the read never decodes.
const (
	canaryQuery   = "atlas-canary-monitor-query"
	canaryMessage = "atlas-canary-monitor-message"
	canaryPerson  = "atlas-canary-person@acme.example"
	canaryHeader  = "atlas-canary-request-header"
)

// inventory is a Datadog organization as the atlas reads it. Each listing is
// served a page at a time, and a route named in refuse answers that status.
type inventory struct {
	org        string
	monitors   []map[string]any
	services   []map[string]any
	hosts      []map[string]any
	tests      []map[string]any
	refuse     map[string]int
	limitFirst string

	mu    sync.Mutex
	calls []string
}

func (f *inventory) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	limited := f.limitFirst == r.URL.Path
	if limited {
		f.limitFirst = ""
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if limited {
		w.Header().Set("X-RateLimit-Reset", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":["Rate limit reached"]}`))
		return
	}
	if status := f.refuse[r.URL.Path]; status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
		return
	}
	q := r.URL.Query()
	num := func(name string) int { n, _ := strconv.Atoi(q.Get(name)); return n }
	page := func(rows []map[string]any, start, size int) []map[string]any {
		if start >= len(rows) {
			return []map[string]any{}
		}
		return rows[start:min(len(rows), start+size)]
	}
	var body any
	switch r.URL.Path {
	case orgPath:
		body = map[string]any{"orgs": []map[string]any{{"name": f.org, "public_id": "abc123"}}}
	case monitorsPath:
		body = page(f.monitors, num("page")*num("page_size"), num("page_size"))
	case servicesPath:
		if q.Get("schema_version") != "v2.2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rows := page(f.services, num("page[number]")*num("page[size]"), num("page[size]"))
		data := make([]map[string]any, 0, len(rows))
		for _, schema := range rows {
			data = append(data, map[string]any{"type": "service-definition",
				"attributes": map[string]any{"schema": schema, "meta": map[string]any{"origin": canaryPerson}}})
		}
		body = map[string]any{"data": data}
	case hostsPath:
		body = map[string]any{"host_list": page(f.hosts, num("start"), num("count")),
			"total_matching": len(f.hosts)}
	case syntheticsPath:
		body = map[string]any{"tests": page(f.tests, num("page_number")*num("page_size"), num("page_size"))}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *inventory) sent(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "GET "+path {
			n++
		}
	}
	return n
}

// acme is an organization with one alerting monitor on a service and a host,
// one healthy monitor, a Synthetic test that warns, a catalogue of one
// service, and two hosts.
func acme() *inventory {
	return &inventory{
		org: "Acme Inc",
		monitors: []map[string]any{
			{"id": 456, "name": "[prod] API 5xx rate > 5%", "type": "metric alert", "overall_state": "Alert",
				"overall_state_modified": "2026-11-12T06:10:00+00:00", "query": canaryQuery,
				"message": canaryMessage + " @" + canaryPerson, "creator": map[string]any{"email": canaryPerson,
					"handle": canaryPerson}, "tags": []string{"service:api", "env:prod", "host:web-1", "team:core"}},
			{"id": 457, "name": "Queue depth", "overall_state": "OK",
				"overall_state_modified": "2026-11-01T00:00:00+00:00", "tags": []string{"service:worker"}},
			{"id": 900, "name": "[Synthetics] API health", "type": "synthetics alert", "overall_state": "Warn",
				"overall_state_modified": "2026-11-12T06:50:00+00:00", "tags": []string{}},
		},
		services: []map[string]any{
			{"schema-version": "v2.2", "dd-service": "api", "team": "core", "description": canaryMessage,
				"contacts": []map[string]any{{"type": "email", "contact": canaryPerson}},
				"links": []map[string]any{
					{"name": "source", "type": "repo", "provider": "github", "url": "https://github.com/acme/api"},
					{"name": "runbook", "type": "runbook", "url": "https://docs.acme.example/runbook"},
				}},
		},
		hosts: []map[string]any{
			{"name": "web-1", "host_name": "web-1", "aliases": []string{"i-0abc"}, "up": true,
				"tags_by_source": map[string][]string{"Datadog": {"env:prod", "owner:" + canaryPerson}},
				"meta":           map[string]any{"gohai": canaryHeader}},
			{"name": "db-1", "tags_by_source": map[string][]string{"Datadog": {"env:prod"}, "Chef": {"env:staging"}}},
		},
		tests: []map[string]any{
			{"public_id": "abc-def-ghi", "name": "API health", "type": "api", "status": "live", "monitor_id": 900,
				"creator": map[string]any{"email": canaryPerson}, "message": canaryMessage,
				"config": map[string]any{"request": map[string]any{"method": "GET",
					"url":     "https://ops:hunter2@api.acme.example/health?token=" + canaryHeader + "#frag",
					"headers": map[string]string{"Authorization": canaryHeader}}}},
			{"public_id": "tcp-one-one", "type": "api", "subtype": "tcp", "status": "live", "monitor_id": 900,
				"config": map[string]any{"request": map[string]any{"host": "9.9.9.9", "port": 443}}},
			{"public_id": "off-off-off", "type": "api", "status": "paused", "monitor_id": 457,
				"config": map[string]any{"request": map[string]any{"url": "https://paused.acme.example/"}}},
		},
		refuse: map[string]int{},
	}
}

func (f *inventory) input(t *testing.T) collect.Input {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	company := &config.Company{Name: "Acme"}
	company.Integrations.Datadog = &config.Datadog{Enabled: true, Provisioning: &config.DatadogProvisioning{
		Site: "datadoghq.eu", APIKey: "${DD_API_KEY}", AppKey: "${DD_APP_KEY}",
	}}
	return collect.Input{Config: company, Now: atlasNow,
		Env:  config.NewResolver(config.MapSource{"DD_API_KEY": fakeAPIKey, "DD_APP_KEY": fakeAppKey}),
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
}

func datadogSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindDatadog)
	if !ok {
		t.Fatal("no Datadog source is registered")
	}
	return src
}

func (f *inventory) read(t *testing.T) collect.Read {
	t.Helper()
	read, err := datadogSource(t).Collect(t.Context(), f.input(t))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

func entry(t *testing.T, v atlas.View, k atlas.Kind, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(k, key).ID())
	if !ok {
		t.Fatalf("%s:%s is not in the read", k, key)
	}
	return e
}

// to is the keys of the entries e relates to through r, sorted.
func to(v atlas.View, e atlas.Entry, r atlas.Relation) []string {
	var out []string
	for _, ed := range e.Relations {
		if ed.Relation != r {
			continue
		}
		if target, ok := v.Find(ed.To); ok {
			out = append(out, target.Ref.Key)
		}
	}
	slices.Sort(out)
	return out
}

func fact(e atlas.Entry, f atlas.Fact) string {
	for _, fv := range e.Facts {
		if fv.Fact == f {
			return fv.Value.Text
		}
	}
	return ""
}

func signals(e atlas.Entry) []atlas.SignalKind {
	var out []atlas.SignalKind
	for _, s := range e.Signals {
		out = append(out, s.Kind)
	}
	return out
}

// view is the read as the atlas folds it, with its signals joined.
func view(read collect.Read) atlas.View {
	return atlas.Join(atlas.Fold(read.Claims, atlasNow), atlas.Joined{}, atlasNow)
}

// THE READ CLAIMS THE ORGANIZATION, EVERY MONITOR WITH ITS STATE, THE
// CATALOGUE, THE HOSTS, AND WHAT EACH MONITOR AND TEST WATCHES: a monitor
// watches the service and the host its tags name, by those exact keys, and a
// running Synthetic test's monitor watches the address it requests. Nothing a
// payload carries beside the mapped fields reaches the read: no query, no
// message, no creator, no contact, no request header.
func TestTheAtlasReadsMonitorsServicesHostsAndChecks(t *testing.T) {
	t.Parallel()
	f := acme()
	read := f.read(t)
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryQuery, canaryMessage, canaryPerson, canaryHeader, "hunter2", "i-0abc",
		"team:core", "runbook"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q, which the read never maps, reached it: %s", canary, out)
		}
	}
	if len(read.Withheld) != 0 || len(read.Unread) != 0 {
		t.Errorf("the read withheld %+v and left unread %+v", read.Withheld, read.Unread)
	}
	// The organization, and one page of each of the four listings.
	if read.Calls != 5 {
		t.Errorf("the read made %d calls, want 5", read.Calls)
	}
	v := view(read)
	account := entry(t, v, atlas.KindAccount, "datadog/datadoghq.eu")
	if fact(account, atlas.FactAccountName) != "Acme Inc" || fact(account, atlas.FactAccountRef) != "abc123" {
		t.Errorf("the organization: %+v", account.Facts)
	}
	alerting := entry(t, v, atlas.KindMonitor, "datadog/monitor/456")
	if alerting.Name != "(prod) API 5xx rate 5%" {
		t.Errorf("the monitor's name: %q", alerting.Name)
	}
	if fact(alerting, atlas.FactMonitorState) != atlas.MonitorAlert ||
		fact(alerting, atlas.FactStateSince) != "2026-11-12T06:10:00Z" {
		t.Errorf("the alerting monitor's state: %+v", alerting.Facts)
	}
	if got := to(v, alerting, atlas.RelationBelongsTo); !slices.Equal(got, []string{account.Ref.Key}) {
		t.Errorf("the monitor belongs to %v", got)
	}
	healthy := entry(t, v, atlas.KindMonitor, "datadog/monitor/457")
	if fact(healthy, atlas.FactMonitorState) != atlas.MonitorOK || fact(healthy, atlas.FactStateSince) != "" {
		t.Errorf("a healthy monitor's state, which carries no instant: %+v", healthy.Facts)
	}
	api := entry(t, v, atlas.KindService, "api")
	if got := to(v, api, atlas.RelationMonitoredBy); !slices.Equal(got, []string{"datadog/datadoghq.eu", "datadog/monitor/456"}) {
		t.Errorf("the catalogue's service is watched by %v", got)
	}
	web := entry(t, v, atlas.KindHost, "datadog/host/web-1")
	if got := to(v, web, atlas.RelationMonitoredBy); !slices.Equal(got, []string{"datadog/datadoghq.eu", "datadog/monitor/456"}) {
		t.Errorf("the host is watched by %v", got)
	}
	if web.Name != "web-1" || fact(web, atlas.FactEnvironment) != "production" {
		t.Errorf("the host: %q %+v", web.Name, web.Facts)
	}
	if db := entry(t, v, atlas.KindHost, "datadog/host/db-1"); fact(db, atlas.FactEnvironment) != "" {
		t.Errorf("a host whose sources disagree on its environment was given one: %+v", db.Facts)
	}
	// A service only a monitor's tag names is related, and never claimed.
	worker := entry(t, v, atlas.KindService, "worker")
	if got := to(v, worker, atlas.RelationMonitoredBy); !slices.Equal(got, []string{"datadog/monitor/457"}) {
		t.Errorf("the tagged service is watched by %v", got)
	}
	for _, c := range read.Claims {
		owned := c.Subject.Kind == atlas.KindAccount || c.Subject.Kind == atlas.KindMonitor ||
			c.Subject == api.Ref || strings.HasPrefix(c.Subject.Key, "datadog/host/")
		if c.Facet.Kind == atlas.FacetExists && !owned {
			t.Errorf("the read says %s exists, which does not live at Datadog", c.Subject)
		}
	}
	// The test's address without its credentials, query and fragment, and
	// the name and the server the two running tests check.
	for _, watched := range []atlas.Ref{
		atlas.MustRef(atlas.KindEndpoint, "https://api.acme.example/health"),
		atlas.MustRef(atlas.KindHostname, "api.acme.example"),
		atlas.MustRef(atlas.KindHost, "addr/9.9.9.9"),
	} {
		e := entry(t, v, watched.Kind, watched.Key)
		if got := to(v, e, atlas.RelationMonitoredBy); !slices.Equal(got, []string{"datadog/monitor/900"}) {
			t.Errorf("%s is watched by %v", watched, got)
		}
	}
	if _, ok := v.Find(atlas.MustRef(atlas.KindHostname, "paused.acme.example").ID()); ok {
		t.Error("a paused test watches a name")
	}
}

// A MONITOR'S STATE IS THE HEALTH OF WHAT IT WATCHES: an alerting monitor
// makes itself, its service and its host alerting, a warning test makes the
// address it checks need a look, and a healthy monitor raises nothing.
//
// Mutation: drop the monitored_by relation from readMonitors, or map Alert to
// ok in monitorState, and the service stays quiet.
func TestAnAlertingMonitorIsTheHealthOfWhatItWatches(t *testing.T) {
	t.Parallel()
	v := view(acme().read(t))
	for _, c := range []struct {
		kind   atlas.Kind
		key    string
		health atlas.Health
		signal []atlas.SignalKind
	}{
		{atlas.KindMonitor, "datadog/monitor/456", atlas.HealthAlerting, []atlas.SignalKind{atlas.SignalMonitorAlerting}},
		{atlas.KindService, "api", atlas.HealthAlerting, []atlas.SignalKind{atlas.SignalMonitorAlerting}},
		{atlas.KindHost, "datadog/host/web-1", atlas.HealthAlerting, []atlas.SignalKind{atlas.SignalMonitorAlerting}},
		{atlas.KindHostname, "api.acme.example", atlas.HealthNeedsLook, []atlas.SignalKind{atlas.SignalMonitorWarning}},
		{atlas.KindService, "worker", atlas.HealthQuiet, nil},
		{atlas.KindHost, "datadog/host/db-1", atlas.HealthQuiet, nil},
		{atlas.KindAccount, "datadog/datadoghq.eu", atlas.HealthQuiet, nil},
	} {
		e := entry(t, v, c.kind, c.key)
		if e.Health() != c.health || !slices.Equal(signals(e), c.signal) {
			t.Errorf("%s is %s with %v, want %s with %v", e.Ref, e.Health(), signals(e), c.health, c.signal)
		}
	}
	since := entry(t, v, atlas.KindService, "api").Signals[0].Since
	if want := time.Date(2026, 11, 12, 6, 10, 0, 0, time.UTC); !since.Equal(want) {
		t.Errorf("the service has been alerting since %s, want Datadog's own instant %s", since, want)
	}
}

// A LISTING THE KEYS MAY NOT READ IS A FINDING, NEVER A FAULT: the read
// completes with the listings it may read, names the refused one with the
// permission that restores it, and what an earlier read of that listing
// recorded is neither withdrawn nor missed, however long the refusal lasts.
//
// Mutation: return the 403 from readAtlas as an error, or leave Holds off the
// finding, and the read fails or the hosts are missed and then retired.
func TestARefusedListingIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	f := acme()
	f.refuse[hostsPath] = http.StatusForbidden
	read := f.read(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "hosts" ||
		!strings.Contains(read.Unread[0].Detail, "hosts_read") {
		t.Fatalf("the refused listing: %+v", read.Unread)
	}
	if strings.Contains(read.Unread[0].Detail, "Forbidden") {
		t.Errorf("the finding repeats the vendor's answer: %q", read.Unread[0].Detail)
	}
	if out, err := json.Marshal(read); err != nil || !strings.Contains(string(out), "hosts_read") {
		t.Errorf("a read with a refused listing encodes as %s, %v", out, err)
	}
	v := view(read)
	if e := entry(t, v, atlas.KindMonitor, "datadog/monitor/456"); fact(e, atlas.FactMonitorState) != atlas.MonitorAlert {
		t.Errorf("the monitors were not read beside the refused hosts: %+v", e.Facts)
	}
	status := collect.Status{Kind: integration.KindDatadog}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
			t.Fatalf("at %s a refused listing recorded %+v and missed %+v", at, plan.Records, plan.Status.Missing)
		}
		if len(plan.Status.Unread) != 1 || plan.Status.Unread[0].Part != "hosts" {
			t.Fatalf("the standing's findings: %+v", plan.Status.Unread)
		}
		status = plan.Status
	}
	// Every listing refused: still a read, of the organization alone.
	for _, path := range []string{monitorsPath, servicesPath, hostsPath, syntheticsPath} {
		f.refuse[path] = http.StatusForbidden
	}
	none := f.read(t)
	if len(none.Unread) != 4 {
		t.Errorf("four refused listings left %d findings", len(none.Unread))
	}
	if plan := collect.Decide(whole.Claims, none, collect.Status{Kind: integration.KindDatadog}, atlasNow.Add(48*time.Hour)); len(plan.Records) != 0 {
		t.Errorf("a read refused every listing recorded %+v", plan.Records)
	}
}

// A MONITOR SOMEBODY DELETED RETIRES ITSELF AND NOTHING IT WATCHED: after two
// complete reads a day apart the monitor is denied, and the service only its
// tag named has that relation withdrawn, with no denial of the service.
//
// Mutation: claim the tagged service with Ref rather than Names, and the
// read denies an entry another source may hold.
func TestADeletedMonitorRetiresNothingItWatched(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	f := acme()
	f.monitors = slices.DeleteFunc(f.monitors, func(m map[string]any) bool { return m["id"] == 457 })
	read := f.read(t)
	first := collect.Decide(whole.Claims, read, collect.Status{Kind: integration.KindDatadog}, atlasNow.Add(time.Hour))
	if len(first.Records) != 0 || len(first.Status.Missing) == 0 {
		t.Fatalf("the first miss recorded %+v and missed %+v", first.Records, first.Status.Missing)
	}
	second := collect.Decide(whole.Claims, read, first.Status, atlasNow.Add(26*time.Hour))
	monitor, worker := atlas.MustRef(atlas.KindMonitor, "datadog/monitor/457"), atlas.MustRef(atlas.KindService, "worker")
	seen := map[atlas.Ref]bool{}
	for _, rec := range second.Records {
		seen[rec.Entry] = true
		switch rec.Entry {
		case monitor:
			if !rec.Retires || len(rec.Claims) != 1 || !rec.Claims[0].Denied {
				t.Errorf("the deleted monitor: %+v", rec)
			}
		case worker:
			if rec.Retires || len(rec.Claims) != 0 || len(rec.Withdrawn) != 1 {
				t.Errorf("the service the monitor watched: %+v", rec)
			}
		default:
			t.Errorf("a deleted monitor recorded %s", rec.Entry)
		}
	}
	if !seen[monitor] || !seen[worker] || second.Status.Retired != 1 {
		t.Errorf("recorded %v and retired %d", seen, second.Status.Retired)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM DATADOG: every shape the
// redaction pass knows, planted in every field the read maps and in the ones
// it never reads, leaves no piece of itself in the read, and what the mapped
// fields refused is counted.
//
// Mutation: map a monitor's message or a test's headers into a claim, cut a
// name before the builder judges it, or skip the builder's grammar on a
// value, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromDatadog(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		// A name long enough that cutting it to a label's length would cut
		// the shape short of its end, where no rule knows it any more.
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		f := &inventory{
			org: s,
			monitors: []map[string]any{
				{"id": 1, "name": s, "overall_state": s, "overall_state_modified": s, "query": s, "message": s,
					"type": s, "creator": map[string]any{"email": s, "handle": s, "name": s},
					"tags":    []string{"service:" + s, "host:" + s, "env:" + s, "team:" + s, s, s + ":" + s},
					"options": map[string]any{"escalation_message": s}},
				{"id": 2, "name": long, "overall_state": "Alert", "overall_state_modified": "2026-11-12T06:10:00+00:00"},
			},
			services: []map[string]any{
				{"schema-version": "v2.2", "dd-service": s, "team": s, "description": s, "application": s,
					"contacts": []map[string]any{{"type": "email", "contact": s, "name": s}},
					"tags":     []string{s}, "extensions": map[string]any{"note": s}},
				{"schema-version": "v2.2", "dd-service": "api", "links": []map[string]any{
					{"name": s, "type": "repo", "provider": s, "url": s},
					{"name": s, "type": "repo", "url": "https://github.com/acme/" + s},
					{"name": s, "type": "repo", "url": "https://" + s + "@github.com/acme/api?key=" + s},
					{"name": s, "type": "doc", "url": "https://docs.acme.example/" + s},
				}},
			},
			hosts: []map[string]any{
				{"name": s, "host_name": s, "aws_name": s, "aliases": []string{s}, "apps": []string{s},
					"tags_by_source": map[string][]string{s: {"env:" + s, s}}, "meta": map[string]any{"gohai": s}},
				{"name": "web-1", "tags_by_source": map[string][]string{"Datadog": {"env:" + s, "owner:" + s}}},
			},
			tests: []map[string]any{
				{"public_id": s, "name": s, "status": "live", "monitor_id": 2, "message": s, "tags": []string{s},
					"creator": map[string]any{"email": s},
					"config": map[string]any{"request": map[string]any{"url": s, "host": s, "body": s,
						"headers": map[string]string{"Authorization": s}, "basicAuth": map[string]string{"password": s}},
						"variables": []map[string]any{{"name": s, "pattern": s}}}},
				{"public_id": "abc-def-ghi", "status": "live", "monitor_id": 2,
					"config": map[string]any{"request": map[string]any{"url": "https://api.acme.example/" + s}}},
				{"public_id": "jkl-mno-pqr", "status": "live", "monitor_id": 2,
					"config": map[string]any{"request": map[string]any{"host": s}}},
				{"public_id": "stu-vwx-yz0", "status": "live", "monitor_id": 2,
					"config": map[string]any{"request": map[string]any{
						"url": "https://ops:" + s + "@www.acme.example/ok?token=" + s + "#" + s}}},
			},
			refuse: map[string]int{},
		}
		read := f.read(t)
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		// The shape's second half, and a piece of its body near its start,
		// which is what a name cut short of the shape's end would keep.
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if strings.Contains(string(out), piece) {
				t.Errorf("%s: a piece of the shape reached the read: %s", shape.Name, out)
			}
		}
		// The organization's name, two monitor names, the two tags the read
		// maps, a service's name, two repository addresses, a host's name
		// and three checked addresses are each refused where they sat.
		if len(read.Withheld) < 12 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
		// What sat beside a credential is still read: an address is kept
		// without the user information and the query that carried one. A
		// shape with a space in it makes the address no address at all.
		if strings.ContainsAny(s, " \n") {
			continue
		}
		v := view(read)
		if e, ok := v.Find(atlas.MustRef(atlas.KindEndpoint, "https://www.acme.example/ok").ID()); !ok || len(e.Signals) != 1 {
			t.Errorf("%s: the address beside the credential was not read: %+v", shape.Name, e)
		}
	}
}

// A NAME DATADOG'S CONVENTIONS WRITE IS A LABEL THE ATLAS TAKES: brackets
// become parentheses, the other markup characters go, and a long name is cut
// to a label's length.
func TestAMonitorsNameIsMadeALabel(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"[prod] API 5xx rate > 5%": "(prod) API 5xx rate 5%",
		"  plain   name ":          "plain name",
		"`disk` <full>":            "disk full",
	} {
		if got := datadog.PlainName(raw); got != want {
			t.Errorf("%q is %q, want %q", raw, got, want)
		}
	}
	long := datadog.PlainName(strings.Repeat("é", 300))
	if n := len([]rune(long)); n != atlas.MaxLabelRunes {
		t.Errorf("a long name is cut to %d characters, want %d", n, atlas.MaxLabelRunes)
	}
	if _, err := atlas.FactName.Value(long); err != nil {
		t.Errorf("the cut name is not a label: %v", err)
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a listing that fails for
// any reason but a refusal fails the whole read, so nothing is retired on a
// listing cut short.
func TestAListingThatFailsFailsTheRead(t *testing.T) {
	t.Parallel()
	for _, path := range []string{monitorsPath, servicesPath, hostsPath, syntheticsPath} {
		f := acme()
		f.refuse[path] = http.StatusServiceUnavailable
		read, err := datadogSource(t).Collect(t.Context(), f.input(t))
		if err == nil || len(read.Claims) != 0 {
			t.Errorf("%s: a listing that would not answer left %d claims and %v", path, len(read.Claims), err)
		}
	}
	// The organization's read is the proof of the keys, and its refusal is
	// the credential's.
	f := acme()
	f.refuse[orgPath] = http.StatusForbidden
	if _, err := datadogSource(t).Collect(t.Context(), f.input(t)); !errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("refused keys: %v", err)
	}
	if f.sent(monitorsPath) != 0 {
		t.Error("a read whose keys were refused went on to list")
	}
}

// EVERY LISTING IS WALKED TO ITS END: a listing longer than a page is read
// page after page, each from where the last ended.
func TestAListingIsWalkedToItsEnd(t *testing.T) {
	t.Parallel()
	f := acme()
	f.monitors, f.hosts, f.services, f.tests = nil, nil, nil, nil
	for i := range 250 {
		f.monitors = append(f.monitors, map[string]any{"id": 1000 + i, "name": fmt.Sprintf("monitor %d", i),
			"overall_state": "OK"})
		f.hosts = append(f.hosts, map[string]any{"name": fmt.Sprintf("web-%d", i)})
		f.services = append(f.services, map[string]any{"dd-service": fmt.Sprintf("svc-%d", i)})
		f.tests = append(f.tests, map[string]any{"public_id": fmt.Sprintf("t-%d", i), "status": "live",
			"monitor_id": 1000 + i, "config": map[string]any{"request": map[string]any{
				"url": fmt.Sprintf("https://s%d.acme.example/", i)}}})
	}
	read := f.read(t)
	v := view(read)
	count := map[atlas.Kind]int{}
	for _, e := range v.Entries {
		count[e.Ref.Kind]++
	}
	if count[atlas.KindMonitor] != 250 || count[atlas.KindHost] != 250 || count[atlas.KindService] != 250 ||
		count[atlas.KindEndpoint] != 250 {
		t.Errorf("read %v, want 250 of each", count)
	}
	// Three pages of each listing, and the organization.
	if read.Calls != 13 {
		t.Errorf("the read made %d calls, want 13", read.Calls)
	}
}

// THE READ SHARES THE CLIENT'S RATE LIMIT HANDLING: a listing Datadog limits
// is waited out as it asks and sent again, and the read completes whole.
func TestTheAtlasReadWaitsOutARateLimit(t *testing.T) {
	t.Parallel()
	f := acme()
	f.limitFirst = hostsPath
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := datadog.NewClient(datadog.ClientOptions{Site: "datadoghq.eu",
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}})
	if err != nil {
		t.Fatal(err)
	}
	var waited []time.Duration
	datadog.SetWait(client, func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	})
	read, err := datadog.ReadAtlas(t.Context(), client, datadog.Credentials{APIKey: fakeAPIKey, AppKey: fakeAppKey},
		"datadoghq.eu", atlasNow)
	if err != nil {
		t.Fatalf("a limited listing failed the read: %v", err)
	}
	if !slices.Equal(waited, []time.Duration{7 * time.Second}) {
		t.Errorf("the read waited %v, want the reset Datadog named", waited)
	}
	if read.Calls != 6 || len(read.Unread) != 0 {
		t.Errorf("the read made %d calls and left unread %+v, want the limited call counted and sent again",
			read.Calls, read.Unread)
	}
	if _, ok := view(read).Find(atlas.MustRef(atlas.KindHost, "datadog/host/web-1").ID()); !ok {
		t.Error("the limited listing was not read")
	}
}

// A COMPANY THAT DOES NOT CONNECT DATADOG, OR WHOSE KEYS DO NOT RESOLVE, IS
// NOT CONFIGURED: the duty forgets the source rather than recording a fault.
func TestAnUnconnectedDatadogIsNotConfigured(t *testing.T) {
	t.Parallel()
	src := datadogSource(t)
	none := &config.Company{Name: "Acme"}
	if _, err := src.Collect(t.Context(), collect.Input{Config: none, Now: atlasNow}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no block: %v", err)
	}
	f := acme()
	in := f.input(t)
	in.Env = config.NewResolver(config.MapSource{"DD_API_KEY": fakeAPIKey})
	if _, err := src.Collect(t.Context(), in); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("an unresolved application key: %v", err)
	}
	if len(f.calls) != 0 {
		t.Error("an unconfigured read reached the vendor")
	}
}

// THE CREDENTIAL CHECK SAYS WHAT THE ATLAS CANNOT READ, AS A WARNING ON A
// WORKING VERDICT: a listing the application key may not read never refuses
// the keys, and names the permission that restores it.
func TestADatadogCheckWarnsWhatTheAtlasCannotRead(t *testing.T) {
	t.Parallel()
	got, fake := checkAgainst(t, 0, monitorsPath, forbidden)
	want := "Works. Both keys are valid. | Organization: Acme | Site: datadoghq.eu | " +
		"They can read: Users, roles and webhooks | The atlas cannot show the organization's monitors. " +
		"Give the application key's owner a role with the monitors_read permission."
	if got.Outcome != setup.VerifyWorks || said(got) != want {
		t.Errorf("answered %+v, want %q", got, want)
	}
	listed := 0
	for _, r := range fake.requests() {
		if r.Method != http.MethodGet {
			t.Errorf("the check sent %s %s", r.Method, r.URL.Path)
		}
		if slices.Contains([]string{monitorsPath, servicesPath, hostsPath, syntheticsPath}, r.URL.Path) {
			listed++
		}
	}
	if listed != 4 {
		t.Errorf("the check read %d of the atlas's four listings", listed)
	}
}
