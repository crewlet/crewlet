package iamapi_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE DASHBOARD PAGES THE DIRECTORY AT THE ENGINE'S SIZE: Access reads the
// directory in pages as large as it serves ([iamdomain.MaxPageSize]), and the
// engine clamps a larger ask. A figure above it is a page the screen counts as
// short and so as the last; one below asks the engine more often than it
// needs to.
//
// Mutation: move either figure and this fails.
func TestTheDashboardPagesTheDirectoryAtTheEnginesSize(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "DIRECTORY_PAGE_MAX")
	if err != nil {
		t.Fatal(err)
	}
	if got != strconv.Itoa(iamdomain.MaxPageSize) {
		t.Errorf("the dashboard's DIRECTORY_PAGE_MAX is %s and the engine's page is %d: "+
			"change it in contract/identity.ts", got, iamdomain.MaxPageSize)
	}
}
