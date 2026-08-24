package mattermost

import "github.com/crewlet/crewlet/internal/notify"

// Prompt is Mattermost's notification prompt.
//
// The triage guidance — deciding whether a message was addressed to this
// agent, and staying silent when it was not — is backend-neutral and lives
// in [notify.ChatPrompt]. What is Mattermost-specific is only how a mention
// is written and read.
func Prompt() notify.ChatPrompt {
	return notify.ChatPrompt{
		Backend:     Backend,
		Label:       "Mattermost",
		DirectKinds: DirectKinds,
		// NO channel-id prefix. Mattermost ids are 26 opaque lowercase
		// alphanumerics, so a prefix test would mark arbitrary public
		// channels as direct messages — see [notify.Addressed].
		DMPrefix: "",
		// @all and @channel are synonyms for everyone in the channel;
		// @here narrows to whoever is online.
		Collectives:   "`@all` / `@channel` / `@here`",
		SelfReference: selfReference,
		MentionHint:   mentionHint,
		IdentityNote:  identityNote,
	}
}

// selfReference is how somebody addresses this agent here: LITERALLY, as
// `@username` in the message text.
//
// The username rather than the id, because that is what a person types and
// therefore what the agent will see written about itself. The id never
// appears in a message body on this backend.
func selfReference(metadata map[string]string) string {
	if name := metadata["bot_username"]; name != "" {
		return "`@" + name + "`"
	}
	return ""
}

// mentionHint tells the agent how to WRITE a mention, which on this backend
// is the same as how it reads one — worth saying explicitly, because a model
// that has seen other chat backends will reach for markup that renders here
// as literal text.
func mentionHint(metadata map[string]string) string {
	if metadata["bot_username"] == "" {
		return ""
	}
	return "**Mentions:** write them literally as `@username` — Mattermost" +
		" stores the text as typed, so any other markup renders as itself" +
		" and notifies nobody."
}

// identityNote names the agent's own user id.
//
// Both halves matter and they are not interchangeable: the ID is how a
// PAYLOAD names this seat, which is what lets the agent recognise its own
// posts when it reads a thread back; the USERNAME is how a person addresses
// it. An agent given only one of them either cannot tell its own replies
// from a colleague's, or cannot write a mention that reaches anybody.
func identityNote(metadata map[string]string) string {
	id, name := metadata["bot_user_id"], metadata["bot_username"]
	switch {
	case id != "" && name != "":
		return "Your Mattermost account is `@" + name + "` (user id `" + id +
			"`) — posts from that id in a thread are your own."
	case name != "":
		return "Your Mattermost account is `@" + name + "`."
	case id != "":
		return "Your Mattermost user id is `" + id +
			"` — posts from it in a thread are your own."
	}
	return ""
}
