package worklet

import "slices"

// RequestedWithoutAsk is the persons an agent's words ask for something
// without an ask: words that ask somebody for an answer or an action (`asks`,
// ADR-0113) and name these persons outside `ask`, the one seat the comment
// puts its question to. Nothing reaches such a person's Inbox as a question,
// so an agent waiting on them waits on words alone (ADR-0100, ADR-0122).
//
// THE ONE PREDICATE: the comment tools refuse such words before posting and
// the advisor's `person.requests_without_ask` counts those already written,
// so the two cannot disagree. Words that ask nothing (`asks: false`) name whom
// they like; a person's words, and an older build's record with no statement,
// are no agent's request.
func RequestedWithoutAsk(agent bool, asks *bool, ask string, named []string,
	person func(handle string) bool) []string {

	if !agent || asks == nil || !*asks || person == nil {
		return nil
	}
	var out []string
	for _, handle := range named {
		if handle != "" && handle != ask && person(handle) && !slices.Contains(out, handle) {
			out = append(out, handle)
		}
	}
	return out
}
