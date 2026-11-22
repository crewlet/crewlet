package growth

import "encoding/json"

// MarshalJSON sends a card's lists as lists, never null, however the product
// built it (a card with no findings is the common case): the dashboard reads
// the card as sent (TestTheDashboardReadsTheGrowthWire).
func (c Card) MarshalJSON() ([]byte, error) {
	type wire Card // the fields alone, so this method does not call itself
	w := wire(c)
	if w.Acts == nil {
		w.Acts = []CardAct{}
	}
	if w.Findings == nil {
		w.Findings = []string{}
	}
	return json.Marshal(w)
}

// MarshalJSON sends an act's changes as a list, never null: an account act
// may change no field the vendor names.
func (a CardAct) MarshalJSON() ([]byte, error) {
	type wire CardAct
	w := wire(a)
	if w.Changes == nil {
		w.Changes = []CardChange{}
	}
	return json.Marshal(w)
}
