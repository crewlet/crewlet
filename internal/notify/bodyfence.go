package notify

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The body a wake hands a seat, fenced as what somebody else wrote (ADR-0040):
// the one part of a trigger the engine did not write, and a model reads all
// its context as possible instruction, so the spine fences it once
// ([Render], a [provenance.Fence] under [Inbound.Fence]'s token, labelled via
// [Prompt.Provenance]). SPINE-SIDE: per-prompt fencing fails silently for the
// first prompt written without it. WHO WROTE IT IS THE PARSER'S FACT: it
// stamps the surface and the one author ([BodySourceField], [BodyAuthorField],
// [BodyAuthorKindField]); the prompt reads them back ([Stated]). An author the
// payload cannot name is NOBODY, `external` on a vendor's surface (the safe error), a colleague on one the
// ENGINE hosts ([Provenance.Hosted], [provenance.TrustForHosted]): only
// company principals write there.

// Metadata keys a parser stamps about the body it produced.
const (
	// BodySourceField is the surface the body was read from, as a
	// [provenance.Source]: `work_comment`, `page_body`, `change_note`, ….
	// Absent or unknown, the source's prompt falls back to the one its
	// surface always produces.
	BodySourceField = "body_source"

	// BodyAuthorField is the id, in the source's own namespace, of the one
	// party who wrote every word of the body — the commenter of a comment
	// just written, the reviewer of a review — and ABSENT whenever the
	// payload cannot say one did: a description somebody may have edited
	// since it was opened, a comment edited by whoever edited it.
	//
	// NOT the actor ([ActorField]), which is who CAUSED the event: the
	// person who assigns an issue did not write its description.
	BodyAuthorField = "body_author"

	// BodyAuthorKindField is the [iam.ActorKind] a FIRST-PARTY record
	// states for that author, which is what a fence names them as when the
	// roster no longer holds their handle. Never stamped by a vendor: an
	// account somewhere else carries no kind this engine can vouch for.
	BodyAuthorKindField = "body_author_kind"
)

// Provenance is what a notification's body is and who wrote it, as its fence
// labels them.
type Provenance struct {
	Source  provenance.Source
	Authors []provenance.Author

	// Hosted says the body is on a surface the engine hosts, where every
	// writer authenticated as a company principal: what it says about
	// writers [Provenance.Authors] does not name is
	// [provenance.TrustForHosted]'s. A vendor's surface leaves it false.
	Hosted bool
}

// Label is this provenance at the trust its authors earn from the reading seat
// self: [provenance.LabelFor] on a vendor's surface, and on the engine's own
// [provenance.LabelForHosted] with the authors named in full — a parser names
// a body's author only where one person wrote every word of it, so a body that
// names one names them all.
func (p Provenance) Label(self string) provenance.Label {
	if p.Hosted {
		return provenance.LabelForHosted(self, p.Source, p.Authors, len(p.Authors) > 0)
	}
	return provenance.LabelFor(self, p.Source, p.Authors)
}

// Fence wraps text in a fence of b carrying this provenance, at the trust its
// authors earn from the reading seat self.
func (p Provenance) Fence(b provenance.Boundary, self, text string) string {
	return p.Label(self).Fence(b, text)
}

// Render is the trigger a seat is woken with: the source's prompt, built over
// the notification with its body fenced.
//
// THE ONE CALLER OF [Prompt.Build]: a prompt is handed the body already inside
// its fence, under the wake's token and at the trust its authors earn from the
// recipient the delivery stamped ([RecipientField]) — see the file comment for
// why the fence is the spine's. A blank body is handed over as it is, so a
// prompt's own "(no text)" rendering still fires.
func Render(p Prompt, n Inbound, parties Parties) string {
	n.Body = FenceBody(p, n, parties)
	return p.Build(n, parties)
}

// FenceBody is n's body inside its fence, or the body as it is when blank.
func FenceBody(p Prompt, n Inbound, parties Parties) string {
	if strings.TrimSpace(n.Body) == "" {
		return n.Body
	}
	return p.Provenance(n, parties).Fence(n.Fence, n.Metadata[RecipientField], n.Body)
}

// SalientLabels are who wrote each message a delivered notification carries —
// the notification itself, or every constituent of a coalesced one, in the
// order they spoke — and where each was read from, at the trust its writers
// earn from self: one label per message, in the order the turn reads them as
// what it was asked (the interactions a learning worker is handed are the same
// messages in the same order).
//
// THE SOURCE'S OWN ANSWER, [Prompt.Provenance] over each message as it was
// delivered, so the text is labelled exactly as the body the trigger fenced.
func SalientLabels(prompts Prompts, n types.ExternalNotification, parties Parties,
	self string) []provenance.Label {

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
	labels := make([]provenance.Label, 0, len(copies))
	for _, in := range copies {
		labels = append(labels, prompt.Provenance(in, parties).Label(self))
	}
	return labels
}

// SalientLabel is who wrote a delivered notification's SALIENT body — the raw
// text its turn was asked, which a model other than the woken seat's executor
// reads (the turn-start relevance judgements) — and where it was read from, at
// the trust its writers earn from self.
//
// A COALESCED notification's salient body is every constituent's message, so
// its label is their [SalientLabels] merged ([provenance.Merge]): every
// constituent's writers at the LEAST trust any of them earns — a stranger's
// line in a burst of colleagues' makes the whole of it `external` — under the
// surface they share, or an integration's notification where they differ.
// Every constituent counts, the ones the merge's supersede rule left out of the
// text included, which can only lower the trust.
func SalientLabel(prompts Prompts, n types.ExternalNotification, parties Parties,
	self string) provenance.Label {
	return provenance.Merge(SalientLabels(prompts, n, parties, self)...)
}

// Stated is the provenance a parser stamped on n: the surface it named, or
// fallback where it named none this build knows, and the one author it named,
// resolved.
//
// THE AUTHOR IS RESOLVED IN THE SOURCE'S OWN NAMESPACE, through the registry
// that resolves its senders ([Parties.ByExternalID]), so a vendor account a
// seat holds reads as that colleague and a first-party handle as its seat.
// One the registry does not name is the kind a first-party record stated
// ([FirstPartyAuthor], e.g. the engine, an operator credential), else an
// outside party. No author stamped is nobody, which the fence labels
// `external` on a vendor's surface; the engine's own sources read it through
// [StatedHosted].
func Stated(n Inbound, parties Parties, fallback provenance.Source) Provenance {
	return stated(n, parties, fallback)
}

// StatedHosted is [Stated] for a source the engine hosts itself — its own
// tracker, knowledge base and chat — whose body nobody named is a colleague's
// rather than an outside party's ([Provenance.Hosted]).
func StatedHosted(n Inbound, parties Parties, fallback provenance.Source) Provenance {
	p := stated(n, parties, fallback)
	p.Hosted = true
	return p
}

func stated(n Inbound, parties Parties, fallback provenance.Source) Provenance {
	source := provenance.Source(n.Metadata[BodySourceField])
	if !source.Valid() {
		source = fallback
	}
	id := strings.TrimSpace(n.Metadata[BodyAuthorField])
	kind := iam.ActorKind(n.Metadata[BodyAuthorKindField])
	if id == "" {
		// A FIRST-PARTY RECORD MAY NAME ITS WRITER BY KIND ALONE: a change
		// the engine made, or an operator credential's, carries no handle,
		// and the kind it states is still a writer the company vouches for.
		switch kind {
		case iam.ActorSystem, iam.ActorOperator:
			return Provenance{Source: source, Authors: []provenance.Author{{Kind: kind}}}
		}
		return Provenance{Source: source}
	}
	if parties != nil {
		if party, ok := parties.ByExternalID(n.Source, id); ok {
			return Provenance{Source: source, Authors: []provenance.Author{PartyAuthor(party)}}
		}
	}
	return Provenance{Source: source, Authors: []provenance.Author{FirstPartyAuthor(id, kind, nil)}}
}

// FirstPartyAuthor is who wrote a first-party record, as a fence names them.
//
// THE ROSTER FIRST, by the handle the record carries, so a colleague is the
// seat the company holds; and where it no longer names them, THE KIND THE
// RECORD STATED — a person whose seat has since gone is still a person who
// wrote this, and an operator credential or the engine is still the company's
// — rather than the outside party a vendor's unknown id is. A record that
// stated neither is nobody's.
func FirstPartyAuthor(handle string, kind iam.ActorKind, parties Parties) provenance.Author {
	if parties != nil && handle != "" {
		if party, ok := parties.ByHandle(handle); ok {
			return PartyAuthor(party)
		}
	}
	switch kind {
	case iam.ActorHuman, iam.ActorAgent:
		return provenance.Author{Kind: kind, Seat: handle}
	case iam.ActorOperator, iam.ActorSystem:
		return provenance.Author{Kind: kind}
	}
	return provenance.Author{}
}

// PartyAuthor is a colleague as a fence names them: their seat, as a person or
// an agent.
func PartyAuthor(party Party) provenance.Author {
	kind := iam.ActorAgent
	if party.Human {
		kind = iam.ActorHuman
	}
	return provenance.Author{Kind: kind, Seat: party.Handle}
}
