# Delegated Decisions

Agents ask people to approve what they cannot do alone: a command on a
server, a merge, a deploy. When a person
accepts the agent's recommendation almost every time, the Inbox fills with
requests that only need a look. **Delegated decisions** let an agent seat you
choose, a *delegate*, decide those requests in a person's place, within
rules Crewlet enforces itself, and put in front of people only what it cannot
decide or judges risky.

It is off unless you turn it on, and one setting turns it off again.

## What a request is for

Every request that waits on a person carries its **act**: what approving it
does. Crewlet sets it from the structured data that made the request, never
from an agent's words.

| Act | What it is |
|---|---|
| `ssh_operate` | An agent's command on a server as its operate user ([SSH](../integrations/ssh.md#the-agents-tools)), or a decision `ask_to_operate` composed for one exact command |
| `pr_merge` | Merging one change at exactly the head the agent read, asked with [`ask_to_merge`](#asking-to-merge-and-to-deploy): a Gitlet change or a GitHub pull request |
| `deploy` | Deploying one commit that holds merged changes to an environment, asked with [`ask_to_deploy`](#asking-to-merge-and-to-deploy) |
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
```

| Field | Meaning |
|---|---|
| `enabled` | Turns delegation on. Off puts every request to people at once and keeps the rules. |
| `rules[].kinds` | The acts the rule delegates. A kind is in at most one rule; `other` is never delegated. |
| `rules[].delegates` | One agent seat, or two for a **panel**: both must allow, and any disagreement goes to people. A panel's two seats must run on different first `llm` entries, so one model's blind spot is not both. |
| `rules[].escalate_to` | The people an escalation is put to, beside administrators. At least one. They read the digest and spot-checks, and may approve the rule's requests as approvers do. |
| `rules[].sample_one_in` | One decision in this many is shown to `escalate_to` after the fact, for a spot-check. |
| `rules[].always_escalate_paths` | Repository path globs (`docs/**`, `services/*/billing/**`) beside the [built-in list](#paths-no-delegate-allows): a merge or deploy touching one goes to people. Only for a rule of `pr_merge` or `deploy`; at most 64. |
| `rules[].required_checks` | The checks a GitHub repository's branch requires before a delegate's merge or deploy stands, beside any its branch protection or rulesets require: each entry a `repo` (`owner/name`), a `branch` and its `checks` (one to 32 names, as GitHub shows their check runs). See [Checks a rule requires](#checks-a-rule-requires). Only for a rule of `pr_merge` or `deploy`; at most 64 entries, one per repository and branch. |

A delegate is an agent seat, never a person. A delegate for `ssh_operate`
holds no SSH access of its own; any agent may ask for a merge or a deploy, so
a request a delegate made itself goes to people the moment it is held. The
document is refused otherwise, and refused with a rule that names nobody to
escalate to.

## What a delegate does

A request of a delegated kind goes to its delegates first, as a turn of its
own. The turn's brief shows the request whole (for a command: the server, the
command exactly, the standard input in full with its size and SHA-256, the
reason, the decision it carries out and the work item) with the requesting
agent's words fenced as data. Nothing inside a request is an instruction to
the delegate. Until it is escalated, the request is in nobody's Inbox.

The delegate answers with `review_delegated`:

```json
{"request": "<id>", "verdict": "allow", "confidence": "high",
 "reason": "Restarts the API after the config change the item describes.",
 "findings": [], "digest": "<the request's digest from the brief>"}
```

- **`allow`** carries out what a person's approval would. For a command, that
  is the answer a person's Allow once gives: a five-minute certificate bound to
  exactly that command, run once on the server as shown, and refused if the
  server's address, port, operate user or keys changed meanwhile. For a merge,
  the engine merges exactly the head asked about, as the requesting agent. For
  a deploy, the agent is told it may deploy exactly that commit there.
- **`escalate`** puts the request to people, with the delegate's verdict and
  findings shown first on its card.
- **`deny`** refuses it and tells the requesting agent the reason.

`digest` binds the verdict to the exact request the delegate was shown; a
verdict naming another digest is refused.

## What Crewlet enforces, whatever the delegate says

| Rule | What happens |
|---|---|
| Low confidence | Goes to people. |
| A panel that disagrees | Goes to people. A panel allows only when both allow. |
| The delegate's own request | A request the delegate made, asked for or handed on, or one from a seat it leads, goes to people. |
| A sensitive item | A request on a work item labelled `security`, `privacy` or `legal` goes to people. |
| Always-escalate rules | A request that touches access grants, budgets or spend, data deletion, secrets or the company's own document goes to people. For a command, that is the [always-escalate list](../integrations/ssh.md#what-a-delegate-never-allows); for a merge or a deploy, the [paths no delegate allows](#paths-no-delegate-allows). |
| A merge's conditions | Read from the code host when the verdict is given. The head is the one asked about, the change is open and not a draft, every check its target requires and every one its rule [requires of that branch](#checks-a-rule-requires) passed on that head, reported by something other than one of the company's own agents (a target with neither goes to people), it merges cleanly, its target's own gate is otherwise met, and somebody other than its authors and the requesting agent approved that head. Otherwise it goes to people. |
| A deploy's conditions | Every change it names has merged into one branch, the commit holds each and is on that branch, and every check the branch requires and every one its rule requires of that branch passed on the commit, a branch with neither going to people (on Gitlet: the commit is at or behind the branch's newest green commit). Otherwise it goes to people. |
| Hourly caps | At most 30 allows per kind and delegate, and 10 commands per server, in one clock hour (UTC), counted across the fleet. Past either, requests go to people. A count Crewlet cannot take also goes to people. |
| A fact it cannot read | A code host that does not answer, a change touching more paths than Crewlet reads, or a check it may not read: the request goes to people. GitHub refusing branch protection or rulesets because the repository's plan does not include them is not one: it is read as the branch requiring nothing of its own, and the rule's own checks decide. |
| Time | A request no delegate answered in 15 minutes goes to people. |
| Turning it off | `enabled: false`, removing a rule, or changing its delegates shows its waiting requests to people on the next read, and records why within a minute. |

Every verdict, and every escalation Crewlet makes itself, is recorded in the
access log **before** it acts. A verdict whose record did not land carries
nothing out; the request reaches people at its deadline.

## What you still see

- **Escalations**, in the Inbox and on Home, with each delegate's verdict, confidence, reason and findings above the request, and why Crewlet put it to you.
- **Decided under delegation**: every answer a delegate gave is marked with its rule wherever the answer is shown: the run's page, the turn, the event and the live event log.
- **The digest**: what delegates decided for you on the last whole company day, on Home.
- **Today, so far**: **Settings › Delegated decisions** opens on the company day still running, with every decision made on it until now (asked again every minute), and says the day is still running. Its pager goes back to yesterday and earlier days, and forward no further than today.
- **Spot-checks**: one decision in `sample_one_in`, shown after the fact, with nothing to answer unless it was wrong.
- **The audit**: `GET /setup/integrations/ssh/audit?kind=verdict` for every verdict on a command, and `GET /delegated-decisions` for the rules, the digest (`day=today` for the day still running) and the spot-checks.

## Asking to merge and to deploy

An agent asks with a tool that holds the call on its run, as a command on a
server is held:

- `ask_to_merge {change, head, method, reason}`: `change` is a Gitlet change
  (`web!42`) or a GitHub pull request's address on the company's GitHub, and
  `head` the commit the agent read. An approval, a delegate's or a person's,
  merges exactly that head, once, as the agent: on Gitlet through the target's
  own merge gate, which still decides it, and on GitHub through GitHub's merge
  with that head's SHA, as the agent's own app (it needs Contents write). A
  change pushed to since is not merged, and the agent asks again.
- `ask_to_deploy {repo, commit, changes, environment, rollback, reason}`: an
  approval tells the agent it may deploy exactly that commit to that
  environment, once. Crewlet has no deploy path of its own: the agent deploys
  with its own tools, and the approval names what it may deploy.

Without a rule for the kind, the request goes to people. A person who may
approve one is an administrator, a lead of the agent, or a person the kind's
rule names in `escalate_to`, signed in again within the step-up window
(`release.approve`).

### Paths no delegate allows

A merge or a deploy whose change touches one of these goes to people, beside
the rule's own `always_escalate_paths`:

| Class | Paths |
|---|---|
| `ci_pipelines` | `.github/workflows/**`, `.github/actions/**`, `.gitlab-ci.yml`, `.gitlab/ci/**`, `.circleci/**`, `.buildkite/**`, `**/Jenkinsfile`, `azure-pipelines.yml`, `.crewlet/**` |
| `code_owners` | `CODEOWNERS` at the root, in `.github/`, `.gitlab/` or `docs/` |
| `secret_files` | `**/.env`, `**/.env.*`, `**/*.pem`, `**/*.key`, `**/*.p12`, `**/*.pfx`, `**/*.jks`, `**/*.keystore`, `**/id_rsa`, `**/id_ed25519`, `**/.npmrc`, `**/.pypirc`, `**/secrets/**` |
| `access_policies` | `**/iam/**`, `**/rbac/**`, `**/*.rego` |
| `infrastructure` | `**/*.tf`, `**/*.tfvars`, `**/terraform/**`, `**/pulumi/**`, `**/Pulumi.yaml`, `**/Pulumi.*.yaml` |
| `migrations` | `**/migrations/**`, `**/migrate/**`, `**/*.sql` |
| `crewlet_company` | `**/company.yaml`, `**/crewlet.yaml` |

These are the conventional places only. A repository can keep a secret or a
migration anywhere, which is what `always_escalate_paths` is for, and a
delegate is told to escalate what it cannot judge.

## Approve once at the decision

An agent can ask before it calls: `ask_to_operate {item, ask, host, command,
stdin, reason}` puts one exact command to a person as a decision on the work
item. Crewlet composes it, quoting the server, the command and the standard
input's SHA-256, and stamps the ask with its act. Choosing **Run it once** is
an SSH approval: only an administrator or a person `integrations.ssh.approvers`
or an `ssh_operate` rule's `escalate_to` names may choose it, signed in again
within the step-up window. In the turn that answer starts, the agent's
`ssh_operate` call that matches the act byte for byte runs without a second
prompt, once; any difference is asked again, and the audit records the
command as approved by that person through that decision.

## Checks a rule requires

A merge or a deploy is allowed under delegation only where something shows CI
passed on the commit decided. On GitHub that is the checks the branch requires
in its protection or rulesets. A private repository on a plan without branch
protection has neither (GitHub answers "Upgrade to GitHub Pro or make this
repository public"), so without more every merge and deploy there goes to
people. `required_checks` names the checks for such a branch, and Crewlet
enforces them whatever the delegate says:

- **The union.** The checks a merge or deploy needs are what GitHub requires
  of the branch, where it can, and what the rule names for that repository and
  branch. A branch with neither still goes to people.
- **A check run that succeeded on exactly that commit.** A named check passes
  only as a check run that completed with `success` on the head asked about
  (for a deploy, the commit), the newest of each app that reported it, and
  only from an app that is not one of the company's own agents. Missing,
  pending, failed, cancelled, timed out, neutral, skipped, or a run on another
  commit goes to people. A GitHub-required check still passes on neutral or
  skipped, as GitHub counts it; a name both require must pass both ways.
- **No commit status.** A named check is never met by a commit status, and no
  status is read for it: agent apps are not offered GitHub's Commit statuses
  permission, so in a private repository Crewlet cannot read one. Name the
  check run your CI reports (a GitHub Actions job's name, such as
  `test (ubuntu-latest)`).
- **Approval is unchanged.** Somebody other than the change's authors and the
  requesting agent must have approved that head in a GitHub review, which
  every plan has.
- **Read as the agent's own app.** The agent needs Checks (Read-only) to read
  the runs; without it the check reads as unread and the request goes to
  people.

The repository is matched ignoring case and the branch and the check names
exactly. An entry for a repository outside the company's GitHub (its
organization, or a seat's `integrations.github.repos`) is never read, and
`crewlet validate` says so as an advisory. Gitlet changes are held to their
target's own merge gate, which names its checks itself, so `required_checks`
applies to GitHub repositories. **Settings › Delegated decisions** lists each
rule's required checks beside its paths.

## What is not supported yet

- Merges on GitLab: GitLab records no approval of one commit, so nothing could
  bind an allow to the head shown. An agent asks for one with `ask_and_wait`,
  and a person decides.
- A GitHub check that external CI reports only as a commit status, in a
  private repository: agent apps are not offered GitHub's Commit statuses
  permission, so a check the branch requires reads as unread, a check a rule
  requires is never met by one, and the merge or deploy goes to people.
- Editing rules from the Settings view, which reads them; change them in the
  company document.
