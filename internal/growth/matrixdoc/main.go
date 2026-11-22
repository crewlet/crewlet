// Command matrixdoc prints a growth integration's coverage matrix, the page
// docs/reference/<product>-operations.md holds:
//
//	go run ./internal/growth/matrixdoc app_store_connect > docs/reference/app-store-connect-operations.md
//	go run ./internal/growth/matrixdoc apple_ads > docs/reference/apple-ads-operations.md
package main

import (
	"fmt"
	"os"

	_ "github.com/crewlet/crewlet/internal/appleads"        // registers its catalog
	_ "github.com/crewlet/crewlet/internal/appstoreconnect" // registers its catalog
	"github.com/crewlet/crewlet/internal/growth"
	_ "github.com/crewlet/crewlet/internal/metaads" // registers its catalog
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: matrixdoc <integration kind>")
		os.Exit(2)
	}
	c, ok := growth.CatalogOf(os.Args[1])
	if !ok {
		fmt.Fprintf(os.Stderr, "matrixdoc: no catalog for %q\n", os.Args[1])
		os.Exit(1)
	}
	fmt.Print(growth.Matrix(c))
}
