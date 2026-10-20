# Dashboard prototypes

Interactive design prototypes of the dashboard. A prototype is the real
dashboard (its screens, its stylesheets and `@crewlethq/ui`) running on an
in-memory demo company with no engine behind it, built once per design
variant with specific modules swapped for a proposal. Every variant of a topic
is a folder of static files a host can serve, and the topic's front page opens
the dashboard as it ships.

Nothing here ships with the engine: `go build` never reads this folder, the
dashboard's own build and suites do not include it, and its checks run only
when asked.

## Layout

```
prototype/
  README.md            this file
  tsconfig.json        typechecks prototype/** against the dashboard's settings
  build.mjs            node prototype/build.mjs --topic <id> --out <dir> [--variant <id> ...]
  check.mjs            node prototype/check.mjs --topic <id> [--variant <id> ...]
  harness/
    defineTopic.ts     the topic API
    boot.tsx           bootPrototype(topic, variantId): src/main.tsx's boot on the demo client
    demoClient.ts      makeDemoClient(company): the Store and a DemoSocket
    restShim.ts        window.fetch for every REST path the dashboard calls
    VariantBar.tsx     the floating control (variants, scenario, reset)
    vite.mjs           the build: the shipped Vite config plus the swaps and the uilet plugin
    engine/            the demo engine: queries, acts, /config, /setup, /secrets
  fixtures/
    fairsplit.ts       the base demo company
    scenarios.ts       scenario helpers a topic composes
    engine/            answers captured from the engine itself (see engine/README.md)
  check/               the jsdom checks check.mjs runs
  topics/<id>/         one folder per topic: topic.ts, its variants and their files
```

## Starting a topic

1. Make a worktree from this branch and install the dashboard's packages:

   ```sh
   git -C <engine checkout> worktree add -b proto/<topic> <dir> proto/harness
   cd <dir>/dashboard && npm ci
   ```

2. Write `prototype/topics/<topic>/topic.ts` (below), with each variant's
   replacement modules and stylesheets beside it, for example under
   `topics/<topic>/variants/<variant>/`.

3. Check it, then build it:

   ```sh
   npx tsc -p prototype/tsconfig.json --noEmit
   npx prettier --check prototype
   node prototype/check.mjs --topic <topic>
   node prototype/build.mjs --topic <topic> --out <dir>
   ```

4. Publish `<dir>` as one artifact: `index.html` is the page, and every other
   file is published at its own path beside it.

Never edit `dashboard/src/**`, `dashboard/vite.config.ts` or
`dashboard/package.json` for a prototype: a variant swaps a module instead. A
new visual primitive belongs in uilet, built against with `uilet:` (below).

## The topic API

```ts
import { defineTopic } from "../../harness/defineTopic.ts";
import { compose, resumeAll, scenario, seatWorking } from "../../fixtures/scenarios.ts";

export default defineTopic({
  id: "agents", // the folder name under topics/
  title: "Agents", // two to four words: the launcher page's <title>
  start: "#/agents", // where every variant opens
  scenarios: [
    scenario("today", "As today", (c) => c), // the first is the default
    scenario("busy", "Both agents working", compose(resumeAll, seatWorking("pm", "FAIR-1"))),
  ],
  variants: [
    { id: "today", label: "Today", summary: "The dashboard as it ships." },
    {
      id: "a",
      label: "A · One list",
      summary: "One line saying what this variant proposes.",
      modules: { "src/routes/agents/People.tsx": "./variants/a/People.tsx" },
      css: ["./variants/a/a.css"],
      uilet: "/abs/path/to/a/uilet/worktree",
    },
  ],
});
```

- `defineTopic` validates the declaration and throws, naming every problem:
  slugs for ids, two to four words of title, `start` beginning `#/`, at least
  one scenario, a first variant `today` that swaps nothing, and labels of at
  most 32 characters (`LABEL_MAX`): each is drawn as one control on the bar.
  About a dozen characters ("A · One list") keeps six variants on one row.
- `scenarios[].apply` is a pure function from the base company (`fairsplit()`)
  to the company to show. It must not change the company it is given.
- `modules` maps a dashboard module (its path under `dashboard/`, or `~/…` for
  one under `src/`) to its replacement (relative to the topic's folder). Every
  importer of the original gets the replacement: the swap is matched on the
  resolved absolute path, so it does not matter how a screen spells the
  import. A build fails when a swap is never used, since that variant would
  otherwise look exactly like today.
- A replacement reaches the module it replaces through `@original/<path under
src>` (typed, `import { People as Shipped } from "@original/routes/agents/People.tsx"`)
  or the `?original` suffix on any specifier. Both bypass every swap, so a
  proposal can wrap the shipped screen rather than copy it. A replacement that
  imports the original by its ordinary name is refused.
- A replacement must export what the original exports, or the bundler stops
  with the missing name.
- `css` stylesheets load after every stylesheet the app ships. Draw with the
  design system's tokens (`var(--spacing-4)`, `var(--color-…)`), never colour
  literals.
- `uilet` builds the variant against a local uilet checkout instead of the
  installed `@crewlethq/*` packages: every JS import, every component
  stylesheet and the design-system sheet the shipped config assembles from
  `node_modules`. The path is absolute or relative to the topic's folder. The
  checkout must have run `npm ci --ignore-scripts` and `npm run build`, as its
  README says; the build refuses a checkout without its workspace links or its
  `dist`. React stays the dashboard's own copy. The published bundle never
  names the path: the topic's literal is replaced and every emitted file is
  scanned for local directories before it is written.
- `static` copies folders into the build as they are, shared by every
  variant: the key is the folder in the build, the value its source relative
  to the topic's folder. It is for what the engine serves from another
  origin, such as the pages an artifact frame opens (the artifacts topic
  copies its pages, the engine's page runtime and the artifact kit).
- The typecheck reads the installed `@crewlethq/ui`, so a replacement that
  imports something only the local uilet build exports declares it beside
  the topic, in a `.d.ts` the typecheck picks up:

  ```ts
  // topics/<topic>/uilet.d.ts
  import "@crewlethq/ui";
  import type { ReactElement } from "react";

  declare module "@crewlethq/ui" {
    export function StatusRing(props: { state: string }): ReactElement;
  }
  ```

  The bundle takes the real export from the local build; the declaration
  only tells the typecheck it is there.

## The base company

`fixtures/fairsplit.ts` is FairSplit, the company the design screenshots were
taken of, with invented values throughout and every time relative to now:

- Miles, a human seat at the root (`@miles`). He manages Crewlet-PM and
  answers decisions in the inbox. The browser is signed in with his Tier A
  token `review` (granted admin), bound to his seat in the identity directory
  and exchanged for a one-hour session eight minutes ago: `/auth/session`
  answers login `token:review` and no name of its own, the `viewer` question
  names the seat (`miles`, Miles, human) with admin, tracker and knowledge,
  and what he writes is recorded as `miles`, a human, through
  `session:<lineage>`. Nobody has enrolled as a person yet.
- Product and Engineering, a team led by Crewlet-PM (`@pm`, the crewlet
  character in rose) with Crewlet-SWE (`@engineer`, the gemlet in cyan)
  reporting to it, on the FAIR project and space.
- Both agents paused by Miles about 25 hours ago ("weekly limit."); the PM's
  last turn failed. The PM's week: 70 turns, 80% approved first pass, a 4.2 s
  median.
- One schedule, `daily-standup` on the PM (weekdays at 09:30), whose one
  recorded fire, three days back, was skipped as missed: the engine came back
  after its catchup window.
- Seven FAIR tasks, two open scope decisions to Miles on FAIR-1, and six
  unread updates in his inbox.
- Three knowledge-base pages, spend over the last three days, a 4M daily
  ceiling on each agent.
- One engine node with one alarm (no backup taken yet), serving on
  `https://fairsplit.example.com` (its Tier A `api.external_url`), and an
  integrations catalogue with nothing connected.

`fairsplit.document.ts` is its company document, which the real engine
accepts as it stands.

## Scenario helpers

From `fixtures/scenarios.ts`. Each returns a new company and composes with
`compose(...)`. A helper that writes the company document is held to the
rules the engine holds a write to (`engine/config.ts`), so a scenario the
engine would refuse throws, naming every rule it breaks:

| Helper                                                      | What it does                                                                                                                                                                                                                                                            |
| ----------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `scenario(id, label, apply)`                                | A scenario entry for `scenarios:`                                                                                                                                                                                                                                       |
| `compose(...steps)`                                         | Steps applied in order, as one                                                                                                                                                                                                                                          |
| `asToday`                                                   | The company unchanged                                                                                                                                                                                                                                                   |
| `resumeAll`                                                 | Every agent resumed and idle                                                                                                                                                                                                                                            |
| `pauseSeat(handle, reason?, minutesAgo?)`                   | One seat paused by the viewer                                                                                                                                                                                                                                           |
| `seatWorking(handle, itemKey?, secondsIn?)`                 | One seat on a turn now, its execute phase running a tool call                                                                                                                                                                                                           |
| `seatNeedsYou(handle, question, itemKey?)`                  | One seat's coding run parked on a question to the viewer (a decision)                                                                                                                                                                                                   |
| `seatOutOfBudget(handle)`                                   | One seat stopped on its spent daily budget                                                                                                                                                                                                                              |
| `seatStopped(handle, reason)`                               | One seat stopped for the engine's reason: `unplaced`, `provider`, `budget` or `paused`                                                                                                                                                                                  |
| `seatFailed(handle, message, kind?)`                        | One seat whose last turn ended in an error on its row                                                                                                                                                                                                                   |
| `inboxZero`                                                 | Every open question answered and every notice read                                                                                                                                                                                                                      |
| `addAgent({ name, handle, unit?, … })`                      | One more idle agent seat, in a unit or at the root, as a new revision                                                                                                                                                                                                   |
| `addTeam({ name, parent?, agents?, lead?, … })`             | One more unit with its agents                                                                                                                                                                                                                                           |
| `restyleAgent(handle, { character?, color? })`              | A seat drawn as another character or colour                                                                                                                                                                                                                             |
| `addPerson({ name, handle, unit?, … })`                     | One more person (a human seat), in a unit or at the root, as a new revision; `manages` names seat handles or unit keys, and `contact` the chat identities they are reached on (none by default)                                                                         |
| `addSchedule(owner, spec)`                                  | One more schedule on `{ seat: handle }` or `{ unit: key }`, written as the document holds one (`name`, `cron`, `task`, and `timezone`, `target`, `enabled`, `timeout_seconds`, `catchup` where set), as a new revision                                                  |
| `scheduleRan(owner, name, outcome, hoursAgo)`               | The fire due at or before `hoursAgo` hours ago, recorded as the scheduler records it: `fired` (each seat it reaches, a trace each, and the event), `skipped_paused`, or `skipped_catchup` (no seat)                                                                     |
| `freshCompany`                                              | Every agent idle with no turn on record                                                                                                                                                                                                                                 |
| `withPublicAddress(url?)`                                   | The node's Tier A `api.external_url` set (`""` for none) and the node restarted on it: every form's address and Slack manifest follow                                                                                                                                   |
| `signedInAsToken(tokenId?, seat?, grants?)`                 | The browser on a session exchanged from Tier A token `tokenId` (default `review`) eight minutes ago, bound to human seat `seat` (default `miles`; `""` for none) and granted `grants` (default `["admin"]`): login `token:<id>`, no own name, kind `person` while bound |
| `signedInAsPerson({ name, login, seat?, grants? })`         | The browser signed in as a person two hours ago: their own directory row, name and dotted login, bound to human seat `seat` (default `miles`), with `grants` (default `["admin"]`) and a password, a second factor and the session on record                            |
| `signedInAsServiceAccount({ login, grants, seat?, name? })` | The browser presenting a service account's machine token (`pat:<id>`) on every request: login `class:id`, kind `machine` while unbound, no session lineage or deadline. The shipped dashboard holds no bearer, so this is a proposal's state                            |
| `connectIntegration(key, submissions)`                      | A tool connected through the connect form's own path, one submission per form section (`seat` for an agent's own)                                                                                                                                                       |
| `integrationStatus(surface, status)`                        | What the tool answers every pass on a declared surface with (`phase`, `actor`, `detail`, `findings`, `last_error`), until a person writes its inputs or disconnects it                                                                                                  |
| `seatApp("github", handle, step)`                           | One agent's own GitHub app at `create_app`, `install_app` or `installed`, its key sealed as the engine names it; GitHub's pass reports from these steps                                                                                                                 |
| `seatProvisioned(kind, handle, { email? })`                 | One agent's own account on `datadog`, `atlassian`, `gitlab` or `mattermost` as that tool's provisioning pass leaves it: a `${VAR}` named at its credential address and sealed by the reconcile loop, so its form row reads ready under the account the engine names     |
| `integrationTraffic(surface, { inbound?, … })`              | Deliveries filed in the event log (the room counts them) and the skipped and coalesced outcomes                                                                                                                                                                         |

`AvatarCharacter` and `AvatarColor` are the sets the engine accepts.

## The demo engine

The dashboard runs unmodified against two seams. The socket is a `DemoSocket`:
the dashboard's own `LiveSocket` with its transport replaced, so every frame
(the handshake snapshot, every push) reaches the store through
`LiveSocket.onMessage` as JSON text, and every query is answered by the engine
after 20 ms. REST is `window.fetch`, answered by the engine for every path the
dashboard calls and passed through for any other origin.

Every question the protocol declares is answered (the table is typed against
`QueryMap`, so a missing one is a compile error). What the real engine derives
is derived here from the same inputs: the org projection, the roster and the
hierarchy (a port of `config.Derive`, held to the engine's own golden files),
schedule rows and their next fire, every tracker answer, spend and budget
windows on the company clock, the integrations room and its connect forms.

Schedules follow the engine's `schedule` and `org` rules (`engine/derive.ts`,
`engine/schedules.ts`). A unit's schedule reaches each of its own agents
unless it targets its lead, and a lead who is a person runs nothing; a turn
is capped at three minutes unless the schedule says otherwise; and a row with
no next fire says why. A write is refused, in the engine's words, for a
malformed cron or zone, two schedules of one name, a unit schedule nobody can
run, or any schedule on a person; a person with no chat contact is advice,
never a refusal. The dispatch ledger holds what the scheduler writes: each fire
labelled with its minute on the schedule's own clock, one row per seat it
reached, in the ledger's order, and one schedule's history paged at fifty
with `truncated` saying when the page filled.

The integrations room and `/setup/integrations` are derived from the document
on every read, by ports of the engine's rules (`engine/setup.ts`,
`engine/integrations.ts`, `engine/rollup.ts`). Every form's definitions come
from a listing captured from the engine (`fixtures/engine`); what each field
holds, which agents each tool lists, each agent's own Slack and GitHub app,
and how far each has got are read from the document and the secret store, and
the derivation reproduces both captures field for field. A write anywhere
moves every form: a connect, a config write, a scenario. The reconcile loop
(`engine/loop.ts`) runs a pass for every surface whenever a revision is
activated, as the engine brings its tick forward, and when a person presses
Check now, but never on a clock. What a pass hears is what a scenario
scripted with `integrationStatus`, on every pass, until a person writes that
surface's inputs or disconnects it: the engine's next pass then asks the tool
afresh. Otherwise it hears what the demo derives. GitHub's pass reports from
the agents' own apps as the engine's does (`githubPass.Run`): one finding
naming every agent with no app, one per app nothing has installed or whose
key does not resolve, and ready once every agent's app is installed. Every
other surface's pass finds nothing, there being no tool behind the demo to
report a fault. Findings fold into the loop's record as the engine's
`Observe` folds them (`engine/findings.ts` ports `Classify` and `Promote`):
the worst finding speaks for the rest, and the attempt count starts again
when the wait moves from one party to another. An agent whose GitHub block
holds no app (`seatApp(…, "create_app")`) is named twice, in the finding for
agents without an app and on its own, because the engine's
`seatsWithoutApps` and `ReconcileSeatApps` both count it today; the demo
reproduces that until the engine is fixed. Each pass stamps the
public address on the surfaces the engine registers itself, while a Slack
app's address is recorded when its form is submitted, so moving the address
leaves Slack asking for action until each agent's app is updated, as the
engine's room does. A disconnect answers 202 as the engine does, and the
demo's loop finishes the teardown at once. The secret store holds names, never
values; the one value the demo keeps is what a provisioning pass recorded to
be shown, an Atlassian account's address (`recorded`), read only while the
store still holds its variable.

`work_items`, which every list and board of work reads, is a port of the
engine's query grammar (`engine/workquery.ts`): a parameter the engine does
not read is refused `bad_params` rather than ignored, and so is every value it
refuses; presets and saved views expand under the caller's own parameters;
finished work is in an answer only when `show_closed` or `closed_since` asks
for it; outside `subtasks=separate` a subtask rides along with its root;
dates resolve on the company clock (`today`, `sow`, `-7d`, `range:a..b`); and
a board's columns, `group_limit`, `totals`, `around` and the paging cursor
follow the engine's rules. The one thing the demo company holds no data for is
a custom field's values: a declared field's term is left out of the answer
with a console warning, and an undeclared one is refused as the engine
refuses it.

Every state-log answer (work, decisions, the inbox, pages) states the level it
was served at: the `read_level` the read asked for, or `stale`, the
dashboard's own default. The demo is always at the head of every log, so each
level is satisfied as asked. An inbox notice's place in the primary split is
decided at read time from the person's own split or the engine's default, as
the engine decides it.

The writes a person can make that are modelled, each changing every answer
that reads it and pushing what the engine pushes:

| Write                                                              | How                                                                                                                                                                                                                                                                                                       |
| ------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pause and resume a seat                                            | `pause_seat`, `resume_seat`; the `agents` push                                                                                                                                                                                                                                                            |
| Answer a decision, comment, ask                                    | `comment_on_work_item`                                                                                                                                                                                                                                                                                    |
| Change, create, place and prioritise work                          | `update_work_item`, `create_work_item`, `place_work_item`, `set_priorities`, `set_pins`, `save_work_view`                                                                                                                                                                                                 |
| Mark, snooze and read through the inbox, and set its primary split | `mark_inbox`, `primary_reasons` included                                                                                                                                                                                                                                                                  |
| Write, save and comment on pages                                   | `write_page`, `save_page`, `comment_on_page`                                                                                                                                                                                                                                                              |
| Org edits and every company document change                        | `GET`/`PATCH`/`PUT /config`, `/config/{kind}/{id}`, dry runs, revisions, references; `If-Match` and `If-None-Match` enforced; validated; a new epoch with the `org`, `seats`, `agents`, `schedules` and `health` pushes                                                                                   |
| Connect, change settings, rotate and disconnect                    | `POST /setup/integrations/{key}/inputs` (201, a reload when only sealed values changed), `DELETE /setup/integrations/{key}` (202, or 200 with `force`); the document, the secrets, the forms and the room move together                                                                                   |
| Run a setup pass, and read the passes run                          | `POST /setup/integrations/{key}/check` and `/provision`, `GET /setup/integrations/{key}/runs[/{id}]`                                                                                                                                                                                                      |
| Secrets                                                            | `GET /secrets`, `PUT` and `DELETE /secrets/{name}`                                                                                                                                                                                                                                                        |
| Artifacts                                                          | Every `artifact*` question, read under the reader's role; rename, trash and restore, pins, shares, version labels and restores, a sheet's writes and schema, and the rooms an open artifact joins (`engine/artifacts.ts`)                                                                                 |
| Native kinds                                                       | Starting a sheet, or a doc or a deck from the engine's starter (`POST /artifacts`); a person's save of a doc, a deck or a design's canvas (`POST …/uploads`, `PUT /artifacts/uploads/…`, `POST …/versions` as a patch, `412 version_moved` when another version landed first) (`engine/artifactKinds.ts`) |

Who the browser is comes from one place, the company's principal
(`engine/principal.ts`), resolved through its identity directory
(`engine/iam.ts`): `GET /auth/session`, `GET /auth/config` and `/health`, the
`viewer` question (its grants, its acts from the authority table's rules, its
seat and project) and how every write is attributed (`iam.ActorFor`: a person
bound to a seat is the seat, the credential beside it as `operator_id`). The
directory's reads are served (`GET /iam/people`, `/iam/people/{id}` and its
sessions, `/iam/credentials`, `/iam/invitations`, `/iam/check`,
`/iam/node-tokens`, `/iam/seats` and `/iam/audit`, whose trail the demo does
not keep). Signing in or out and every directory change are refused.

Every native kind is read from its version's files as the engine reads it
(`engine/artifactKinds.ts`), so a fixture, a starter and a person's save are
drawn by one derivation: a doc's tabs (`GET …/doc`), a deck's slides read into
the canonical tree (`GET …/deck`, each slide file parsed by the browser's own
HTML parser), a design's `canvas.json` (`GET …/source`), a dashboard's board
and each of its datasets read as the viewer (`GET …/dashboard`,
`GET …/sources/{dataset}`, `GET /artifacts/sources`), and every version's
outline, which a gallery card draws (`GET …/outline`). A dataset reads the
artifact's store, a file of the version, a calc over the others (`filter`,
`group`, `compute`, `join`, `sort`, `limit`, `select`), or the engine's
`tracker.items`, `tracker.workload`, `spend.seats` and `spend.daily`; any other
engine source answers `unanswered`. Not ported: the publish linter and the
slide format's closed subset, so a slide the engine would refuse is drawn here
as far as the dashboard's renderer goes. Every artifact answers a store, empty
where it never wrote a collection, as the engine answers one.

A page, and each artboard of a design, is served from the build: a topic ships
each page under `static`, and a ticket points the frame at it with the shared
origin's sandbox (a design's ticket names the board's path as its
`document`), so the design system's `SandboxedFrame` and the engine's page
runtime run their real handshake. A page in that frame has an opaque origin,
so it loads only classic scripts and draws in the system face: a static host
serves no module or font to it without CORS.

Not modelled, and refused in a sentence ending "is not modelled in this
prototype, so nothing was changed." with a console warning: `steer_turn`,
`answer_run`, `answer_knowledge`, `restore_work_item`, `write_project`, taking
a backup, removing a broker member, a retention gate, and creating an app at
GitHub or Slack (which leaves for the third-party site). An unknown REST path
is answered 404 `no_route`, and an unknown question `unknown_query`, each with
a warning. Nothing pretends to have worked.

PROPOSED, NOT ENGINE ROUTES, served only so a proposal can be clicked through.
Each is a change the engine would need, and nothing here is evidence it
exists:

| Route                                                                                 | What the demo answers                                                                                                                                                                                                  |
| ------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /artifacts/templates`, `POST /artifacts` with `template`                         | The starting points a create offers (each with its kind and its outline), and an artifact copied from one                                                                                                              |
| `GET`, `POST /artifacts/{id}/comments`, `POST …/{thread}/replies`, `PATCH …/{thread}` | An artifact's comment threads, anchored to a version and a place in it                                                                                                                                                 |
| `comments`, `changes` on a listing row and a detail                                   | Open threads, and versions since the reader last opened it                                                                                                                                                             |
| `POST /setup/integrations/{key}/verify`                                               | Checks one typed credential `{ field, value, seat? }` without storing it: `{ ok, detail }`. A whole `${NAME}` works when the secret store holds NAME; a literal is refused when it says "bad" or is under 8 characters |

The engine is reachable from the console as `window.crewletPrototype.client`
(`engine`, `store`, `socket`).

## What a page remembers

Switching variants keeps the address and the company as this tab left it
(session storage), so a change made in one variant is there in the next.
Choosing a scenario rebuilds the company from it. Reset starts over: it
forgets every change made in this tab; the scenario stays. Both drop the org
builder's unsaved draft too, since it is a draft of the company they replace.
A stored company is kept only by a build of the same demo engine: its format
is a digest of `harness/engine/` and `fixtures/`, which every variant of a
topic shares, so a republished topic whose company changed starts a tab over
rather than misreading it. Nothing is stored for the sign-in: the frame asks
`GET /auth/session` and dials the socket for the session it finds, as it does
against an engine, and the demo engine answers it with the scenario's
principal. Where a frame refuses storage, an in-memory stand-in is used and
nothing throws.

## The variant bar

Built from `@crewlethq/ui` only, at the bottom of the window. The first row
names the topic and holds the scenario menu, whose trigger names the scenario
on screen ("Scenario: As today"), the light and dark toggle (the dashboard's
own theme preference, so it holds across every variant and matches its
Appearance settings), Reset and Fold; the second is the current
variant's summary; the last is the variants, one chip each. The card is one
width whatever the summary says (the window less 32px, up to 720px) and the
variants are its last row, so a longer summary grows the card upwards and
every variant stays where it was: a reader clicking along the row lands where
they aimed. Six labels of about a dozen characters sit on one row from a
1024px window; at a phone's width they wrap, and the bar starts folded. It
folds to a pill naming the variant. `[` and `]` step through the variants
unless the dashboard took the press (the peek rail and the org chart use the
same keys).

## Build output

`<out>/index.html` is the launcher: it opens the variant this browser last
had open (every variant's boot records it, `harness/session.ts`), or
`today/index.html` the first time, on the topic's start address, and lists
every variant for a browser that does not follow the redirect. Each `<out>/<variant>/` holds `index.html`, one script and
one stylesheet under `assets/`, the tab icon and the third-party notices; the
faces live once in `<out>/fonts/` and every variant's stylesheet points there.
Every URL is relative. The one shipped module that writes out the address the
engine serves the dashboard at (`/static/dashboard/`, for the brand mark) is
rewritten to the variant's own folder, and so is a topic's replacement copied
from it; an emitted file that still names that address fails the build, since
no host but the engine serves it. The publisher also refuses an XML file that
uses DTD machinery, and the brand mark `@crewlethq/icons` ships opens with
SVG 1.0's document type, which changes nothing a browser draws: the build
removes that one line from every emitted SVG, fails on any file that still
declares a document type or an entity, and fails on an SVG that no longer
parses as one (`harness/publish.mjs`, which the checks hold). It also fails
once no file carries a document type, which means uilet has dropped it from
the mark and the removal is to be deleted. The build prints each variant's
files and bytes against the publishing limits (16 MB a file, 64 MB and 255
files a publish) and exits non-zero past any of them. A variant is about
2.5 MB in five files, so a six-variant topic is about 36 files and 15 MB.

## The checks

`node prototype/check.mjs --topic <id>` first typechecks `prototype/` with
unused declarations refused, which the dashboard's own settings do not do (its
source holds some a prototype may not touch, so only files under `prototype/`
are judged). Then it boots every variant in jsdom, as its page boots, and
walks every workspace and section the navigation offers plus one of each
object page (`check/routes.test.tsx`): a route fails on an error boundary, on
"no such screen", or on anything said to the console, a refused question
included. The `today` run also runs the harness's own checks:

| Check                   | What it holds                                                                                                                                                                               |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `derive.test.ts`        | The hierarchy against the engine's golden files, `internal/config/testdata/derived`                                                                                                         |
| `engine.test.ts`        | Every modelled write, what reads it back, and the refusals                                                                                                                                  |
| `workquery.test.ts`     | The work query grammar: refusals, presets, finished work, boards, totals, paging                                                                                                            |
| `schedules.test.ts`     | Schedule rows, the schedule and person rules a write is refused by, and every fire a scenario records, read back as the screens read them                                                   |
| `integrations.test.ts`  | The integrations room and its forms: the captures reproduced, the address, passes, GitHub's pass from the agents' apps, scripted answers and the writes that end them, traffic, the roll-up |
| `session.test.ts`       | What a tab remembers across a variant switch, Reset keeping the scenario, storage refused                                                                                                   |
| `bar.test.tsx`          | The variant bar: the scenario named on its trigger, each press, `[` `]`, folding, labels                                                                                                    |
| `boot.test.ts`          | `boot.tsx`'s stylesheets against `src/main.tsx`'s, in order                                                                                                                                 |
| `publish.test.ts`       | The shipped brand mark losing exactly its document type line and still parsing as an SVG, and every SVG the publisher would refuse being refused                                            |
| `fidelity.test.tsx`     | The base company on the shipped screens, against the design screenshots                                                                                                                     |
| `interactions.test.tsx` | Writes made through the shipped controls: Resume, a decision, a connect form, an org edit                                                                                                   |

`check/routes.test.tsx` names what the shipped dashboard says on a route by
itself (a production bug the check found) in `KNOWN`, with the module it is
in. A console line is attributed to the module whose component drew it (the
first frame of React's owner stack), and an entry excuses only its own
module's lines: a variant wrapping the shipped screen through `@original`
is excused as Today is, and a replacement that copies the bug is not. The
list is two-sided wherever the variant ships the module as it is: an entry
that stops occurring fails the check. A variant that swaps the module out is
never held to it.
