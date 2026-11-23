package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHERE AN OPENCODE BOX COMPACTS A CONTINUED SESSION'S FIRST REQUEST, decided
// from what the read brought back ([openCodePointOf]): each case hands it
// model.list's and config.get's answers as a box of the release would.

// pointModel is one model.list record of provider/model: its context and
// input limits (0 leaves one unset) and its release time.
type pointModel struct {
	provider, model string
	context, input  int64
	released        int64
	extra           string
}

// modelsAnswer is model.list's answer listing models.
func modelsAnswer(models ...pointModel) []byte {
	var records []string
	for _, m := range models {
		limit := fmt.Sprintf(`"context":%d,"output":32000`, m.context)
		if m.input > 0 {
			limit += fmt.Sprintf(`,"input":%d`, m.input)
		}
		records = append(records, fmt.Sprintf(`{"id":%q,"modelID":%q,"providerID":%q,"limit":{%s},`+
			`"time":{"released":%d},"cost":[]%s}`, m.model, m.model, m.provider, limit, m.released, m.extra))
	}
	return []byte(`{"location":"/box/models.json","data":[` + strings.Join(records, ",") + `]}`)
}

// configAnswer is config.get's answer holding documents, lowest first, each a
// document's info as JSON, and the box's configuration directory.
func configAnswer(documents ...string) []byte {
	entries := []string{`{"type":"directory","path":"/box/.config/opencode"}`}
	for i, info := range documents {
		entries = append(entries, fmt.Sprintf(`{"type":"document","path":"/box/doc-%d.json","info":%s}`, i, info))
	}
	return []byte("[" + strings.Join(entries, ",") + "]")
}

// listingOf is the script's lines of a read whose steps all ran, run in /w,
// with more after them.
func listingOf(more ...string) string {
	lines := append([]string{"status models 0", "status config 0", "cwd /w", "dir /box/.config/opencode",
		"dir /w/.opencode", "dir /.opencode"}, more...)
	return strings.Join(lines, "\n") + "\n"
}

// stated is a document stating stub/m's limit at context, a model the
// catalog does not list.
func stated(context int64) string {
	return fmt.Sprintf(`{"providers":{"stub":{"models":{"m":{"limit":{"context":%d}}}}}}`, context)
}

// readPoint is the point a box of release decides for the launch's stub/m,
// the session written by release, from models, the documents and the lines.
func readPoint(release string, models []byte, config []byte, listing string,
	sources *sandbox.RunSources) sandbox.BoxPoint {
	ask := sandbox.PointAsk{Model: "stub/m", Release: release, Writer: release, Sources: sources}
	return openCodePointOf(ask, openCodeAnswers{release: release, models: models, config: config,
		listing: listing}, Paths{Home: "/box"})
}

// A DEFAULTED LIMIT IS NO POINT. OpenCode holds a model nothing states the
// limit of to its default record's 200,000 tokens, which may not be its
// server's window. Stated, IN THIS ORDER: a document states its context or
// input limit; else the catalog listed it, which its record's release time
// says, unless a document set that time (`releaseDate`), which leaves the
// limit the default's. A document entry that sets only options on a catalog
// model leaves the catalog's limit stated.
//
// Mutation: read a release time above zero as stated whatever a document set,
// and the releaseDate case goes red.
func TestADefaultedLimitIsNoPoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		released int64
		document string
		want     sandbox.PointMark
	}{
		{name: "nothing states it", want: sandbox.PointDefaulted},
		{name: "a document sets its release date", released: 1793923200000,
			document: `{"providers":{"stub":{"models":{"m":{"releaseDate":"2026-11-01"}}}}}`,
			want:     sandbox.PointDefaulted},
		{name: "the catalog lists it", released: 1793923200000},
		{name: "a document sets options on a catalog model", released: 1793923200000,
			document: `{"providers":{"stub":{"models":{"m":{"options":{"temperature":0}}}}}}`},
		{name: "a document states it", document: stated(200000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var docs []string
			if tc.document != "" {
				docs = append(docs, tc.document)
			}
			got := readPoint("2.3.2", modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000,
				released: tc.released}), configAnswer(docs...), listingOf(), nil)
			if got.Mark != tc.want || (tc.want == "" && got.Tokens != 170000) {
				t.Errorf("the point is %+v, want mark %q (and 170,000 where none)", got, tc.want)
			}
		})
	}
}

// A ZERO LIMIT IS NO POINT WHERE COMPACTION IS ON. A catalog model with no
// limit is built with a limit of zero, and OpenCode's point function answers
// infinity for it, so only the provider's refusal compacts it, at a size
// nothing here knows; with compaction off it is sent whole as any model is.
//
// Mutation: read a zero limit as a point never reached, and this goes red.
func TestAZeroLimitIsNoPointWhereCompactionIsOn(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", released: 1793923200000})
	if got := readPoint("2.3.2", models, configAnswer(), listingOf(), nil); got.Mark != sandbox.PointNoLimit ||
		got.Never {
		t.Errorf("a zero limit with compaction on reads %+v, want %q", got, sandbox.PointNoLimit)
	}
	off := configAnswer(`{"compaction":{"auto":false}}`)
	if got := readPoint("2.3.2", models, off, listingOf(), nil); got.Mark != "" || !got.Never {
		t.Errorf("a zero limit with compaction off reads %+v, want never compacted", got)
	}
}

// COMPACTION OFF SENDS WHOLE WHATEVER THE COUNT. `compaction.auto: false`
// skips every reason but a manual one, the provider's refusal included, so
// the session is sent whole however long it is; the highest document that
// sets the key decides it.
//
// Mutation: read compaction off as a zero limit, or take the lowest
// document's word, and a case goes red.
func TestCompactionOffSendsWholeWhateverTheCount(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	off := readPoint("2.3.2", models, configAnswer(stated(200000), `{"compaction":{"auto":false}}`),
		listingOf(), nil)
	if off.Mark != "" || !off.Never || !off.SendsWhole(&sandbox.ResumedCount{Tokens: 900000}, "stub", "stub",
		"brief") {
		t.Errorf("compaction off reads %+v, want a session sent whole whatever its count", off)
	}
	on := readPoint("2.3.2", models, configAnswer(`{"compaction":{"auto":false}}`, stated(200000),
		`{"compaction":{"auto":true}}`), listingOf(), nil)
	if on.Never || on.Tokens != 170000 {
		t.Errorf("compaction turned on by the highest document reads %+v, want 170,000", on)
	}
}

// THE HIGHEST DOCUMENT'S BUFFER MOVES THE POINT. A buffer below the limit is
// held back from it; the highest document that sets one decides it; a
// `"N%"` takes that share of the limit, rounded half up; a buffer at or past
// the limit leaves the share.
//
// Mutation: take the lowest document's buffer, or apply a buffer past the
// limit, and a case goes red.
func TestTheHighestDocumentsBufferMovesThePoint(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	for _, tc := range []struct {
		name, release string
		documents     []string
		want          int64
		mark          sandbox.PointMark
	}{
		{name: "the highest of two", release: "2.3.2", want: 170000,
			documents: []string{`{"compaction":{"buffer":40000}}`, `{"compaction":{"buffer":30000}}`}},
		{name: "a share on 2.3.2", release: "2.3.2", want: 175000,
			documents: []string{`{"compaction":{"buffer":"12.5%"}}`}},
		{name: "a share rounded half up", release: "2.3.2", want: 200000 - 25001,
			documents: []string{`{"compaction":{"buffer":"12.5005%"}}`}},
		{name: "a share that is no number", release: "2.3.2", mark: sandbox.PointUnread,
			documents: []string{`{"compaction":{"buffer":"ten%"}}`}},
		{name: "past the limit on 2.3.2", release: "2.3.2", want: 170000,
			documents: []string{`{"compaction":{"buffer":250000}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := readPoint(tc.release, models, configAnswer(append([]string{stated(200000)},
				tc.documents...)...), listingOf(), nil)
			if got.Mark != tc.mark || (tc.mark == "" && got.Tokens != tc.want) {
				t.Errorf("the point is %+v, want %d (mark %q)", got, tc.want, tc.mark)
			}
		})
	}
}

// THE SHARE. With no buffer, OpenCode holds back the larger of fifteen
// hundredths of the limit and 16,000 tokens once the limit is twice that
// (measured at its boundaries on 2.3.2); a limit under 32,000 holds back the
// share alone.
//
// Mutation: take a tenth, or hold back 16,000 under 32,000, and a case goes
// red.
func TestTheShareHoldsBackFifteenHundredths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		release string
		limit   int64
		want    int64
	}{
		{"2.3.2", 200000, 170000}, {"2.3.2", 300000, 255000}, {"2.3.2", 100000, 84000},
		{"2.3.2", 20000, 17000},
	} {
		models := modelsAnswer(pointModel{provider: "stub", model: "m", context: tc.limit})
		got := readPoint(tc.release, models, configAnswer(stated(tc.limit)), listingOf(), nil)
		if got.Mark != "" || got.Tokens != tc.want {
			t.Errorf("OpenCode %s at a limit of %d reads %+v, want %d", tc.release, tc.limit, got, tc.want)
		}
	}
}

// THE LIMIT IS THE SMALLER OF INPUT AND CONTEXT. OpenCode takes the smaller of
// a model's input and context limits where both are set.
//
// Mutation: take the input limit where both are set, and this goes red.
func TestTheLimitIsTheSmallerOfInputAndContext(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000, input: 300000})
	got := readPoint("2.3.2", models, configAnswer(stated(200000)), listingOf(), nil)
	if got.Mark != "" || got.Tokens != 170000 {
		t.Errorf("OpenCode 2.3.2 with input 300,000 and context 200,000 reads %+v, want 170,000", got)
	}
}

// A NATIVE COMPACTION OR CONTEXT MANAGEMENT IS NO POINT. A model whose
// compaction is its provider's own compacts by a rule nothing here reads, and
// a provider's contextManagement option edits the conversation server-side:
// set in a document on the launch's provider, or carried in the model's
// settings. Another provider's option is not the launch's.
//
// Mutation: ignore either, and a case goes red.
func TestANativeCompactionOrContextManagementIsNoPoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		model    pointModel
		document string
		want     sandbox.PointMark
	}{
		{name: "native compaction", model: pointModel{extra: `,"compaction":{"type":"native"}`},
			want: sandbox.PointNative},
		{name: "a document's option", want: sandbox.PointContextManagement,
			document: `{"providers":{"stub":{"settings":{"contextManagement":{"edits":[]}}}}}`},
		{name: "the model's settings", want: sandbox.PointContextManagement,
			model: pointModel{extra: `,"settings":{"contextManagement":{}}`}},
		{name: "another provider's option",
			document: `{"providers":{"other":{"settings":{"contextManagement":{}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := tc.model
			m.provider, m.model, m.context = "stub", "m", 200000
			docs := []string{stated(200000)}
			if tc.document != "" {
				docs = append(docs, tc.document)
			}
			if got := readPoint("2.3.2", modelsAnswer(m), configAnswer(docs...), listingOf(), nil); got.Mark != tc.want {
				t.Errorf("the point is %+v, want mark %q", got, tc.want)
			}
		})
	}
}

// A PLUGIN OTHER THAN THE ENGINE'S IS NO POINT. A plugin runs in the server and
// can compact the session or add to it before the first request, so a
// document's plugin, a plugin in a folder the read listed, and a
// configuration directory the read did not list for plugins are each no
// point; a document's removal of a plugin adds nothing.
//
// Mutation: leave the folders unread, or the unlisted directory, and a case
// goes red.
func TestAPluginOtherThanTheEnginesIsNoPoint(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	for _, tc := range []struct {
		name     string
		document string
		listing  string
		config   []byte
		want     sandbox.PointMark
	}{
		{name: "a document's plugin", document: `{"plugins":[{"package":"@acme/compactor","options":{"k":1}}]}`,
			want: sandbox.PointPlugin},
		{name: "a folder's plugin", listing: listingOf("plugin /w/.opencode/plugins/compactor"),
			want: sandbox.PointPlugin},
		{name: "a directory the read did not list", want: sandbox.PointPlugin,
			config: []byte(`[{"type":"directory","path":"/elsewhere/opencode"},` +
				`{"type":"document","path":"/d","info":` + stated(200000) + `}]`)},
		{name: "a removal", document: `{"plugins":["-@acme/compactor"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := tc.config
			if config == nil {
				docs := []string{stated(200000)}
				if tc.document != "" {
					docs = append(docs, tc.document)
				}
				config = configAnswer(docs...)
			}
			listing := tc.listing
			if listing == "" {
				listing = listingOf()
			}
			if got := readPoint("2.3.2", models, config, listing, nil); got.Mark != tc.want {
				t.Errorf("the point is %+v, want mark %q", got, tc.want)
			}
		})
	}
}

// A BOX OF ANOTHER RELEASE THAN THE WRITER'S IS NO POINT. No pair of differing
// releases has been measured, so a box of one release judges no session
// another wrote, and a writer nobody recorded is no pair either; a box
// release outside the set is no point whoever wrote the session.
//
// Mutation: drop the pair's check, and a case goes red.
func TestABoxOfAnotherReleaseThanTheWritersIsNoPoint(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	for _, tc := range []struct {
		box, writer string
		want        sandbox.PointMark
	}{
		{"2.3.2", "2.3.2", ""}, {"2.3.2", "2.4.0", sandbox.PointPair}, {"2.3.2", "", sandbox.PointPair},
		{"2.4.0", "2.4.0", sandbox.PointRelease},
	} {
		ask := sandbox.PointAsk{Model: "stub/m", Release: tc.box, Writer: tc.writer}
		got := openCodePointOf(ask, openCodeAnswers{release: tc.box, models: models,
			config: configAnswer(stated(200000)), listing: listingOf()}, Paths{Home: "/box"})
		if got.Mark != tc.want {
			t.Errorf("a box of %q judging a session %q wrote reads %+v, want mark %q", tc.box, tc.writer, got,
				tc.want)
		}
	}
}

// THE BOX'S SOURCES ARE SIZED ONLY IN ITS OWN BOX. Where the box holds the
// session's copy, OpenCode may hand the session every context source that
// changed since its last request there, so the read sizes them: the run's own
// instructions and every instruction file it found, a removal sentence for
// each directory it walked, the run's skills and its servers' guidance, the
// date and the environment. A source the read cannot size (a document's MCP
// servers, a server the box dials itself) is no point there. After an import
// into another box nothing is handed over, so nothing is sized or marked.
//
// Mutation: size the sources after an import, or leave a file found, the
// skills or the servers' guidance out of the charge, and a case goes red.
func TestTheBoxsSourcesAreSizedOnlyInItsOwnBox(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	config := configAnswer(stated(200000))
	sources := &sandbox.RunSources{System: "the run's own instructions",
		Skills:   []sandbox.Skill{{Name: "deploy", Description: "how to deploy"}},
		Guidance: map[string]string{"tracker": "the tracker's\ninstructions"}}
	bare := readPoint("2.3.2", models, config, listingOf("walk /w", "walk /"), sources)
	if bare.Mark != "" || bare.BoxBytes <= int64(len("the run's own instructions")) {
		t.Fatalf("in its own box the point reads %+v, want the run's sources sized", bare)
	}
	found := readPoint("2.3.2", models, config, listingOf("walk /w", "walk /", "instr  5000 /w/AGENTS.md"),
		sources)
	if found.BoxBytes < bare.BoxBytes+5000 {
		t.Errorf("an AGENTS.md of 5,000 bytes moves the charge from %d to %d, want at least its bytes more",
			bare.BoxBytes, found.BoxBytes)
	}
	for name, without := range map[string]sandbox.RunSources{
		"its servers' guidance": {System: sources.System, Skills: sources.Skills},
		"its skills":            {System: sources.System, Guidance: sources.Guidance},
	} {
		less := readPoint("2.3.2", models, config, listingOf("walk /w", "walk /"), &without)
		if bare.BoxBytes <= less.BoxBytes {
			t.Errorf("the run's sources charge %d bytes with %s and %d without, want more with them", bare.BoxBytes,
				name, less.BoxBytes)
		}
	}
	if imported := readPoint("2.3.2", models, config, listingOf("walk /w"), nil); imported.BoxBytes != 0 {
		t.Errorf("after an import the point charges %d bytes, want none", imported.BoxBytes)
	}
	mcp := configAnswer(stated(200000), `{"mcp":{"servers":{"x":{"type":"local"}}}}`)
	if got := readPoint("2.3.2", models, mcp, listingOf(), sources); got.Mark != sandbox.PointBoxSources {
		t.Errorf("a document's MCP servers in the box's own copy read %+v, want %q", got, sandbox.PointBoxSources)
	}
	if got := readPoint("2.3.2", models, mcp, listingOf(), nil); got.Mark != "" {
		t.Errorf("a document's MCP servers after an import read %+v, want a point", got)
	}
	dialed := *sources
	dialed.Unsized = []string{"github"}
	if got := readPoint("2.3.2", models, config, listingOf(), &dialed); got.Mark != sandbox.PointBoxSources {
		t.Errorf("a server the box dials itself reads %+v, want %q", got, sandbox.PointBoxSources)
	}
}

// EACH SOURCE IS CHARGED AT LEAST ITS RENDER. In the box that holds the copy,
// OpenCode 2.3.2 hands a changed skill list an entry per skill whose id and
// name are both its listed name ([sandbox.Skill.Listed]), a server's
// instructions an entry, and a changed day and environment their lines, the
// working directory twice and the box's temporary folder once, the text
// inside each entry XML-escaped ([openCodeRendered]); the charge of each is
// at least that render.
//
// Mutation: charge a skill entry by its name or its description alone, charge
// a description or a server's instructions unescaped, or drop the date's and
// the environment's charge, and a case goes red.
func TestEachSourceIsChargedAtLeastItsRender(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	config := configAnswer(stated(200000))
	paths := Paths{Home: "/box"}
	charge := func(sources sandbox.RunSources) int64 {
		t.Helper()
		got := readPoint("2.3.2", models, config, listingOf("walk /w", "walk /"), &sources)
		if got.Mark != "" {
			t.Fatalf("in its own box the point reads %+v, want one", got)
		}
		return got.BoxBytes
	}
	first := sandbox.Skill{Name: "deploy", Description: "how to deploy"}
	second := sandbox.Skill{Name: "release-the-nightly-build", Description: "cut " + strings.Repeat("<it> & ", 8)}
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	entry := fmt.Sprintf(openCodeRendered.skill, escape(second.Listed()), escape(second.Listed()),
		escape(second.Description))
	if got := charge(sandbox.RunSources{Skills: []sandbox.Skill{first, second}}) -
		charge(sandbox.RunSources{Skills: []sandbox.Skill{first}}); got < int64(len(entry)) {
		t.Errorf("a skill whose entry renders %d bytes is charged %d", len(entry), got)
	}
	said := "Use acme for " + strings.Repeat("<deploys> & ", 8) + "\nSecond line."
	server := fmt.Sprintf(openCodeRendered.server, "acme", "acme",
		strings.ReplaceAll(escape(said), "\n", "\n    "))
	if got := charge(sandbox.RunSources{Guidance: map[string]string{"base": "b", "acme": said}}) -
		charge(sandbox.RunSources{Guidance: map[string]string{"base": "b"}}); got < int64(len(server)) {
		t.Errorf("a server whose entry renders %d bytes is charged %d", len(server), got)
	}
	system := "The instructions changed:\nInstructions from: " + paths.System() + "\n" +
		finalSystem("", paths, nil) + "\n"
	day := openCodeRendered.date
	environment := fmt.Sprintf(openCodeRendered.environment, "/w", "/w", paths.Home+"/.tmp/opencode")
	if got, want := charge(sandbox.RunSources{}), len(system)+len(day)+len(environment); got < int64(want) {
		t.Errorf("the run's instructions, the day and the environment render %d bytes and are charged %d",
			want, got)
	}
}

// A CODE MODE CATALOG IS CHARGED AT LEAST ITS RENDER. A server new to a session
// in the box that holds its copy is listed in OpenCode's update with a
// signature per tool, escaped; the charge of each tool is at least what
// 2.3.2 rendered of it, measured ([openCodeCatalogRendered]): a flat schema,
// one with an enum, an array, bounds and escaped descriptions, a union, and
// one nested eight levels.
//
// Mutation: drop the catalog's charge, or charge a tool by its name and
// description alone, and a case goes red.
func TestACodeModeCatalogIsChargedAtLeastItsRender(t *testing.T) {
	t.Parallel()
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000})
	config := configAnswer(stated(200000))
	charge := func(catalogs map[string][]sandbox.CatalogTool) int64 {
		t.Helper()
		got := readPoint("2.3.2", models, config, listingOf("walk /w", "walk /"),
			&sandbox.RunSources{Catalogs: catalogs})
		if got.Mark != "" {
			t.Fatalf("in its own box the point reads %+v, want one", got)
		}
		return got.BoxBytes
	}
	base := []sandbox.CatalogTool{{Name: "ping", Description: "Ping.", Schema: map[string]any{"type": "object"}}}
	for name, tool := range openCodeCatalogTools() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			render := openCodeCatalogRendered[name]
			got := charge(map[string][]sandbox.CatalogTool{"base": base, "acme": {tool}}) -
				charge(map[string][]sandbox.CatalogTool{"base": base})
			if want := int64(len("- acme (1 tool)\n") + len(render)); got < want {
				t.Errorf("a tool whose catalog entry renders %d bytes is charged %d", want, got)
			}
		})
	}
}

// openCodeCatalogTools are the probe's tools, as its MCP server declared them.
func openCodeCatalogTools() map[string]sandbox.CatalogTool {
	var deep func(n int) map[string]any
	deep = func(n int) map[string]any {
		if n == 0 {
			return map[string]any{"type": "string", "description": "leaf <0> & end"}
		}
		return map[string]any{"type": "object", "description": fmt.Sprintf("level %d", n), "required": []any{"next"},
			"properties": map[string]any{"next": deep(n - 1), "flag": map[string]any{"type": "boolean"}}}
	}
	return map[string]sandbox.CatalogTool{
		"echo": {Name: "echo", Description: "Echo a tag back (C10 probe).", Schema: map[string]any{
			"type": "object", "required": []any{"tag"},
			"properties": map[string]any{"tag": map[string]any{"type": "string"}}}},
		"search_issues": {Name: "search_issues", Description: "Search issues where <label> & state match.",
			Schema: map[string]any{"type": "object", "required": []any{"query"}, "properties": map[string]any{
				"query":  map[string]any{"type": "string", "description": "The query, e.g. a <b> & c"},
				"state":  map[string]any{"type": "string", "enum": []any{"open", "closed", "all"}},
				"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels"},
				"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"sort": map[string]any{"type": "object", "properties": map[string]any{
					"field": map[string]any{"type": "string"}, "desc": map[string]any{"type": "boolean"}}}}}},
		"union_tool": {Name: "union_tool", Description: "One of two shapes.", Schema: map[string]any{
			"type": "object", "properties": map[string]any{
				"v": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"},
					map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}},
				"n": map[string]any{"type": []any{"string", "null"}}}}},
		"deep_tool": {Name: "deep_tool", Description: "Nested eight levels.", Schema: deep(8)},
	}
}

// openCodeCatalogRendered is what OpenCode 2.3.2 rendered of each probe tool
// in a continued session's update, byte for byte.
var openCodeCatalogRendered = map[string]string{
	"echo": "  - tools.acme.echo({\n" +
		"  tag: string,\n" +
		"}): Promise&lt;unknown&gt; // Echo a tag back (C10 probe).\n",
	"search_issues": "  - tools.acme.search_issues({\n" +
		"  /** The query, e.g. a &lt;b&gt; &amp; c */\n" +
		"  query: string,\n" +
		"  state?: \"open\" | \"closed\" | \"all\",\n" +
		"  /** Labels */\n" +
		"  labels?: Array&lt;string&gt;,\n" +
		"  /** @integer @minimum 1 @maximum 100 */\n" +
		"  limit?: number,\n" +
		"  sort?: {\n" +
		"    field?: string,\n" +
		"    desc?: boolean,\n" +
		"  },\n" +
		"}): Promise&lt;unknown&gt; // Search issues where &lt;label&gt; &amp; state match.\n",
	"union_tool": "  - tools.acme.union_tool({\n" +
		"  v?: string | number | {\n" +
		"      a?: string,\n" +
		"    },\n" +
		"  n?: string | null,\n" +
		"}): Promise&lt;unknown&gt; // One of two shapes.\n",
	"deep_tool": "  - tools.acme.deep_tool({\n" +
		"  /** level 7 */\n" +
		"  next: {\n" +
		"    /** level 6 */\n" +
		"    next: {\n" +
		"      /** level 5 */\n" +
		"      next: {\n" +
		"        /** level 4 */\n" +
		"        next: {\n" +
		"          /** level 3 */\n" +
		"          next: {\n" +
		"            /** level 2 */\n" +
		"            next: {\n" +
		"              /** level 1 */\n" +
		"              next: {\n" +
		"                /** leaf &lt;0&gt; &amp; end */\n" +
		"                next: string,\n" +
		"                flag?: boolean,\n" +
		"              },\n" +
		"              flag?: boolean,\n" +
		"            },\n" +
		"            flag?: boolean,\n" +
		"          },\n" +
		"          flag?: boolean,\n" +
		"        },\n" +
		"        flag?: boolean,\n" +
		"      },\n" +
		"      flag?: boolean,\n" +
		"    },\n" +
		"    flag?: boolean,\n" +
		"  },\n" +
		"  flag?: boolean,\n" +
		"}): Promise&lt;unknown&gt; // Nested eight levels.\n",
}

// openCodeRendered is how OpenCode 2.3.2 renders a skill entry, a server's
// entry, the day and the environment in a continued session's update, as a
// probe's request carried them, the day at its longest. The catalog of a
// server's Code Mode tools, which 2.3.2 renders beside its entry, is not here.
var openCodeRendered = struct{ skill, server, date, environment string }{
	skill: "  &lt;skill&gt;\n    &lt;id&gt;%s&lt;/id&gt;\n    &lt;name&gt;%s&lt;/name&gt;\n" +
		"    &lt;description&gt;%s&lt;/description&gt;\n  &lt;/skill&gt;\n",
	server: "  &lt;server name=\"%s\"&gt;\n    Use tools from this server through `execute` under " +
		"`tools[\"%s\"]`.\n    %s\n  &lt;/server&gt;\n",
	date: "Today's date is now: Wed Sep 30 2026\n",
	environment: "The environment you are running in is now:\n&lt;env&gt;\n  Working directory: %s\n" +
		"  Workspace root folder: %s\n  Is directory a git repo: yes\n  Platform: linux\n  Prefer %s over " +
		"generic system temporary directories such as /tmp; it is pre-created and approved for external " +
		"access.\n&lt;/env&gt;\n",
}

// THE READ KEEPS NO VALUE IT WAS HANDED. config.get answers a document's
// `{env:…}` and `{file:…}` references resolved, so a plugin's options and a
// provider's settings may hold a credential: the point keeps the plugins'
// packages alone, and no field of it holds a value it was answered.
//
// Mutation: keep a plugin's options, or the raw answer in Why, and this goes
// red.
func TestTheReadKeepsNoValueItWasHanded(t *testing.T) {
	t.Parallel()
	const secret = "sk-test-0000-resolved-from-a-file-reference"
	models := modelsAnswer(pointModel{provider: "stub", model: "m", context: 200000,
		extra: `,"settings":{"apiKey":"` + secret + `"}`})
	config := configAnswer(stated(200000),
		`{"providers":{"stub":{"settings":{"apiKey":"`+secret+`"}}},`+
			`"plugins":[{"package":"@acme/compactor","options":{"token":"`+secret+`"}}]}`)
	got := readPoint("2.3.2", models, config, listingOf(), nil)
	if got.Mark != sandbox.PointPlugin || len(got.Plugins) != 1 || got.Plugins[0] != "@acme/compactor" {
		t.Fatalf("the point is %+v, want the plugin named by its package", got)
	}
	if strings.Contains(fmt.Sprintf("%#v", got), secret) {
		t.Errorf("the point keeps a value it was answered: %#v", got)
	}
}
