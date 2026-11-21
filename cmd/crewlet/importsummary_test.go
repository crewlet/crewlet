package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// AN IMPORT'S SUMMARY FITS THE ACTIVATION POINTER, OR IS REFUSED NAMING THE
// FLAG. The operator's own -summary past coord.MaxSummaryBytes is refused
// whole, before anything is stored, since a cut summary is not what they
// wrote; the one an import composes cuts the file's path, so a path of any
// length, every byte escaping to six, still fits.
//
// Mutation: drop the flag's bound, and the long summary is taken and refused
// only at the boot's publish; drop the path's cut, and the composed one is
// past the bound.
func TestAnImportsSummaryFitsThePointerOrIsRefused(t *testing.T) {
	t.Parallel()
	composed, err := importSummary("", strings.Repeat("\x01", 10_000))
	if err != nil {
		t.Fatalf("an import with no -summary: %v", err)
	}
	if err = coord.HoldSummary(composed); err != nil {
		t.Errorf("the summary an import composes over a long path: %v", err)
	}
	_, err = importSummary(strings.Repeat("s", coord.MaxSummaryBytes+1), "company.yaml")
	if !errors.Is(err, coord.ErrSummaryTooLong) || !strings.Contains(err.Error(), "-summary") {
		t.Errorf("a -summary past the bound = %v, want coord.ErrSummaryTooLong naming the flag", err)
	}
	if got, err := importSummary("  fixed the budget ", "company.yaml"); err != nil || got != "fixed the budget" {
		t.Errorf("a -summary inside the bound = %q, %v, want it trimmed and kept", got, err)
	}
}

// THE SUMMARIES A BOOT'S IMPORT AND A PROVISIONING RUN COMPOSE FIT THE
// ACTIVATION POINTER AT THEIR WORST: a -company path of any length, and a run
// that sealed many long names, every byte escaping to six. A boot's import
// past the bound is refused at its publish, and a run's re-activation leaves
// the values it sealed inactive.
//
// Mutation: drop the path's cut or the names' cut, and its summary is past the
// bound.
func TestTheSummariesABootAndAProvisioningRunComposeFitThePointer(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("\x01", 10_000)
	for _, override := range []bool{false, true} {
		if err := coord.HoldSummary(seedSummary(tierBSeed{Path: long, Override: override})); err != nil {
			t.Errorf("the summary of a boot's import (override %v) over a long path: %v", override, err)
		}
	}
	names := []string{long, long, long}
	if err := coord.HoldSummary(provisionSummary(names)); err != nil {
		t.Errorf("the summary of a provisioning run that sealed long names: %v", err)
	}
}
