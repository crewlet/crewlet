package exec

import (
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// RefusalOf is a refused answer as the engine reads it: the API's own code,
// the envelope's `message`, the route's `hint`, where the authority table
// refused the deciding rule's `reason` beside the permissions that would have
// admitted the person (they alone cannot say whether access is the way past:
// a stale proof names them too), and the route's `detail` as
// [wire.Refusal.Detail], which only the model reads.
//
// A detail written for a person is also [wire.Refusal.Said] (ADR-0060), marked
// by [Response.ModelDetail]; unmarked, it names tools, fields and values and
// is the model's alone. The envelope is read by internal/api/httpjson's
// decoder; a non-envelope answer is a refusal by status (`http_<status>`).
// What a person sees is decided once, by cards.PersonRefusal.
func RefusalOf(resp Response) wire.Refusal {
	refused, ok := httpjson.Decode(resp.Body)
	if !ok {
		return wire.Refusal{Code: fmt.Sprintf("http_%d", resp.Status),
			Message: http.StatusText(resp.Status)}
	}
	said := ""
	if resp.ModelDetail != "" {
		said = refused.Detail
	}
	return wire.Refusal{Code: string(refused.Code), Message: refused.Message,
		Hint: refused.Hint, Reason: refused.Reason, Grants: refused.Grants,
		Detail: refused.Detail, Said: said}
}
