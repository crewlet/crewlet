package notify

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The body a wake hands a seat, fenced as what somebody else wrote (ADR-0040).
//
// A notification's body is the one part of its trigger the engine did not
// write — a comment, a description, a message, a monitor's text — and a model
// reads every word of its context as a possible instruction. So the spine
// fences it, once, before any prompt sees it: [Render] puts the body inside a
// [provenance.Fence] under the wake's own token ([Inbound.Fence]), labelled
// with what the source's prompt says it is and who wrote it
// ([Prompt.Provenance]), and hands the prompt that. A prompt can therefore
// weave the body anywhere it likes and never hand a seat somebody's words
// unlabelled — it is never given them bare.
//
// SPINE-SIDE, NOT PER PROMPT, because the alternative is one fence call per
// place a prompt renders a body: the code-host prompts render one in six
// openers each, and the first one written without it is a surface whose every
// wake addresses the seat in a stranger's voice with nothing failing.
//
// WHO WROTE IT IS THE PARSER'S FACT and the prompt's reading: the parser holds
// the payload, so it stamps the body's surface and its one author where the
// payload says one person wrote every word ([BodySourceField],
// [BodyAuthorField], [BodyAuthorKindField]), and a source's prompt reads them
// back ([Stated]). An author the payload cannot name — a description others
// may have edited, an edited comment, a monitor — is NOBODY, which the fence
// labels `external`: nothing vouches for it, and the error in the safe
// direction is a colleague read with more care rather than a stranger read as
// a colleague.

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
}

// Fence wraps text in a fence of b carrying this provenance, at the trust its
// authors earn from the reading seat self.
func (p Provenance) Fence(b provenance.Boundary, self, text string) string {
	return provenance.Fence(b, p.Source, provenance.TrustFor(self, p.Authors), p.Authors, text)
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

// SalientProvenance is who wrote a delivered notification's SALIENT body — the
// raw text its turn was asked, which a model other than the woken seat's
// executor reads (the turn-start relevance judgements) — and where it was read
// from, at the trust its writers earn from self.
//
// THE SOURCE'S OWN ANSWER, [Prompt.Provenance] over the notification as it was
// delivered, so the text is labelled exactly as the body the trigger fenced. A
// COALESCED notification's salient body is every constituent's message, so its
// label names every constituent's writers at the LEAST trust any of them earns
// — a stranger's line in a burst of colleagues' makes the whole of it
// `external` — under the surface they share, or an integration's notification
// where they differ. Every constituent counts, the ones the merge's supersede
// rule left out of the text included, which can only lower the trust.
func SalientProvenance(prompts Prompts, n types.ExternalNotification, parties Parties,
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

// Stated is the provenance a parser stamped on n: the surface it named, or
// fallback where it named none this build knows, and the one author it named,
// resolved.
//
// THE AUTHOR IS RESOLVED IN THE SOURCE'S OWN NAMESPACE, through the registry
// that resolves its senders ([Parties.ByExternalID]), so a vendor account a
// seat holds reads as that colleague and a first-party handle as the seat it
// is. One the registry does not name is the kind a first-party record stated
// ([FirstPartyAuthor]), and otherwise an outside party: an account nobody in
// the company holds. No author stamped is nobody, which the fence labels
// `external`.
func Stated(n Inbound, parties Parties, fallback provenance.Source) Provenance {
	source := provenance.Source(n.Metadata[BodySourceField])
	if !source.Valid() {
		source = fallback
	}
	id := strings.TrimSpace(n.Metadata[BodyAuthorField])
	if id == "" {
		return Provenance{Source: source}
	}
	if parties != nil {
		if party, ok := parties.ByExternalID(n.Source, id); ok {
			return Provenance{Source: source, Authors: []provenance.Author{PartyAuthor(party)}}
		}
	}
	kind := iam.ActorKind(n.Metadata[BodyAuthorKindField])
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
