package cdp

import "encoding/json"

// List is a list a command's parameters must carry, sent as an empty list
// when it is nil. encoding/json writes a nil slice as null, which is no list:
// Chromium refuses a required list sent as null as an invalid parameter
// (-32602), so a field of a plain slice type would fail its command whenever a
// caller left it at its zero value — measured on both pinned Chromiums for
// DOMSnapshot.captureSnapshot's computedStyles and Storage.setCookies' cookies.
// A List's zero value asks for no item instead, and is a value of the
// parameter. Only a list the protocol requires is one; an optional list is a
// plain slice tagged omitempty, left off the wire when it is empty.
type List[T any] []T

// MarshalJSON writes l as a JSON array: [] when l is nil.
func (l List[T]) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]T(l))
}
