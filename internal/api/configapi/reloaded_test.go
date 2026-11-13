package configapi_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
)

// A RE-ACTIVATION REFUSED FOR ITS SIZE SAYS SO, never as a fault of this node.
//
// A credential write re-activates the revision that reads the value it sealed,
// with a summary of its own, and the activation pointer carries the document
// sealed beside that summary: a pointer past what one key of the coordination
// store holds is refused before anything is stored. The write's answer says
// so, with the pointer's size and the bound, and what to do, rather than
// answering internal_error, which only this node's log could explain.
//
// Mutation: drop ReloadOutcome's case for a DocumentTooLargeError, and the
// refusal is answered internal_error.
func TestAReactivationRefusedForItsSizeSaysSo(t *testing.T) {
	t.Parallel()
	refused := &configapi.DocumentTooLargeError{DocumentBytes: 4_800_000, PointerBytes: 6_400_123}
	got := configapi.ReloadOutcome(configapi.Applied{}, fmt.Errorf("re-activate: %w", refused))
	if got.Error != httpjson.CodeBodyTooLarge {
		t.Fatalf("a re-activation refused for its size answers %q, want %q", got.Error,
			httpjson.CodeBodyTooLarge)
	}
	for _, size := range []int{refused.PointerBytes, coord.MaxValueBytes} {
		if !strings.Contains(got.Detail, strconv.Itoa(size)) {
			t.Errorf("the detail %q does not name %d", got.Detail, size)
		}
	}
	if got.RevisionID != "" || got.Epoch != 0 {
		t.Errorf("a refused re-activation names a revision: %+v", got)
	}
}
