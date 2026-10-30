package httpjson

import (
	"bytes"
	"encoding/json"
)

// The two keys a refusal on AUTHORITY carries beside the envelope's own: the
// rule's reason and the permissions that would have admitted the caller.
// Spelled here, beside the envelope's other keys, so the writer
// (internal/authz's RefusalFields, which names them DetailReason and
// DetailGrants) and the readers ([Decode], and `unauthorized`'s sentence,
// which [unauthorizedMessage] chooses by them) can never disagree about them.
const (
	KeyReason = "reason"
	KeyGrants = "grants"
)

// Refused is a refusal envelope read back from its bytes: the code, its
// sentence, the route's `detail` and `hint`, and an authority refusal's
// reason and grants. A field the envelope did not carry is empty.
type Refused struct {
	Code    Code
	Message string
	Detail  string
	Hint    string
	Reason  string
	Grants  []string
}

// Decode reads a refusal envelope back: what [Envelope] wrote, by the keys it
// wrote it under. False for a body that is not one (empty, not a JSON object,
// or an object with no `error` code), which a caller reads by its status
// alone.
//
// THE ONE READER OF THE ENVELOPE, for an in-process client of this API (the
// Dashboard Assistant reading the answers its own requests got): a second
// decoder would be a second spelling of every key, and the first key renamed
// here would be silently empty there.
func Decode(body []byte) (Refused, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return Refused{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return Refused{}, false
	}
	text := func(key string) string {
		var s string
		if raw, ok := fields[key]; ok {
			_ = json.Unmarshal(raw, &s)
		}
		return s
	}
	out := Refused{Code: Code(text(keyError)), Message: text(keyMessage),
		Detail: text(keyDetail), Hint: text(keyHint), Reason: text(KeyReason)}
	if out.Code == "" {
		return Refused{}, false
	}
	if raw, ok := fields[KeyGrants]; ok {
		_ = json.Unmarshal(raw, &out.Grants)
	}
	return out, true
}
