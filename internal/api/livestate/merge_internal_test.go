package livestate

import (
	"reflect"
	"strings"
	"testing"
)

// EVERY FIELD OF THE OVERLAY REACHES THE ROW A CLIENT IS SENT.
//
// The row is written key by key ([mergeStatus], [mergeOverlay]), so a field
// added to [Overlay] and to the projection reaches no snapshot, `agents` push,
// `seats` push or `GET /agents` until somebody also writes its key there, and
// nothing said so: `provider_limit` was projected, masked and classified, and
// never written, so every surface that reads it off a live row (the model-limit
// rows on Home and in the Inbox) was blank for as long as a limit lasted.
//
// Read off the struct's own JSON tags, so a field added later is held to this
// without anybody remembering to list it. The roster half writes everything
// but the call in flight, which has its own question.
func TestEveryOverlayFieldIsWrittenOntoTheRow(t *testing.T) {
	callOnly := map[string]bool{"live_call": true, "live_call_seq": true}
	whole := map[string]any{}
	mergeOverlay(whole, Overlay{})
	roster := map[string]any{}
	mergeStatus(roster, Overlay{})

	typ := reflect.TypeFor[Overlay]()
	for i := range typ.NumField() {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if key == "" || key == "-" {
			continue
		}
		if _, ok := whole[key]; !ok {
			t.Errorf("Overlay.%s (%q) is never written onto a merged row, so no client is "+
				"sent it", typ.Field(i).Name, key)
		}
		if _, ok := roster[key]; !ok && !callOnly[key] {
			t.Errorf("Overlay.%s (%q) is missing from the roster row (mergeStatus)",
				typ.Field(i).Name, key)
		}
	}
}
