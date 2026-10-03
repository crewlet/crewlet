package configapi_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
)

// TestNoChartCollectionIsAddressedHere.
//
// A seat and a unit are the org chart's, written through `/chart/*`, and no
// revision carries either — so this surface lists, reads and writes neither,
// and a path naming one is a route it does not serve. Served, even read-only,
// it would be a second address for the chart that answered from a document
// the chart is never in.
//
// DERIVED FROM THE CHART'S OWN KEYS rather than from the two names this used
// to serve, so a chart collection added to the entity table under any name is
// caught, not only `roles` and `units` coming back.
func TestNoChartCollectionIsAddressedHere(t *testing.T) {
	t.Parallel()
	chartKeys := config.ChartKeys()
	if len(chartKeys) == 0 {
		t.Fatal("the chart owns no top-level key, so this case certifies nothing")
	}
	for _, key := range chartKeys {
		if slices.Contains(configapi.EntityKinds(), key) {
			t.Errorf("/config addresses %q, which is the org chart's", key)
		}
	}

	s := newSurface(t)
	s.seed(t, companyDoc)
	before := s.activeDocument(t)
	for _, key := range []string{"roles", "units"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			res := s.do(t, method, "/config/"+key+"/ceo", `{"name":"CEO","handle":"ceo"}`,
				map[string]string{"X-Summary": "edit it here"})
			if res.Code != http.StatusNotFound || decode(t, res)["error"] != "no_route" {
				t.Errorf("%s /config/%s/ceo = %d %s, want 404 no_route: the chart is "+
					"not a collection of the settings", method, key, res.Code, res.Body)
			}
		}
	}
	if after := s.activeDocument(t); after != before {
		t.Errorf("a request for a chart collection changed the active document:\n%s", after)
	}

	// THE CONTROL: the collections this surface does address still answer,
	// or every 404 above would pass over a surface that answers nothing.
	if res := s.do(t, http.MethodGet, "/config/llm-providers/zulu", "", nil); res.Code != http.StatusOK {
		t.Errorf("GET /config/llm-providers/zulu = %d, want 200: %s", res.Code, res.Body)
	}
}

// A PUT CARRYING THE CHART IS REFUSED WHOLE, NAMING EVERY CHART KEY IT HOLDS.
//
// The settings half of the same body is not written either: a write that kept
// the settings and dropped the chart would answer 201 for a document whose new
// seat is nowhere, which is the failure the door exists to prevent. And the
// settings ALONE still land, which is what makes the refusal a redirection
// rather than a wall. Mutation: drop the door from [configapi.Service]'s put
// and the chart-carrying body is stored and answered 201.
func TestAPutCarryingTheChartIsRefusedWhole(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)
	before := s.activeDocument(t)

	withChart := strings.Replace(companyDoc, "name: Acme", "name: Acme\nmission: hire an engineer", 1) + `
roles:
  - {name: CEO, handle: ceo, llm: zulu}
units:
  - name: Engineering
    id: engineering
    roles:
      - {name: Engineer, handle: engineer, llm: zulu}
`
	res := s.do(t, http.MethodPut, "/config", withChart, map[string]string{"X-Summary": "hire"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("a PUT carrying the chart = %d, want 400: %s", res.Code, res.Body)
	}
	answer := decode(t, res)
	if answer["error"] != "chart_not_writable_here" {
		t.Errorf("error = %v, want chart_not_writable_here: %v", answer["error"], answer)
	}
	fields, _ := answer["fields"].([]any)
	if !slices.Equal(fields, []any{"roles", "units"}) {
		t.Errorf("fields = %v, want both chart keys the body carried", answer["fields"])
	}
	if hint, _ := answer["hint"].(string); !strings.Contains(hint, configapi.ChartRoutes.Batch) {
		t.Errorf("the refusal does not name the chart's routes: %v", answer)
	}
	if after := s.activeDocument(t); after != before {
		t.Errorf("a refused PUT changed the active document — its settings half "+
			"landed without its chart:\n%s", after)
	}

	// THE CONTROL: the same settings without the chart are an ordinary write.
	settingsOnly := strings.Replace(companyDoc, "name: Acme", "name: Acme\nmission: hire an engineer", 1)
	if res := s.do(t, http.MethodPut, "/config", settingsOnly,
		map[string]string{"X-Summary": "the settings alone"}); res.Code != http.StatusCreated {
		t.Fatalf("a settings-only PUT = %d, want 201: %s", res.Code, res.Body)
	}
}

// A PATCH NAMING THE CHART IS REFUSED EVEN WHEN THE MERGE WOULD CARRY NONE.
//
// `{"units": null}` asks to remove the chart and merges onto a settings
// revision as nothing at all, so a door judging the MERGED document would
// answer 201 for a write that did nothing — the silently-dropped half the door
// exists to refuse. It judges what the caller sent. Mutation: judge the merge
// in [configapi.Service]'s patch and the null case answers 201.
func TestAPatchNamingTheChartIsRefusedWhateverItMerges(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"units": [{"name": "Engineering", "id": "engineering"}]}`,
		`{"units": null}`,
		`{"roles": null, "mission": "ship it"}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, companyDoc)
			before := s.activeDocument(t)

			res := s.do(t, http.MethodPatch, "/config", body, summaryHeader)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s = %d, want 400: %s", body, res.Code, res.Body)
			}
			answer := decode(t, res)
			if answer["error"] != "chart_not_writable_here" {
				t.Errorf("error = %v, want chart_not_writable_here: %v", answer["error"], answer)
			}
			// THE REFUSAL POINTS AT SOMETHING THAT WORKS, and the walk in
			// internal/api holds every route it names against what the
			// chart surface mounts.
			if hint, _ := answer["hint"].(string); !strings.Contains(hint, configapi.ChartRoutes.Batch) {
				t.Errorf("the refusal does not name the chart's routes: %v", answer)
			}
			if after := s.activeDocument(t); after != before {
				t.Errorf("a refused patch changed the active document:\n%s", after)
			}
		})
	}

	// THE CONTROL: a patch naming only a setting lands, so the refusals above
	// are about the chart and not about patching.
	s := newSurface(t)
	s.seed(t, companyDoc)
	res := s.do(t, http.MethodPatch, "/config", `{"mission": "ship it"}`, summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("a settings-only PATCH = %d, want 201: %s", res.Code, res.Body)
	}
}
