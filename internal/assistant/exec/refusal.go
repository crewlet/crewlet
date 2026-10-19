package exec

import (
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// RefusalOf is a refused answer as the engine reads it: the API's own code,
// the sentence the envelope says to a person (`message`), the route's `hint`
// where it gave one, where the authority table refused the deciding rule's
// `reason` beside the permissions that would have admitted the person, and
// the route's `detail` (what happened to this request, in the terms of the
// request) as [wire.Refusal.Detail], which only the model reads.
//
// THE REASON TRAVELS WITH THE PERMISSIONS, because they alone cannot say
// whether access is the way past: a stale proof names the permissions that
// admitted the person.
//
// READ THROUGH internal/api/httpjson's decoder, the one reader of the envelope,
// so the keys are spelled where they are written. An answer that is not the
// envelope (a body some handler wrote by hand, or none) is a refusal by its
// status alone: `http_<status>` and the status's own text.
//
// What a person is shown of it is decided once, by
// internal/assistant/cards.PersonRefusal, at each place a refusal leaves the
// engine for a person.
func RefusalOf(resp Response) wire.Refusal {
	refused, ok := httpjson.Decode(resp.Body)
	if !ok {
		return wire.Refusal{Code: fmt.Sprintf("http_%d", resp.Status),
			Message: http.StatusText(resp.Status)}
	}
	return wire.Refusal{Code: string(refused.Code), Message: refused.Message,
		Hint: refused.Hint, Reason: refused.Reason, Grants: refused.Grants,
		Detail: refused.Detail}
}
