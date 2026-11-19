package planted

import "encoding/json"

// Answer is what the planted surface sends.
type Answer struct {
	Open   bool `json:"open,omitempty"`
	Marked bool `json:"marked,omitempty"` //wiregate:absent-is-false a mark the row draws only when set
	Count  int  `json:"count,omitzero"`
	Wrong  int  `json:"wrong,omitempty"` //wiregate:absent-is-false a number marked as a flag
	Bare   bool `json:"bare,omitempty"`  //wiregate:absent-is-false
	Always bool `json:"always"`
	Inner  Row  `json:"inner"`
}

// Row is a struct the answer reaches through a field.
type Row struct {
	Done bool `json:"done,omitempty"`
}

// Request is a body the surface decodes and never sends.
type Request struct {
	Confirm bool `json:"confirm,omitempty"`
}

// Stray carries a marker, and no surface sends it.
type Stray struct {
	Gone bool `json:"gone,omitempty"` //wiregate:absent-is-false nobody sends this struct
}

func write(any) {}

func decode(raw []byte, into any) error { return json.Unmarshal(raw, into) }

// Serve decodes a request and answers.
func Serve(raw []byte) {
	var req Request
	if decode(raw, &req) == nil {
		write(Answer{})
	}
}
