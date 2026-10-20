package notify

import (
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
)

// labelledMessage is a prompt that fences the message it delivers, and so can
// say what that fence says of it ([ChatPrompt]).
type labelledMessage interface {
	messageLabel(meta map[string]string, parties Parties, self string,
	) (provenance.Source, provenance.Trust, []provenance.Author)
}

// MessageProvenance is what the fence around a delivered notification's
// message says of it, for the seat self: where it was read from, who wrote it
// and the trust they earn from self. It is for a reader of the message's
// words other than the trigger it was delivered in: the answer a reply records
// for a coding run parked on a question, which the run reads fenced under
// these facts. A coalesced notification's message is every constituent's, so
// its facts name every constituent's writer, at the least trust any of them
// earns.
//
// THE PROMPT'S OWN ANSWER ([ChatPrompt.messageLabel]), the one its trigger's
// fence was written from, so the words are labelled as the trigger labelled
// them. FALSE for a notification whose source fences no message: only a chat
// surface's prompt does, and a reader hands any other source's words over as
// its trigger does, unfenced and defused.
func MessageProvenance(prompts Prompts, n types.ExternalNotification, parties Parties,
	self string) (provenance.Source, provenance.Trust, []provenance.Author, bool) {

	labels, ok := prompts.For(n.NotificationSource).(labelledMessage)
	if !ok {
		return "", "", nil, false
	}
	metas := []map[string]string{n.Metadata}
	if len(n.Messages) > 0 {
		metas = metas[:0]
		for _, m := range n.Messages {
			metas = append(metas, m.Metadata)
		}
	}
	var (
		source  provenance.Source
		trusts  []provenance.Trust
		authors []provenance.Author
	)
	for _, meta := range metas {
		src, trust, by := labels.messageLabel(meta, parties, self)
		source = src
		trusts = append(trusts, trust)
		for _, a := range by {
			if !slices.Contains(authors, a) {
				authors = append(authors, a)
			}
		}
	}
	return source, provenance.Least(trusts...), authors, true
}
