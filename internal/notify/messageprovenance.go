package notify

import (
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
)

// MessageProvenance is what the fence around a delivered notification's
// message says of it, for the seat self: where it was read from, who wrote it
// and the trust they earn from self. It is for a reader of the message's words
// other than the trigger it was delivered in: the answer a reply records for a
// coding run parked on a question, which the run reads fenced under these
// facts. A coalesced notification's message is every constituent's, so its
// facts name every constituent's writer, at the least trust any of them earns,
// under the surface they share, or an integration's notification where they
// differ.
//
// THE SOURCE'S OWN ANSWER, [Prompt.Provenance] over the notification as it
// was delivered, the one its trigger's fence was written from ([FenceBody]),
// so the words are labelled as the trigger labelled them.
func MessageProvenance(prompts Prompts, n types.ExternalNotification, parties Parties,
	self string) (provenance.Source, provenance.Trust, []provenance.Author) {

	prompt := prompts.For(n.NotificationSource)
	copies := []Inbound{{Source: n.NotificationSource, EventType: n.SourceEventType,
		Sender: n.Sender, Subject: n.Subject, Metadata: n.Metadata}}
	if len(n.Messages) > 0 {
		copies = copies[:0]
		for _, m := range n.Messages {
			copies = append(copies, Inbound{Source: n.NotificationSource,
				EventType: m.SourceEventType, Sender: m.Sender, Metadata: m.Metadata})
		}
	}
	var (
		source  provenance.Source
		trusts  []provenance.Trust
		authors []provenance.Author
	)
	for i, in := range copies {
		p := prompt.Provenance(in, parties)
		switch {
		case i == 0:
			source = p.Source
		case p.Source != source:
			source = provenance.SourceNotification
		}
		trusts = append(trusts, provenance.TrustFor(self, p.Authors))
		for _, a := range p.Authors {
			if !slices.Contains(authors, a) {
				authors = append(authors, a)
			}
		}
	}
	return source, provenance.Least(trusts...), authors
}
