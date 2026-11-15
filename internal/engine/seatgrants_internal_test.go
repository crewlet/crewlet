package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A REQUEST'S GRANT IS SAVED THROUGH THE INSTALLED SAVER, and refused before it is.
func TestARequestsGrantIsSavedThroughTheInstalledSaver(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	desk := grantDesk{e: e}
	admit := func(bool) error { return nil }
	if err := desk.Grant(t.Context(), "sre", "abcde12345", false, admit); !errors.Is(err, ErrNoGrantSaver) {
		t.Fatalf("a grant with no saver installed = %v, want ErrNoGrantSaver", err)
	}
	var got string
	e.UseGrantSaver(func(_ context.Context, asker, id string, permanent bool, admit func(bool) error) error {
		got = fmt.Sprint(asker, "/", id, "/", permanent)
		return admit(true)
	})
	if err := desk.Grant(t.Context(), "sre", "abcde12345", true, admit); err != nil || got != "sre/abcde12345/true" {
		t.Errorf("a grant through the saver = %v, saved %q; want sre/abcde12345/true", err, got)
	}
}
