package exec

import (
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// RefusalOf is a refused answer as a step, a card or a proposal shows it: the
// API's own code, the sentence it said (the route's `detail` where it gave one,
// which says what happened to this request, else the code's own sentence), the
// route's `hint` where it gave one, and the permissions that would have
// admitted the person.
//
// READ THROUGH internal/api/httpjson's decoder, the one reader of the envelope,
// so the keys are spelled where they are written. An answer that is not the
// envelope (a body some handler wrote by hand, or none) is a refusal by its
// status alone: `http_<status>` and the status's own text.
func RefusalOf(resp Response) wire.Refusal {
	refused, ok := httpjson.Decode(resp.Body)
	if !ok {
		return wire.Refusal{Code: fmt.Sprintf("http_%d", resp.Status),
			Message: http.StatusText(resp.Status)}
	}
	message := refused.Message
	if refused.Detail != "" {
		message = refused.Detail
	}
	return wire.Refusal{Code: string(refused.Code), Message: message, Hint: refused.Hint,
		Grants: refused.Grants}
}
