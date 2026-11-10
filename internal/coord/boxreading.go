package coord

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

// BoxReadings are what a run found in the box a seat's executor runs in, one
// per (seat, cell), so every node tells the seat the same box (ADR-0104). A
// reading is kept under the basis the box was made from and is told only while
// the document still makes the box that way. In the positions register, which
// no clock ages: the next run's reading replaces it, and a removed seat's go
// with it.
type BoxReadings interface {
	// BoxReading is seat's reading of its box in cell, false where none is kept.
	BoxReading(ctx context.Context, seat uuid.UUID, cell string) (BoxReading, bool, error)
	// ListBoxReadings is every seat's readings.
	ListBoxReadings(ctx context.Context) ([]BoxReading, error)
	// PutBoxReading keeps r, replacing the seat's reading in its cell.
	PutBoxReading(ctx context.Context, r BoxReading) error
	// DeleteBoxReading drops seat's reading in cell; none kept is no error.
	DeleteBoxReading(ctx context.Context, seat uuid.UUID, cell string) error
}

// BoxReading is one seat's box in one cell as a run found it.
type BoxReading struct {
	Seat uuid.UUID `json:"seat"`
	Cell string    `json:"cell"`
	// Basis fingerprints what the box was made from (config's BoxBasis).
	Basis string `json:"basis"`
	// MemoryBytes is the memory the box is held to, 0 where it could not be
	// read; Browser and ContainerRuntime name what it has, empty for none.
	MemoryBytes      int64     `json:"memory_bytes,omitempty"`
	Browser          string    `json:"browser,omitempty"`
	ContainerRuntime string    `json:"container_runtime,omitempty"`
	At               time.Time `json:"at"`
}

// BoxBrowsers are the browsers a reading names, in the order one is preferred,
// and BoxPlaywrightChromium the one a Playwright install carries: a reading's
// browser is one of them, so no other text reaches a seat's prompt.
var BoxBrowsers = []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "firefox"}

// BoxPlaywrightChromium is the browser of a box that has only Playwright's own
// download of Chromium.
const BoxPlaywrightChromium = "Playwright's Chromium"

// BoxContainerRuntimes are the container runtimes a reading names.
var BoxContainerRuntimes = []string{"docker", "podman"}

// Validate reports why a reading cannot be kept.
func (r BoxReading) Validate() error {
	switch {
	case r.Seat == uuid.Nil || r.Cell == "":
		return errors.New("coord: a box reading names its seat and its cell")
	case r.Basis == "":
		return errors.New("coord: a box reading names the basis its box was made from")
	case r.MemoryBytes < 0:
		return errors.New("coord: a box reading's memory is never negative")
	case r.Browser != "" && r.Browser != BoxPlaywrightChromium && !slices.Contains(BoxBrowsers, r.Browser):
		return errors.New("coord: a box reading names a browser from the closed list")
	case r.ContainerRuntime != "" && !slices.Contains(BoxContainerRuntimes, r.ContainerRuntime):
		return errors.New("coord: a box reading names a container runtime from the closed list")
	case r.At.IsZero():
		return errors.New("coord: a box reading needs the instant it was taken")
	}
	return nil
}

// BoxReadingClass is the key class a reading is filed under.
const BoxReadingClass = "box_reading"

// BoxReadingKey is seat's reading key in cell.
func BoxReadingKey(seat uuid.UUID, cell string) string {
	return DocumentKey(BoxReadingClass, seat.String(), cell)
}

// BoxReadingsFilter selects every seat's readings.
func BoxReadingsFilter() string { return DocumentFilter(BoxReadingClass) }
