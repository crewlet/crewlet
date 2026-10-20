# Answers captured from the engine

The setup surface's answer (`GET /setup/integrations`) carries every label,
help line, choice and per-agent block a connect form draws, written in each
vendor's Go package. Rather than restate that copy, the demo company's
listings are the engine's own answers for FairSplit's document:

- `setup.with-address.json`: the document as `fixtures/fairsplit.document.ts`
  writes it, on a node whose `api.external_url` (Tier A) is
  `https://fairsplit.example.com`, which is what makes each agent's Slack
  manifest and delivery address available. FairSplit's own node serves on it.
- `setup.no-address.json`: the same document on a node with no address,
  which the engine's own suites build and no configuration a node boots
  reaches.

Each was normalised in two ways: written back as two-space JSON with no
escaped `<`, `>` or `&` (Go's encoder escapes them), and an em dash in the
engine's own copy (the GitHub organization webhook choice's hint) written as
a colon, to follow the house style for copy that ships.

The demo never serves them as they stand. It derives every listing from the
document (`harness/engine/setup.ts`), and uses these two ways: the first is
the CATALOGUE of every form's definitions, and both are the oracle
`check/integrations.test.ts` holds the derivation to, field for field. Two
fields are left out of that comparison: `revision_id`, which is the capturing
engine's own, and `can_provision`, which the capture reports `false` for
every tool because the capture runs the setup surface without the engine's
setup passes. A running engine serves passes for Atlassian, Datadog, Jira,
Confluence, GitLab, Mattermost and GitHub, and the demo answers as it does.

## Capturing them again

After a change to a vendor's requirements, or to `fairsplit.document.ts`,
capture both again from the engine checkout this folder belongs to:

1. Write the company document as JSON:

   ```sh
   cd dashboard
   node --input-type=module -e '
     import { createServer } from "vite";
     const s = await createServer({ configFile: false, server: { middlewareMode: true },
       appType: "custom", resolve: { alias: { "~": process.cwd() + "/src" } } });
     const m = await s.ssrLoadModule("/prototype/fixtures/fairsplit.document.ts");
     process.stdout.write(JSON.stringify(m.fairsplitDocument(), null, 2));
     await s.close();' > /tmp/fairsplit.json
   ```

2. Add this test to `internal/api/setupapi/` as a file of its own, run it,
   and delete it. It is never committed: it reuses that package's test
   harness, which no other package can reach.

   ```go
   package setupapi_test

   import (
   	"net/http"
   	"os"
   	"path/filepath"
   	"testing"
   )

   func TestPrototypeCapture(t *testing.T) {
   	out := os.Getenv("PROTO_CAPTURE")
   	doc, err := os.ReadFile(os.Getenv("PROTO_DOC"))
   	if out == "" || err != nil {
   		t.Skip("no capture asked for")
   	}
   	for name, base := range map[string]string{
   		"setup.no-address.json":   "",
   		"setup.with-address.json": "https://fairsplit.example.com",
   	} {
   		s := newSurfaceWithApps(t, nil, base)
   		res := s.do(t, http.MethodPut, "/config", string(doc), map[string]string{"X-Summary": "import"})
   		if res.Code != http.StatusCreated {
   			t.Fatalf("import = %d: %s", res.Code, res.Body)
   		}
   		res = s.do(t, http.MethodGet, "/setup/integrations", "", nil)
   		if res.Code != http.StatusOK {
   			t.Fatalf("list = %d: %s", res.Code, res.Body)
   		}
   		if err := os.WriteFile(filepath.Join(out, name), res.Body.Bytes(), 0o644); err != nil {
   			t.Fatal(err)
   		}
   	}
   }
   ```

   ```sh
   PROTO_CAPTURE=$PWD/dashboard/prototype/fixtures/engine PROTO_DOC=/tmp/fairsplit.json \
     go test -count=1 -run TestPrototypeCapture ./internal/api/setupapi/
   ```

3. Apply the normalisation above, then format the two files with
   `npx prettier --write prototype`.

The import in step 2 is also the proof that the engine accepts the demo
company's document as it stands: a document it refused would fail there.
