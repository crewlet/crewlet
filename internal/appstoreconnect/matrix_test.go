package appstoreconnect_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/growth"
)

// The reference page lists every operation as the catalog classes it: a row
// the code gains, loses or reclasses changes the page, regenerated with
// `go run ./internal/growth/matrixdoc app_store_connect`.
//
// Mutation: reclass one operation in operations.go without regenerating the
// page, and this fails naming the page.
func TestTheCoverageMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	page := filepath.Join("..", "..", "docs", "reference", "app-store-connect-operations.md")
	got, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != growth.Matrix(appstoreconnect.Catalog()) {
		t.Errorf("%s is not the catalog's matrix: run go run ./internal/growth/matrixdoc app_store_connect > %s",
			page, page)
	}
}
