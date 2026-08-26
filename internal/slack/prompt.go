package slack

import "github.com/crewlet/crewlet/internal/notify"

// Prompt is Slack's notification prompt.
//
// The triage guidance — deciding whether a message was addressed to this
// agent, and staying silent when it was not — is backend-neutral and lives
// in [notify.ChatPrompt]. What is Slack-specific is only how a mention is
// written and read, and the one thing an agent gets wrong here more than
// anywhere else: Slack shows `@ana` and DELIVERS `<@U024BE7LH>`, so an agent
// that writes what it sees notifies nobody.

// Prompt builds the chat prompt for this backend.
func Prompt() notify.ChatPrompt {
	return notify.ChatPrompt{
		Backend:     Backend,
		Label:       "Slack",
		DirectKinds: DirectKinds,
		// Slack ids carry their kind in the first letter, so the prefix
		// test is exact rather than a guess — and it is the only answer
		// for an app_mention, whose payload omits the channel type.
		DMPrefix:      DMPrefix,
		Collectives:   "`<!channel>` / `<!here>` / `<!everyone>`",
		SelfReference: selfReference,
		MentionHint:   mentionHint,
		IdentityNote:  identityNote,
	}
}

// selfReference is how somebody addresses this agent here: as MARKUP,
// resolved by Slack before delivery.
//
// The id rather than a display name, because that is what actually appears
// in the message body the agent reads. A prompt that told it to look for
// "@agent-swe" would have it scanning for a string Slack never sends.
func selfReference(metadata map[string]string) string {
	if id := metadata["bot_user_id"]; id != "" {
		return "`<@" + id + ">`"
	}
	return ""
}

// mentionHint tells the agent how to WRITE a mention, which on this backend
// is NOT how it reads one — the one difference worth a line of prompt.
//
// A Slack client shows `@ana` and sends `<@U024BE7LH>`. An agent that echoes
// what it sees produces literal text that renders as itself and notifies
// nobody, and the failure is silent: the message posts, it looks right in
// the transcript, and the person it was for is never told.
func mentionHint(map[string]string) string {
	return "**Mentions:** write them as `<@USER_ID>`, never as `@name` —" +
		" Slack resolves a mention to an id before delivery, so the plain" +
		" text renders as itself and notifies nobody. Look the colleague up" +
		" to get their Slack user id first. Channels are `<#CHANNEL_ID>`."
}

// identityNote names the agent's own user id.
//
// It is what lets the agent recognise its own replies when it reads a thread
// back, and — because Slack's mention markup is the same id — what lets it
// tell being named from a colleague being named.
func identityNote(metadata map[string]string) string {
	id := metadata["bot_user_id"]
	if id == "" {
		return ""
	}
	return "Your Slack user id is `" + id + "` — messages from it in a" +
		" thread are your own, and `<@" + id + ">` in a message is somebody" +
		" addressing you."
}
