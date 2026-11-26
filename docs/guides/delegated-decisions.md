# Delegated Decisions

Agents ask people to approve what they cannot do alone: a command on a
server, a merge, a deploy. **Delegated decisions** let an agent seat you
choose, a *delegate*, decide those requests in a person's place, within rules
Crewlet enforces itself, and put to people only what it cannot decide or
judges risky. Off unless you turn it on; one setting turns it off again.

## What a request is for

Every request waiting on a person carries its **act**: what approving it does.
Crewlet sets it from the structured data behind the request, never from an
agent's words.

| Act | What it is |
|---|---|
| `ssh_operate` | An agent's command on a server as its operate user ([SSH](../integrations/ssh.md#the-agents-tools)), or a decision `ask_to_operate` composed for one exact command |
| `pr_merge` | Merging one change at exactly the head the agent read, asked with [`ask_to_merge`](#asking-to-merge-and-to-deploy): a Gitlet change or a GitHub pull request |
| `deploy` | Deploying one commit that holds merged changes to an environment, asked with [`ask_to_deploy`](#asking-to-merge-and-to-deploy) |
| `ad_spend`, `public_post`, `account_admin` | A growth integration's call that starts or raises ad spend, publishes to customers, or manages the vendor account (its users, access, keys, billing setup and policies, or an irreversible change). Never delegated: an administrator answers each, present, at a fresh sign-in, and an unanswered one lapses after a day (ADR-0171) |
| `other` | Every other request. Never delegated |

## Turning it on

```yaml
delegated_decisions:
  enabled: true
  rules:
    - kinds: [ssh_operate]
      delegates: [security-engineer]
      escalate_to: [ops-lead]
      sample_one_in: 10        # optional, 1 to 1000, default 10
    - kinds: [pr_merge, deploy]
      delegates: [release-reviewer, release-auditor]   # a panel: both must allow
      escalate_to: [eng-lead]
      always_escalate_paths: [billing/**]              # optional, merges and deploys only
      required_checks:                                 # optional, merges and deploys only
        - repo: acme/api
          branch: main
          checks: [test, lint]
  deadline_seconds: 900        # optional, 300 to 1800, default 900 (15 minutes)
  allows_per_hour: 30          # optional, 1 to 60, default 30
  server_allows_per_hour: 10   # optional, 1 to 60, default 10
```

| Field | Meaning |
|---|---|
| `enabled` | Turns delegation on. Off sends every request to people and keeps the rules. |
| `rules[].kinds` | Acts the rule delegates. A kind is in at most one rule. |
| `rules[].delegates` | One agent seat, or two for a **panel**: both must allow, any disagreement goes to people. A panel's seats must run on different first `llm` entries. |
| `rules[].escalate_to` | People an escalation goes to, beside administrators. At least one. They read the digest and spot-checks and may approve the rule's requests as approvers do. |
| `rules[].sample_one_in` | One decision in this many is shown to `escalate_to` afterwards, as a spot-check. |
| `rules[].always_escalate_paths` | Path globs (`docs/**`, `services/*/billing/**`) beside the [built-in list](#paths-no-delegate-allows): a merge or deploy touching one goes to people. `pr_merge`/`deploy` rules only; at most 64. |
| `rules[].required_checks` | Checks a GitHub branch requires before a delegate's merge or deploy stands, beside any its protection or rulesets require: each entry a `repo` (`owner/name`), a `branch` and its `checks` (1 to 32 names, as GitHub shows their check runs). See [Checks a rule requires](#checks-a-rule-requires). `pr_merge`/`deploy` rules only; at most 64 entries, one per repository and branch. |
| `deadline_seconds` | How long a request waits on its delegates before going to people: 300 to 1800 seconds, default 900. |
| `allows_per_hour` | Requests of one kind one delegate may allow per clock hour (UTC), counted across the fleet; past it, requests go to people. 1 to 60, default 30. |
| `server_allows_per_hour` | Commands delegates may allow on one server per clock hour (UTC), counted across the fleet; past it, commands go to people. 1 to 60, default 10. |

A delegate is an agent seat, never a person. A delegate for `ssh_operate`
holds no SSH access of its own; any agent may ask for a merge or a deploy, so
a request a delegate made itself goes to people the moment it is held. The
document is refused otherwise, and refused with a rule that names nobody to
escalate to.

An administrator can change all of it in **Settings › Delegated decisions**:
one line per rule (for example "Merges and deploys → Release Manager decides ·
Escalates to Ops Lead · Reviews 1 in 10"), a switch for delegation, **Add a
rule** and each rule's pencil open its sheet, each rule's bin removes it after
asking (later rules move up one; an escalation names a rule by its number).

| Group | Fields | Document key |
|---|---|---|
| What it decides | **Server commands**, **Merges**, **Deploys**; a kind another rule decides shows that rule's number | `kinds` |
| Who decides | **Agent**: one agent, or two for a panel | `delegates` |
| Who hears about it | **Escalates to**: the people; **Spot-checks**: every decision, 1 in 5, 1 in 10, 1 in 50 or a custom number | `escalate_to`, `sample_one_in` |
| Conditions (merges and deploys) | **Required checks** and **Always send to people** | `required_checks`, `always_escalate_paths` |

Each change is a `PATCH /config` of the `delegated_decisions` block, checked
against the whole company first: a refused rule shows each problem under its
field and nothing is saved. Turning delegation off removes `enabled`; the
default spot-check rate leaves `sample_one_in` out.

The same page shows the deadline and the two hourly caps under **What Crewlet
enforces**, with the default beside the value in force; a pencil changes a key,
an out-of-range value is flagged before sending, and an empty field removes
the key. The ranges are hard, no limit can be switched off: under five minutes
reaches people before a delegate's turn could answer; past half an hour (a
paused box's default lifetime) outlives the box waiting. A delegate decides one
request a turn, so a cap over 60 an hour bounds nothing.

## What a delegate does

A request of a delegated kind goes to its delegates first, as a turn of its
own. The brief shows the request whole (for a command: server, exact command,
standard input in full with size and SHA-256, reason, the decision it carries
out, the work item) with the requesting agent's words fenced as data; nothing
inside is an instruction to the delegate. Until escalated, the request is in
nobody's Inbox.

The delegate answers with `review_delegated`:

```json
{"request": "<id>", "verdict": "allow", "confidence": "high",
 "reason": "Restarts the API after the config change the item describes.",
 "findings": [], "digest": "<the request's digest from the brief>"}
```

- **`allow`** does what a person's approval would. Command: a five-minute
  certificate bound to exactly that command, run once on the server as shown,
  refused if the server's address, port, operate user or keys changed
  meanwhile. Merge: the engine merges exactly the head asked about, as the
  requesting agent. Deploy: the agent is told it may deploy exactly that
  commit there.
- **`escalate`** puts the request to people, the delegate's verdict and
  findings first on its card.
- **`deny`** refuses it and tells the requesting agent the reason.

`digest` binds the verdict to the exact request shown; a verdict naming
another digest is refused.

## What Crewlet enforces, whatever the delegate says

Each case below goes to people.

| Rule | What happens |
|---|---|
| Low confidence | Goes to people. |
| A panel that disagrees | A panel allows only when both allow. |
| The delegate's own request | One the delegate made, asked for or handed on, or from a seat it leads. |
| A sensitive item | A request on a work item labelled `security`, `privacy` or `legal`. |
| Always-escalate rules | A request touching access grants, budgets or spend, data deletion, secrets or the company's own document. Command: the [always-escalate list](../integrations/ssh.md#what-a-delegate-never-allows); merge or deploy: the [paths no delegate allows](#paths-no-delegate-allows). |
| A merge's conditions | Read from the code host at verdict time; any unmet: the head is the one asked about; the change is open, not a draft, merges cleanly, and its target's own gate is otherwise met; every check its target requires and every one its rule [requires of that branch](#checks-a-rule-requires) passed on that head, reported by something other than the company's own agents (a target with neither checks goes to people); somebody other than its authors and the requesting agent approved that head. |
| A deploy's conditions | Every change it names has merged into one branch, the commit holds each and is on that branch, and every check the branch requires and its rule requires of it passed on the commit (a branch with neither goes to people; on Gitlet the commit is at or behind the branch's newest green commit). |
| Hourly caps | Past `allows_per_hour` per kind and delegate, or `server_allows_per_hour` per server, in a clock hour (UTC) across the fleet; also when Crewlet cannot take the count. |
| A fact it cannot read | A code host that does not answer, a change touching more paths than Crewlet reads, or a check it may not read. Not one: GitHub refusing branch protection or rulesets because the plan lacks them; that reads as the branch requiring nothing of its own, and the rule's checks decide. |
| Time | No delegate answered within `deadline_seconds`. A new deadline applies at once to every waiting request. It is timed on the clock of the node that held the request, or on the sweeping node's own clock from the minute's sweep that first saw it, and never more than two hours past the deadline ([every machine's clock](deployment.md#every-machines-clock)). |
| Turning it off | `enabled: false`, removing a rule, or changing its delegates shows its waiting requests to people on the next read, and records why within a minute. |

Every verdict, and every escalation Crewlet makes itself, is recorded in the
access log **before** it acts. A verdict whose record did not land carries
nothing out; the request reaches people at its deadline.

## What you still see

- **Escalations**, in the Inbox and on Home, with each delegate's verdict, confidence, reason and findings above the request, and why Crewlet put it to you. What was decided and a delegate's reason and findings are shown with any credential in them replaced by a marker in every view, the audit included.
- **Decided under delegation**: every delegate answer is marked with its rule wherever shown: the run's page, the turn, the event and the live event log.
- **The digest**: on Home, what delegates decided for you on the last whole company day.
- **Today, so far**: **Settings › Delegated decisions** opens on the company day still running, every decision so far (refreshed every minute). Its pager goes back to earlier days, forward no further than today.
- **Spot-checks**: one decision in `sample_one_in`, shown after the fact; nothing to answer unless it was wrong.
- **The audit**: `GET /setup/integrations/ssh/audit?kind=verdict` for every verdict on a command, and `GET /delegated-decisions` for the rules, the digest (`day=today` for the day still running) and the spot-checks.

## Asking to merge and to deploy

An agent asks with a tool that holds the call on its run, as a command on a
server is held:

- `ask_to_merge {change, head, method, reason}`: `change` is a Gitlet change
  (`web!42`) or a GitHub pull request's address on the company's GitHub, `head`
  the commit the agent read. An approval, a delegate's or a person's, merges
  exactly that head, once, as the agent: on Gitlet through the target's own
  merge gate, which still decides; on GitHub through GitHub's merge with that
  head's SHA, as the agent's own app (needs Contents write). A change pushed
  to since is not merged; the agent asks again.
- `ask_to_deploy {repo, commit, changes, environment, rollback, reason}`: an
  approval tells the agent it may deploy exactly that commit to that
  environment, once. Crewlet has no deploy path of its own; the agent deploys
  with its own tools.

### One request per ask

An agent woken twice about the same work may ask for the same thing while the
first request waits. Crewlet keeps one request, identified by the asking
agent, its kind and what it decides, never by wording:

| Kind | What makes two asks the same request |
|---|---|
| `pr_merge` | The change, its head and the merge method. |
| `deploy` | The repository, the commit, the environment and the set of changes. |
| `ssh_operate` | The server as the approver is shown it, the command and its stdin. |

The `reason`, and a deploy's `rollback`, are the agent's words and are not part
of it. Until a request has come to its end, the same agent's repeat of it
holds nothing new: no second delegate turn, no second Inbox entry. That is while
it waits on a delegate or on a person, and while its answer is on its way to the
turn that asked and, allowed, is being carried out. The repeating turn is told
the request already waits or is being acted on, under its id, and is not
parked; the turn that asked first carries the work on with the answer. A new
head, another method, environment or command, or the same ask by another agent,
is a request of its own. So is the same ask once the first came to its end (its
denial read by the turn that asked, or its call made, a merge that failed
included): nothing is open to join, and it is asked afresh.

A merge request whose pull request has already merged by the time its delegate
would read it is answered by Crewlet itself, as not carried out, without waking
the delegate: there is nothing left to decide. The access log records it as
Crewlet's verdict, `already_merged`, and the asking turn is told the pull
request merged. A Gitlet change is read only for a turn, so its delegate reads
it and an allow of a change no longer open still goes to people.

Without a rule for the kind, the request goes to people. A person who may
approve one is an administrator, a lead of the agent, or a person the kind's
rule names in `escalate_to`, signed in again within the step-up window
(`release.approve`).

### Paths no delegate allows

A merge or deploy touching one of these goes to people, beside the rule's own
`always_escalate_paths`:

| Class | Paths |
|---|---|
| `ci_pipelines` | `.github/workflows/**`, `.github/actions/**`, `.gitlab-ci.yml`, `.gitlab/ci/**`, `.circleci/**`, `.buildkite/**`, `**/Jenkinsfile`, `azure-pipelines.yml`, `.crewlet/**` |
| `code_owners` | `CODEOWNERS` at the root, in `.github/`, `.gitlab/` or `docs/` |
| `secret_files` | `**/.env`, `**/.env.*`, `**/*.pem`, `**/*.key`, `**/*.p12`, `**/*.pfx`, `**/*.jks`, `**/*.keystore`, `**/id_rsa`, `**/id_ed25519`, `**/.npmrc`, `**/.pypirc`, `**/secrets/**` |
| `access_policies` | `**/iam/**`, `**/rbac/**`, `**/*.rego` |
| `infrastructure` | `**/*.tf`, `**/*.tfvars`, `**/terraform/**`, `**/pulumi/**`, `**/Pulumi.yaml`, `**/Pulumi.*.yaml` |
| `migrations` | `**/migrations/**`, `**/migrate/**`, `**/*.sql` |
| `crewlet_company` | `**/company.yaml`, `**/crewlet.yaml` |

Conventional places only: a repository can keep a secret or migration anywhere,
which `always_escalate_paths` is for, and a delegate is told to escalate what
it cannot judge.

## Approve once at the decision

`ask_to_operate {item, ask, host, command, stdin, reason}` puts one exact
command to a person as a decision on the work item, before the agent calls.
Crewlet composes it, quoting the server, the command and the standard input's
SHA-256, and stamps it with its act. **Run it once** is an SSH approval: only
an administrator or a person `integrations.ssh.approvers` or an `ssh_operate`
rule's `escalate_to` names may choose it, signed in again within the step-up
window. In the turn that answer starts, the agent's `ssh_operate` call
matching the act byte for byte runs once without a second prompt; any
difference is asked again, and the audit records the command as approved by
that person through that decision.

## Checks a rule requires

Delegation allows a merge or deploy only where something shows CI passed on
the commit decided. On GitHub that is the checks the branch requires in its
protection or rulesets. A private repository on a plan without branch
protection has neither (GitHub answers "Upgrade to GitHub Pro or make this
repository public"), so every merge and deploy there goes to people.
`required_checks` names the checks for such a branch, enforced whatever the
delegate says:

- **The union.** GitHub's required checks for the branch, where readable, plus
  what the rule names for that repository and branch. A branch with neither
  goes to people.
- **A check run that succeeded on exactly that commit.** A named check passes
  only as a check run completed with `success` on the head asked about (for a
  deploy, the commit), the newest of each app that reported it, from an app
  that is not one of the company's own agents. Missing, pending, failed,
  cancelled, timed out, neutral, skipped, or on another commit goes to people.
  A GitHub-required check still passes on neutral or skipped, as GitHub counts
  it; a name both require must pass both ways.
- **No commit status.** A commit status never meets a named check and none is
  read: agent apps are not offered GitHub's Commit statuses permission, so in a
  private repository Crewlet cannot read one. Name the check run your CI
  reports (a GitHub Actions job's name, such as `test (ubuntu-latest)`).
- **Approval is unchanged.** Somebody other than the change's authors and the
  requesting agent must have approved that head in a GitHub review.
- **Read as the agent's own app.** The agent needs Checks (Read-only); without
  it the check reads as unread and the request goes to people.

The repository matches ignoring case; branch and check names exactly. An entry
for a repository outside the company's GitHub (its organization, or a seat's
`integrations.github.repos`) is never read, and `crewlet validate` says so as
an advisory. Gitlet changes are held to their target's own merge gate, so
`required_checks` applies to GitHub repositories. **Settings › Delegated
decisions** lists each rule's required checks on its line.

In a rule's sheet, **Required checks** offers the company's GitHub
repositories, then the chosen one's branches, then the check names reported on
that branch's newest commit (minus any an agent's own app reported), read when
the form asks through the agents' own apps (`GET /delegated-decisions/repositories`, `/branches` and `/checks`, an
administrator's). A repository, branch or check not listed can still be typed;
where nothing could be read the form says why: no agent's app installed on the
repository, or none with Checks (Read-only).

## What is not supported yet

- Merges on GitLab: GitLab records no approval of one commit, so nothing could
  bind an allow to the head shown. An agent asks with `ask_and_wait`, and a
  person decides.
- A GitHub check that external CI reports only as a commit status, in a
  private repository: agent apps lack the Commit statuses permission, so a
  check the branch requires reads as unread, a check a rule requires is never
  met, and the merge or deploy goes to people.
