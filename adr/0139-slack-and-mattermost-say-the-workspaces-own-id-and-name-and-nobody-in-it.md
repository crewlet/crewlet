# ADR-0139: Slack and Mattermost say the workspace's own id and name, and nobody in it

- **Status:** accepted
- **Authority:** `internal/slack`
- **Enforced-by:** `internal/slack.TestTheAtlasReadsTheSlackWorkspacesIdentity`, `internal/slack.TestASlackWorkspaceNoAppCanAskOfIsAFinding`, `internal/slack.TestNoCredentialShapeReachesTheAtlasFromSlack`, `internal/slack.TestASlackCallThatFailsOrIsRateLimitedFailsTheRead`, `internal/slack.TestAnUnconnectedSlackIsNotConfigured`, `internal/mattermost.TestTheAtlasReadsTheMattermostTeamsIdentity`, `internal/mattermost.TestAMattermostTeamNoCredentialIsShownIsAFinding`, `internal/mattermost.TestNoCredentialShapeReachesTheAtlasFromMattermost`, `internal/mattermost.TestAMattermostCallThatFailsFailsTheRead`, `internal/mattermost.TestAnUnconnectedMattermostIsNotConfigured`
- **Cost-when-tried:** not tried. Every other source reads a vendor under one company credential. A chat vendor is the first the engine holds no company read credential for: what it holds is one bot per agent.
- **Tag-status:** unreleased

## The decision

**A chat workspace is the account the settings already name, and the read adds its identity to it.** Slack's workspace is the account keyed `slack`; Mattermost's server is the account keyed `mattermost/` and the host of `integrations.mattermost.url` as the document writes it. The read claims that entry and says the workspace's own id (`account_ref`) and its own name (`account_name`): Slack's team id and team name, with the workspace's address as its scheme and host; Mattermost's team id and the team's display name. It never names the entry, which the settings do.

**It asks once, with a credential an agent already holds.** Slack is asked `auth.test`, which every bot token may call and which takes no scope. Mattermost is asked for the team `integrations.mattermost.team` names, by its slug, with one `GET`. The read asks under the bot token of the first agent, by handle, whose app or bot is switched on and whose token resolves; at Mattermost the provisioning token is asked last, so a system administrator's credential is on this path only where no bot's would do. A seat that is switched off is never asked through.

**One credential's refusal says nothing of the workspace.** A bot token Slack calls dead (an app somebody uninstalled) and a token Mattermost answers `401`, `403` or `404` (a bot somebody deactivated, or one that never joined the team) are passed over, and the next is asked. Where every credential is refused, the read is whole and the workspace is unread: a finding on the standing, which carries what an earlier read said (ADR-0130). Every other failure, a rate limit a call cannot wait out included, fails the read whole and no other credential is tried.

**Nobody is read.** Slack's answer says which bot user asked, and Mattermost's row carries the team's contact address and its invite id; none is decoded. No member, channel, message or post is ever listed.

**A company with no agent on the vendor is not read.** With no token to ask with, there is nothing to read, which is not a fault.

## Why the obvious alternative is wrong

**Keying Slack's workspace by its team id** names a second account beside the one the settings key `slack`, for every company, since the settings do not carry the id.

**Asking through every agent's token** costs a call an agent every hour to learn one workspace's name once, and would put a second value on a single-valued fact where one app is installed elsewhere.

**Asking Mattermost with the provisioning token first** puts a system administrator's credential on a path any member bot can serve.

**Failing the read on a dead bot token**, as a company credential's `401` does elsewhere, makes the source show a fault every hour for a seat somebody switched off at the vendor, which the seat's own access status already reports.

**Reading the server's version** has no fact to hold it on an account, and a fact added for it is a word the dashboard must learn. The workspace's identity is its id and its name.

**Listing members or channels** to "see the workspace" reads people, which the atlas never does, and needs scopes the agents' apps do not all hold.

## What this does not decide

Which channels a unit posts in, which the settings already say. Whether an agent's app is an entry. Whether a monitor's route to a channel is a relation. How a refused part is shown, which the dashboard decides.
