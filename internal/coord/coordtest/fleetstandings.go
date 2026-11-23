package coordtest

import (
	"maps"
	"slices"
)

// ---- the integration reconcile status ---------------------------------- //

func (h *fleetHarness) putIntegration(kind, value string) {
	h.t.Helper()
	if err := h.f.PutIntegrationStatus(h.ctx, kind, []byte(value)); err != nil {
		h.t.Fatalf("PutIntegrationStatus(%q): %v", kind, err)
	}
}

func (h *fleetHarness) recordPass(kind, status string, findings []byte) {
	h.t.Helper()
	if err := h.f.RecordIntegrationPass(h.ctx, kind, []byte(status), findings); err != nil {
		h.t.Fatalf("RecordIntegrationPass(%q): %v", kind, err)
	}
}

func (h *fleetHarness) integrationStatus(kind string) (string, bool) {
	h.t.Helper()
	got, found, err := h.f.IntegrationStatus(h.ctx, kind)
	if err != nil {
		h.t.Fatalf("IntegrationStatus(%q): %v", kind, err)
	}
	return string(got), found
}

func (h *fleetHarness) integrationFindingsOf(kind string) (string, bool) {
	h.t.Helper()
	got, found, err := h.f.IntegrationFindingsOf(h.ctx, kind)
	if err != nil {
		h.t.Fatalf("IntegrationFindingsOf(%q): %v", kind, err)
	}
	return string(got), found
}

func (h *fleetHarness) integrations() map[string][]byte {
	h.t.Helper()
	got, err := h.f.IntegrationStatuses(h.ctx)
	if err != nil {
		h.t.Fatalf("IntegrationStatuses: %v", err)
	}
	return got
}

func (h *fleetHarness) integrationNames() []string {
	h.t.Helper()
	names := slices.Collect(maps.Keys(h.integrations()))
	slices.Sort(names)
	return names
}

var integrationCases = []fleetCase{
	{name: "a recorded status reads back byte for byte", fn: func(h *fleetHarness) {
		// The value is OPAQUE to this store: internal/integration
		// marshals a phase, an actor, a sentence and a findings list into
		// it, and a backend that re-encoded on the way through would
		// hand the reader a document its producer never wrote.
		const status = `{"kind":"gitlab","report":{"phase":"degraded","actor":"admin"}}`
		h.putIntegration("gitlab", status)

		got := h.integrations()
		if string(got["gitlab"]) != status {
			t := h.t
			t.Fatalf("read back %q, want %q", got["gitlab"], status)
		}
	}},

	{name: "a surface with no status is absent rather than empty", fn: func(h *fleetHarness) {
		// The loop reads a missing key as "never reconciled, so due now".
		// A backend that answered with a zero-length value instead would
		// make that surface parse as a status whose phase is the empty
		// string, which is not a phase.
		h.putIntegration("slack", `{"kind":"slack"}`)
		if _, present := h.integrations()["gitlab"]; present {
			h.t.Fatal("a surface nothing recorded came back present")
		}
	}},

	{name: "the last write wins", fn: func(h *fleetHarness) {
		// No compare-and-set, deliberately — but NOT because the worker
		// duty makes one node the only writer, which is the reason this
		// used to give and is not true: the dashboard records a pass's
		// outcome, stamps an endpoint and marks a disconnect on whichever
		// node served the request. What makes last-write-wins safe is the
		// SURFACE's own provisioning lease, held by every writer across
		// the read, the work and the write alike. See [coord.Integrations].
		h.putIntegration("gitlab", `{"attempts":1}`)
		h.putIntegration("gitlab", `{"attempts":2}`)
		if got := string(h.integrations()["gitlab"]); got != `{"attempts":2}` {
			h.t.Fatalf("read back %q, want the second write", got)
		}
	}},

	{name: "every recorded surface is listed", fn: func(h *fleetHarness) {
		h.putIntegration("gitlab", `{}`)
		h.putIntegration("slack", `{}`)
		h.putIntegration("datadog", `{}`)
		want := []string{"datadog", "gitlab", "slack"}
		if got := h.integrationNames(); !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "a deleted status is gone and the rest survive", fn: func(h *fleetHarness) {
		h.putIntegration("gitlab", `{}`)
		h.putIntegration("slack", `{}`)
		if err := h.f.DeleteIntegrationStatus(h.ctx, "gitlab"); err != nil {
			h.t.Fatalf("DeleteIntegrationStatus: %v", err)
		}
		want := []string{"slack"}
		if got := h.integrationNames(); !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "deleting a status that is not there is the outcome asked for", fn: func(h *fleetHarness) {
		// The loop forgets every surface the company no longer declares,
		// on every tick, so this is the common path rather than an edge
		// case. Raising here would fill an operator's log with warnings
		// about work that was already done.
		if err := h.f.DeleteIntegrationStatus(h.ctx, "never-recorded"); err != nil {
			h.t.Fatalf("deleting an absent status: %v", err)
		}
	}},

	{name: "a surface name that is not a bare word survives the round trip", fn: func(h *fleetHarness) {
		// The KV backend escapes a key on the way in and unescapes it on
		// the way out, and the mapping has to be injective in both
		// directions. Nothing in integration.Kinds needs escaping today,
		// which is exactly why a backend that got this wrong would go
		// unnoticed until the first surface that did.
		const kind = "self-hosted:gitlab.example"
		h.putIntegration(kind, `{"kind":"gitlab"}`)
		if _, present := h.integrations()[kind]; !present {
			h.t.Fatalf("listed %v, want it to contain %q", h.integrationNames(), kind)
		}
	}},

	{name: "a pass's findings and status read back by key and apart", fn: func(h *fleetHarness) {
		// THE STATUS AND THE FINDINGS ARE TWO RECORDS: a hot reader lists
		// or reads statuses, and only a reader that needs them reads the
		// findings, by key or in a pass of their own.
		const status, findings = `{"report":{"phase":"degraded"}}`, `{"findings":[{"kind":"grant_short"}]}`
		h.recordPass("gitlab", status, []byte(findings))
		if got, found := h.integrationStatus("gitlab"); !found || got != status {
			h.t.Errorf("the status by key reads %q (found %t), want %q", got, found, status)
		}
		if got, found := h.integrationFindingsOf("gitlab"); !found || got != findings {
			h.t.Errorf("the findings by key read %q (found %t), want %q", got, found, findings)
		}
		all, err := h.f.IntegrationFindings(h.ctx)
		if err != nil || string(all["gitlab"]) != findings || len(all) != 1 {
			h.t.Errorf("the findings pass reads %q (%v), want gitlab's alone", all, err)
		}
		if got := h.integrations(); len(got) != 1 || string(got["gitlab"]) != status {
			h.t.Errorf("the status pass reads %q, want gitlab's status alone and never its findings", got)
		}
		if _, found := h.integrationStatus("slack"); found {
			h.t.Error("a surface nothing recorded reads as found by key")
		}
		if _, found := h.integrationFindingsOf("slack"); found {
			h.t.Error("findings nothing recorded read as found by key")
		}
	}},

	{name: "a pass that found nothing drops the findings and a status write keeps them", fn: func(h *fleetHarness) {
		h.recordPass("gitlab", `{"attempts":1}`, []byte(`{"findings":[]}`))
		h.putIntegration("gitlab", `{"attempts":2}`)
		if got, found := h.integrationFindingsOf("gitlab"); !found || got != `{"findings":[]}` {
			h.t.Errorf("a status write left the findings as %q (found %t), want them kept", got, found)
		}
		h.recordPass("gitlab", `{"attempts":3}`, nil)
		if got, found := h.integrationFindingsOf("gitlab"); found {
			h.t.Errorf("a pass that found nothing left the findings %q", got)
		}
		if got, _ := h.integrationStatus("gitlab"); got != `{"attempts":3}` {
			h.t.Errorf("the status reads %q, want the pass's", got)
		}
	}},

	{name: "a deleted status takes its findings with it", fn: func(h *fleetHarness) {
		h.recordPass("gitlab", `{}`, []byte(`{"findings":[]}`))
		h.recordPass("slack", `{}`, []byte(`{"findings":[]}`))
		if err := h.f.DeleteIntegrationStatus(h.ctx, "gitlab"); err != nil {
			h.t.Fatalf("DeleteIntegrationStatus: %v", err)
		}
		if _, found := h.integrationFindingsOf("gitlab"); found {
			h.t.Error("a deleted surface's findings are still recorded")
		}
		all, err := h.f.IntegrationFindings(h.ctx)
		if err != nil || len(all) != 1 || all["slack"] == nil {
			h.t.Errorf("the findings pass after the delete reads %q (%v), want slack's alone", all, err)
		}
	}},

	{name: "a status the caller mutates afterwards does not change the store", fn: func(h *fleetHarness) {
		// A caller reusing its marshalling buffer is ordinary, and a
		// backend that kept the caller's slice would have the next
		// reader see whatever that buffer holds by then.
		value := []byte(`{"attempts":1}`)
		if err := h.f.PutIntegrationStatus(h.ctx, "gitlab", value); err != nil {
			h.t.Fatalf("PutIntegrationStatus: %v", err)
		}
		copy(value, []byte(`{"attempts":9}`))
		if got := string(h.integrations()["gitlab"]); got != `{"attempts":1}` {
			h.t.Fatalf("read back %q after the caller reused its buffer", got)
		}
	}},
}

// ---- the atlas sources' standings --------------------------------------- //

func (h *fleetHarness) putAtlasSource(kind, value string) {
	h.t.Helper()
	if err := h.f.PutAtlasSourceStatus(h.ctx, kind, []byte(value)); err != nil {
		h.t.Fatalf("PutAtlasSourceStatus(%q): %v", kind, err)
	}
}

func (h *fleetHarness) atlasSources() map[string][]byte {
	h.t.Helper()
	got, err := h.f.AtlasSourceStatuses(h.ctx)
	if err != nil {
		h.t.Fatalf("AtlasSourceStatuses: %v", err)
	}
	return got
}

func (h *fleetHarness) putAtlasMisses(kind, value string) {
	h.t.Helper()
	if err := h.f.PutAtlasSourceMisses(h.ctx, kind, []byte(value)); err != nil {
		h.t.Fatalf("PutAtlasSourceMisses(%q): %v", kind, err)
	}
}

func (h *fleetHarness) atlasMisses(kind string) (string, bool) {
	h.t.Helper()
	value, found, err := h.f.AtlasSourceMisses(h.ctx, kind)
	if err != nil {
		h.t.Fatalf("AtlasSourceMisses(%q): %v", kind, err)
	}
	return string(value), found
}

func (h *fleetHarness) atlasSourceNames() []string {
	h.t.Helper()
	names := slices.Collect(maps.Keys(h.atlasSources()))
	slices.Sort(names)
	return names
}

var atlasSourceCases = []fleetCase{
	{name: "a recorded standing reads back byte for byte", fn: func(h *fleetHarness) {
		// The value is OPAQUE to this store: internal/atlas/collect marshals
		// the last read, its digest and what it has stopped seeing into it.
		const standing = `{"kind":"cloudflare","last_complete_at":"2026-11-12T07:00:00Z","digest":"ab12"}`
		h.putAtlasSource("cloudflare", standing)
		if got := string(h.atlasSources()["cloudflare"]); got != standing {
			h.t.Fatalf("read back %q, want %q", got, standing)
		}
	}},

	{name: "a source with no standing is absent rather than empty", fn: func(h *fleetHarness) {
		// The duty reads a missing key as "never read, so due now, with
		// nothing to retire"; an empty value would parse as a standing with
		// no read in it, which is not the same fact.
		h.putAtlasSource("datadog", `{"kind":"datadog"}`)
		if _, present := h.atlasSources()["cloudflare"]; present {
			h.t.Fatal("a source nothing recorded came back present")
		}
	}},

	{name: "the last write wins", fn: func(h *fleetHarness) {
		// No compare-and-set: the collect duty is a fleet singleton that
		// re-claims its lease before every source it visits, so one writer
		// holds a key at a time. See [coord.AtlasSources].
		h.putAtlasSource("cloudflare", `{"calls":1}`)
		h.putAtlasSource("cloudflare", `{"calls":2}`)
		if got := string(h.atlasSources()["cloudflare"]); got != `{"calls":2}` {
			h.t.Fatalf("read back %q, want the second write", got)
		}
	}},

	{name: "every recorded source is listed", fn: func(h *fleetHarness) {
		h.putAtlasSource("cloudflare", `{}`)
		h.putAtlasSource("datadog", `{}`)
		want := []string{"cloudflare", "datadog"}
		if got := h.atlasSourceNames(); !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "a deleted standing is gone and the rest survive", fn: func(h *fleetHarness) {
		h.putAtlasSource("cloudflare", `{}`)
		h.putAtlasSource("datadog", `{}`)
		if err := h.f.DeleteAtlasSourceStatus(h.ctx, "cloudflare"); err != nil {
			h.t.Fatalf("DeleteAtlasSourceStatus: %v", err)
		}
		if got := h.atlasSourceNames(); !slices.Equal(got, []string{"datadog"}) {
			h.t.Fatalf("listed %v, want datadog alone", got)
		}
	}},

	{name: "deleting a standing that is not there is the outcome asked for", fn: func(h *fleetHarness) {
		// The duty forgets every source the company no longer connects, on
		// every tick, so this is the common path.
		if err := h.f.DeleteAtlasSourceStatus(h.ctx, "never-recorded"); err != nil {
			h.t.Fatalf("deleting an absent standing: %v", err)
		}
	}},

	{name: "a standing the caller mutates afterwards does not change the store", fn: func(h *fleetHarness) {
		value := []byte(`{"calls":1}`)
		if err := h.f.PutAtlasSourceStatus(h.ctx, "cloudflare", value); err != nil {
			h.t.Fatalf("PutAtlasSourceStatus: %v", err)
		}
		copy(value, []byte(`{"calls":9}`))
		if got := string(h.atlasSources()["cloudflare"]); got != `{"calls":1}` {
			h.t.Fatalf("read back %q after the caller reused its buffer", got)
		}
	}},

	{name: "a standing is kept apart from the integration statuses", fn: func(h *fleetHarness) {
		// The two are the same shape of record about the same vendors, and
		// a backend that kept them in one place would hand the reconcile
		// loop a row it forgets as a surface nothing declares.
		h.putAtlasSource("cloudflare", `{"calls":1}`)
		h.putIntegration("cloudflare", `{"phase":"ready"}`)
		if got := string(h.integrations()["cloudflare"]); got != `{"phase":"ready"}` {
			h.t.Fatalf("the integration status reads %q", got)
		}
		if err := h.f.DeleteIntegrationStatus(h.ctx, "cloudflare"); err != nil {
			h.t.Fatal(err)
		}
		if got := string(h.atlasSources()["cloudflare"]); got != `{"calls":1}` {
			h.t.Fatalf("forgetting the integration status took the standing with it: %q", got)
		}
	}},

	{name: "a source's misses are read by its key and never listed beside the standings", fn: func(h *fleetHarness) {
		// The tick and every report list the standings, and a listing that
		// carried each source's misses would wait the crossing of them all.
		// Mutation: list the misses beside the standings, and datadog is
		// listed.
		h.putAtlasSource("cloudflare", `{"calls":1}`)
		h.putAtlasMisses("cloudflare", `{"missing":[{"entries":["a"]}]}`)
		h.putAtlasMisses("datadog", `{"missing":[]}`)
		if got := h.atlasSourceNames(); !slices.Equal(got, []string{"cloudflare"}) {
			h.t.Fatalf("the standings listed %v, want cloudflare's alone", got)
		}
		if got := string(h.atlasSources()["cloudflare"]); got != `{"calls":1}` {
			h.t.Fatalf("the standing reads %q beside its misses", got)
		}
		if got, found := h.atlasMisses("cloudflare"); !found || got != `{"missing":[{"entries":["a"]}]}` {
			h.t.Fatalf("the misses read back %q (%v)", got, found)
		}
		if _, found := h.atlasMisses("scaleway"); found {
			h.t.Fatal("a source nothing recorded the misses of came back with some")
		}
	}},

	{name: "forgetting a source drops its misses with its standing", fn: func(h *fleetHarness) {
		// A source the company connects again starts its misses afresh
		// with its standing, never from an earlier connection's.
		// Mutation: purge the standing alone, and its misses survive.
		h.putAtlasSource("cloudflare", `{}`)
		h.putAtlasMisses("cloudflare", `{"missing":[]}`)
		h.putAtlasMisses("datadog", `{"missing":[]}`)
		if err := h.f.DeleteAtlasSourceStatus(h.ctx, "cloudflare"); err != nil {
			h.t.Fatalf("DeleteAtlasSourceStatus: %v", err)
		}
		if _, found := h.atlasMisses("cloudflare"); found {
			h.t.Fatal("the forgotten source's misses survived it")
		}
		if _, found := h.atlasMisses("datadog"); !found {
			h.t.Fatal("forgetting one source took another's misses")
		}
	}},
}

// ---- the atlas sources' refresh asks ------------------------------------ //

func (h *fleetHarness) putAtlasRefresh(kind, value string) {
	h.t.Helper()
	if err := h.f.PutAtlasSourceRefresh(h.ctx, kind, []byte(value)); err != nil {
		h.t.Fatalf("PutAtlasSourceRefresh(%q): %v", kind, err)
	}
}

func (h *fleetHarness) atlasRefreshes() map[string][]byte {
	h.t.Helper()
	got, err := h.f.AtlasSourceRefreshes(h.ctx)
	if err != nil {
		h.t.Fatalf("AtlasSourceRefreshes: %v", err)
	}
	return got
}

func (h *fleetHarness) atlasRefreshNames() []string {
	h.t.Helper()
	names := slices.Collect(maps.Keys(h.atlasRefreshes()))
	slices.Sort(names)
	return names
}

var atlasSourceRefreshCases = []fleetCase{
	{name: "a recorded ask reads back byte for byte", fn: func(h *fleetHarness) {
		const ask = `{"requested_at":"2026-01-02T03:04:05Z","by":"ada"}`
		h.putAtlasRefresh("cloudflare", ask)
		if got := string(h.atlasRefreshes()["cloudflare"]); got != ask {
			h.t.Fatalf("read back %q, want %q", got, ask)
		}
	}},

	{name: "a source nobody asked for is absent rather than empty", fn: func(h *fleetHarness) {
		h.putAtlasRefresh("datadog", `{}`)
		if _, present := h.atlasRefreshes()["cloudflare"]; present {
			h.t.Fatal("a source nobody asked for came back present")
		}
	}},

	{name: "the last ask wins", fn: func(h *fleetHarness) {
		h.putAtlasRefresh("cloudflare", `{"by":"ada"}`)
		h.putAtlasRefresh("cloudflare", `{"by":"grace"}`)
		if got := string(h.atlasRefreshes()["cloudflare"]); got != `{"by":"grace"}` {
			h.t.Fatalf("read back %q, want the second ask", got)
		}
	}},

	{name: "a dropped ask is gone, the rest survive, and dropping it again is fine", fn: func(h *fleetHarness) {
		h.putAtlasRefresh("cloudflare", `{}`)
		h.putAtlasRefresh("datadog", `{}`)
		for range 2 {
			if err := h.f.DeleteAtlasSourceRefresh(h.ctx, "cloudflare"); err != nil {
				h.t.Fatalf("DeleteAtlasSourceRefresh: %v", err)
			}
		}
		if got := h.atlasRefreshNames(); !slices.Equal(got, []string{"datadog"}) {
			h.t.Fatalf("listed %v, want datadog alone", got)
		}
	}},

	{name: "an ask the caller mutates afterwards does not change the store", fn: func(h *fleetHarness) {
		value := []byte(`{"by":"ada"}`)
		if err := h.f.PutAtlasSourceRefresh(h.ctx, "cloudflare", value); err != nil {
			h.t.Fatalf("PutAtlasSourceRefresh: %v", err)
		}
		copy(value, []byte(`{"by":"eve"}`))
		if got := string(h.atlasRefreshes()["cloudflare"]); got != `{"by":"ada"}` {
			h.t.Fatalf("read back %q after the caller reused its buffer", got)
		}
	}},

	{name: "an ask is kept apart from its source's standing", fn: func(h *fleetHarness) {
		// The duty replaces a standing whole on every visit: an ask stored
		// beside it would be lost to a visit in flight.
		h.putAtlasRefresh("cloudflare", `{"by":"ada"}`)
		h.putAtlasSource("cloudflare", `{"calls":1}`)
		if err := h.f.DeleteAtlasSourceStatus(h.ctx, "cloudflare"); err != nil {
			h.t.Fatal(err)
		}
		if got := string(h.atlasRefreshes()["cloudflare"]); got != `{"by":"ada"}` {
			h.t.Fatalf("forgetting the source's standing took its ask with it: %q", got)
		}
	}},
}
