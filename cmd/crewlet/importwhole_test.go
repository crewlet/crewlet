package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// wholeCompanyDoc is a company file whose org has a unit, a seat in it and a
// seat that declares no handle.
const wholeCompanyDoc = cliCompanyDoc + `  - name: Software Engineer
    llm: main
units:
  - name: Engineering
    id: engineering
    lead: ceo
    roles:
      - name: CTO
        handle: cto
        llm: main
`

// AN IMPORT STORES THE WHOLE COMPANY, its seats and units with everything
// else, whichever route it takes.
//
// The org chart is part of the company document, so a revision is the whole
// file: offline, the store holds every seat and unit with each seat's handle
// written down; through a node, the file's own bytes travel to `PUT /config`,
// which stores and activates them. An import that divided the file — the org
// one way, the rest another — would leave a fleet whose revision and whose org
// disagree.
//
// Mutation: store the document without its roles, or send the node a re-encoded
// half, and the matching half fails.
func TestImportStoresTheWholeCompany(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	file := companyFile(t, dir, "company.yaml", func(string) string { return wholeCompanyDoc })

	t.Run("offline", func(t *testing.T) {
		if _, errs, err := configCmd(t, cfg, "import", file); err != nil {
			t.Fatalf("import: %v (%s)", err, errs)
		}
		company, _, err := companyFromStore(t.Context(), cfg)
		if err != nil {
			t.Fatalf("read the stored company: %v", err)
		}
		handles := map[string]bool{}
		for role := range company.EachRole() {
			handles[role.Handle] = true
		}
		for _, want := range []string{"ceo", "software-engineer", "cto"} {
			if !handles[want] {
				t.Errorf("the stored company holds seats %v, want %s with its "+
					"handle written down", handles, want)
			}
		}
		if len(company.Units) != 1 || company.Units[0].ID != "engineering" {
			t.Errorf("the stored company holds units %+v, want engineering", company.Units)
		}
	})

	t.Run("through a node", func(t *testing.T) {
		var (
			mu           sync.Mutex
			method, path string
			sent         []byte
		)
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			method, path, sent = r.Method, r.URL.Path, body
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"revision_id":"rev-whole","epoch":7}`)
		}))
		t.Cleanup(node.Close)
		t.Setenv(apiTokenEnv, "a-tier-a-token")

		out, errs, err := configCmd(t, cfg, "import", file, "-api", node.URL)
		if err != nil {
			t.Fatalf("import through the node: %v (%s)", err, errs)
		}
		if !strings.Contains(out, "rev-whole") {
			t.Errorf("the import does not name the revision the node stored: %q", out)
		}
		written, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if method != http.MethodPut || path != "/config" {
			t.Errorf("the import sent %s %s, want PUT /config", method, path)
		}
		if string(sent) != string(written) {
			t.Errorf("the node was sent\n%s\nwant the file's own bytes\n%s", sent, written)
		}
	})
}
