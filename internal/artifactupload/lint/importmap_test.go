package lint_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// AN IMPORT MAP ENTRY ON ANOTHER ORIGIN IS COPIED IN OR REFUSED, exactly as
// a module script's source is: a nonced module's imports load under its
// nonce from wherever the map points, so a remote entry left as written is a
// module every viewer's browser fetches from that origin at view time.
func TestAnImportMapEntryOnAnotherOriginIsCopiedIn(t *testing.T) {
	t.Parallel()
	r := &resolver{}
	src := "<script type=\"importmap\">\n{\n  \"imports\": {\n" +
		"    \"three\": \"https://cdn.jsdelivr.net/npm/three@0.169.0/build/three.module.js\",\n" +
		"    \"app\": \"./app.js\"\n  },\n" +
		"  \"scopes\": { \"./admin/\": { \"chart\": \"//unpkg.com/frappe-charts@1.6.2/dist/frappe-charts.esm.js\" } },\n" +
		"  \"integrity\": { \"https://cdn.jsdelivr.net/npm/three@0.169.0/build/three.module.js\": \"sha384-abc\" }\n" +
		"}\n</script><script type=module>import * as THREE from \"three\";</script>"
	page, err := lint.HTML("pages/index.html", []byte(src), opts(r))
	if err != nil {
		t.Fatal(err)
	}
	text := string(page.Text)
	for _, want := range []string{
		`"three": "../_vendor/cdn.jsdelivr.net/npm/three@0.169.0/build/three.module.js"`,
		`"app": "./app.js"`,
		`"chart": "../_vendor/unpkg.com/frappe-charts@1.6.2/dist/frappe-charts.esm.js"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the stored page lacks %s:\n%s", want, text)
		}
	}
	if len(r.asked) != 2 || r.asked[0].Kind != lint.RefModule || r.asked[1].Kind != lint.RefModule {
		t.Fatalf("the resolver was asked %+v, want two module copies", r.asked)
	}
	if r.asked[0].Integrity != "sha384-abc" || r.asked[0].Line != 4 {
		t.Errorf("the first copy was asked with integrity %q at line %d, want the map's own "+
			"integrity at line 4", r.asked[0].Integrity, r.asked[0].Line)
	}
	if got := lint.LocalRefs("pages/index.html", page.Text, true); !slices.Contains(got,
		"_vendor/cdn.jsdelivr.net/npm/three@0.169.0/build/three.module.js") {
		t.Errorf("LocalRefs = %v, which leaves out the copy the map names", got)
	}
	if _, err := lint.Stored("pages/index.html", page.Text, opts(nil)); err != nil {
		t.Errorf("the stored page did not read back as the linter's own: %v", err)
	}
}

// A COPY IN THE TOP DIRECTORY IS WRITTEN AS A RELATIVE ADDRESS: a browser maps
// nothing to a bare one.
func TestAnImportMapCopyIsAnAddressABrowserMaps(t *testing.T) {
	t.Parallel()
	src := `<script type=importmap>{"imports":{"lib":"https://unpkg.com/lib@1.0.0/x.js"}}</script>`
	page, err := lint.HTML("index.html", []byte(src), opts(&resolver{}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"lib":"./_vendor/unpkg.com/lib@1.0.0/x.js"}`; !strings.Contains(string(page.Text), want) {
		t.Fatalf("the stored page reads %s, want the address %s", page.Text, want)
	}
}

// AN ENTRY THAT CANNOT BE COPIED IN IS REFUSED, each with the code a reader
// acts on.
func TestAnImportMapEntryThatCannotBeCopiedInIsRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		src  string
		want lint.Code
	}{
		"an unlisted host": {`<script type="importmap">{"imports":{"charts":"https://esm.sh/frappe-charts@1.6.2"}}</script>`,
			lint.CodeRemoteImport},
		"a scope's unlisted host": {`<script type="importmap">{"scopes":{"/":{"x":"https://evil.example/x.js"}}}</script>`,
			lint.CodeRemoteImport},
		"plain http": {`<script type="importmap">{"imports":{"x":"http://unpkg.com/x@1.0.0/x.js"}}</script>`,
			lint.CodeRemoteImport},
		"a data address": {`<script type="importmap">{"imports":{"x":"data:text/javascript,alert(1)"}}</script>`,
			lint.CodeRemoteImport},
		"a folder": {`<script type="importmap">{"imports":{"lodash/":"https://cdn.jsdelivr.net/npm/lodash-es@4.17.21/"}}</script>`,
			lint.CodeRemoteImport},
		"an unpinned package": {`<script type="importmap">{"imports":{"vue":"https://unpkg.com/vue/dist/vue.esm-browser.js"}}</script>`,
			lint.CodeUnpinned},
		"an external map": {`<script type="importmap" src="https://evil.example/map.json"></script>`,
			lint.CodeRemoteImport},
		"a map that is not JSON": {`<script type="importmap">{"imports":{"x":"https://evil.example/x.js",}}</script>`,
			lint.CodeUnparseable},
		"an upper-case type": {`<script type=" ImportMap ">{"imports":{"x":"https://evil.example/x.js"}}</script>`,
			lint.CodeRemoteImport},
	} {
		_, err := lint.HTML("index.html", []byte(c.src), opts(&resolver{}))
		if got := codes(t, err); !slices.Contains(got, c.want) {
			t.Errorf("%s answered %v, want %s", name, got, c.want)
		}
	}
}

// A MAP OF THE VERSION'S OWN FILES IS PUBLISHED BYTE FOR BYTE, nonced like
// every script element.
func TestAnImportMapOfLocalFilesIsUnchanged(t *testing.T) {
	t.Parallel()
	body := `{ "imports": { "app": "./app.js", "util": "/x/util.js" }, "scopes": {} }`
	page, err := lint.HTML("index.html", []byte(`<script type="importmap">`+body+`</script>`), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := `<script` + nonce + ` type="importmap">` + body + `</script>`; string(page.Text) != want {
		t.Fatalf("the page reads\n%s\nwant\n%s", page.Text, want)
	}
}

// THE VERIFICATION HOLDS EVERY IMPORT MAP OF THE ANSWER: a remote entry that
// reached the output unrewritten, in imports or in a scope, is refused, and so
// is a map the reading cannot take apart.
func TestTheVerificationRefusesAnImportMapNamingAnotherOrigin(t *testing.T) {
	t.Parallel()
	open := `<script` + nonce + ` type="importmap">`
	if err := lint.Verify([]byte(open+`{"imports":{"a":"./a.js"}}</script>`), 1); err != nil {
		t.Fatalf("a map of local files was refused: %v", err)
	}
	for name, page := range map[string]string{
		"an import":      open + `{"imports":{"a":"https://evil.example/a.js"}}</script>`,
		"a scope":        open + `{"scopes":{"/":{"a":"//evil.example/a.js"}}}</script>`,
		"a duplicate":    open + `{"imports":{"a":"./a.js","a":"https://evil.example/a.js"}}</script>`,
		"an unread map":  open + `{"imports":[}</script>`,
		"a padded type":  `<script` + nonce + ` type=" importmap ">{"imports":{"a":"https://evil.example/a.js"}}</script>`,
		"after a script": `<script` + nonce + `></script>` + open + `{"imports":{"a":"https://evil.example/a.js"}}</script>`,
	} {
		want := strings.Count(page, artifacts.NoncePlaceholder)
		if err := lint.Verify([]byte(page), want); err == nil {
			t.Errorf("a page with %s was verified", name)
		}
	}
}
