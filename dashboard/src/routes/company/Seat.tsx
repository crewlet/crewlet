/**
 * One seat: who it is, what it is doing, what it remembers, what it costs.
 *
 * Tabs are SECTIONS — they push a history entry, because the reader called
 * them — and the tab is in the URL so a colleague can be sent the exact view.
 */

import { useId, useMemo, useRef, type ReactNode } from "react";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import {
  QueryState,
  RECORD_MAX_HEIGHT,
  SeatChip,
  Section,
  StateBadge,
} from "~/components/common.tsx";
// THE TRACKER'S OWN ROW AND THE PERSON'S OWN BLOCKS, imported rather than
// redrawn. `components/work.tsx` states the rule this follows — one renderer,
// screens pick the density — and a per-seat list that drew its own columns is
// exactly how the board came to know a task could be blocked while the
// personal page did not.
import { Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { peekHref } from "~/app/frame/DetailRail.tsx";
import { Asks, Checklist, TaskBlock } from "~/routes/me/MyWork.tsx";
import { TurnCard } from "~/components/TurnCard.tsx";
import { useSettled } from "~/lib/settled.ts";
import {
  Avatar,
  BarList,
  Button,
  Callout,
  Card,
  CodeBlock,
  EmptyState,
  EmptyValue,
  InlineCode,
  Meter,
  Skeleton,
  StatCard,
  StatGroup,
  Tabs,
  Tag,
  tabId,
} from "@crewlethq/ui";
import {
  ArrowForwardGlyph,
  Book2Glyph,
  BoltGlyph,
  CalendarTodayGlyph,
  ChatGlyph,
  CheckGlyph,
  DatabaseGlyph,
  ErrorGlyph,
  FlagGlyph,
  GroupGlyph,
  HelpGlyph,
  InboxGlyph,
  KeyGlyph,
  LayersGlyph,
  LinkGlyph,
  MemoryGlyph,
  NeurologyGlyph,
  PersonGlyph,
  ScheduleGlyph,
  TargetGlyph,
  TimelineGlyph,
  TokenGlyph,
} from "@crewlethq/icons/glyphs";
// OURS, AND THERE IS NO PEER. `phaseColor` picks one of `--color-phase-*`,
// which uilet publishes as tokens without a function that chooses between
// them; `Charts` exports only `dataColor` over the neutral data ramp. See the
// report.
import { phaseColor } from "~/ui/charts.tsx";
// ONE PHASE PILL for this screen and the Model screen alike — it is uilet's
import { PhaseTag } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import {
  useAgents,
  useOrg,
  usePhaseEvents,
  useSandboxes,
  useSchedules,
  useTokens,
} from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  awaitingPerson,
  indexOrg,
  llmChain,
  mcpEnvOf,
  reportsCaption,
  seatPath,
  seatReading,
  seatSettings,
  statusLine,
  afkReason,
  runState,
  type OrgIndex,
  type Seat,
  type SeatReading,
  type SeatSettings,
} from "~/lib/seats.ts";
import { configValueKind, fmtCount, fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { spanWords } from "~/lib/range.ts";
import {
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type {
  AgentRow,
  CompanyDocument,
  ConfigRole,
  ConversationEntry,
  CounterpartyProfile,
  EventRecord,
} from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { PropertiesRail, type Property } from "~/app/frame/PropertiesRail.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import {
  DateCell,
  DurationCell,
  KeyCell,
  NumberCell,
  TextCell,
  TokenCell,
} from "~/app/frame/cells.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { usePageLabels } from "~/app/Shell.tsx";

// THE KIND DECIDES THE SET, and the set decides what a `tab=` may resolve to.
// Turns, Conversations, Memory and Cost are properties of a RUNTIME and a
// human seat has none — it is addressable and never spawned — so the two
// lists are declared here and handed to `useTab`, which is what makes a
// `tab=` naming the other kind's tab land on Overview instead of on a strip
// with nothing selected and nothing below it.
//
// WORK IS ON BOTH, and it is the one tab that is. A human teammate has tasks
// assigned to them, questions waiting on them and checklist items on other
// people's tasks — that is the whole of what this product asks a person to
// do — so a seat page without it made the human half of the roster a
// directory entry with a contact card. It is what takes the human set to the
// three the design calls for.
//
// `turns` WAS `model`, and the rename is the panel catching up with what it
// draws: the phase history and the transcript are a record of TURNS, and
// "Model activity" was the old flat screen's name for a list that has since
// become `#/activity/turns`. A `?tab=model` bookmark resolves to Overview
// through `useTab`'s own fallback rather than to a blank strip, and no tag
// has ever shipped the parameter.
const AGENT_TABS = [
  "overview",
  "work",
  "turns",
  "threads",
  "memory",
  "cost",
  "access",
  "schedules",
] as const;
const HUMAN_TABS = ["overview", "work", "access"] as const;
type Tab = (typeof AGENT_TABS)[number];

const seatTurnKey = (g: { turnId: string }) => g.turnId;

/**
 * The operator-gated half of a seat, said precisely when it cannot be shown.
 *
 * `QueryState` covers a refused or failed read, which includes the guarded
 * banner with its Set token button. The three states after it are this
 * screen's own: no configuration is active, the document has no seat by this
 * name (the projection and the document can disagree for a moment either side
 * of an apply), and a name held by two seats in a revision stored before names
 * had to be unique.
 *
 * NONE OF THEM IS AN EMPTY VALUE. "Not set" over a field nobody was allowed to
 * read is a statement about the company, and it is the wrong one.
 */
function SettingsState({
  error,
  loading,
  doc,
  settings,
  seat,
  children,
}: {
  error: string | null;
  loading: boolean;
  doc: CompanyDocument | null;
  settings: SeatSettings | null;
  seat: Seat;
  children: ReactNode;
}) {
  if (loading && !doc && !error) {
    return <Skeleton variant="text" rows={3} label="Loading the company document" />;
  }
  if (error) return <QueryState error={error} loading={loading} />;
  if (!doc) {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph size={32} />}
        title="No company configuration is active"
        description="This seat's settings live in the company document, and none is active on this engine."
      />
    );
  }
  if (settings?.state === "missing") {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph size={32} />}
        title={`The active configuration has no seat named ${seat.name}`}
        description="The org chart and the configuration can disagree for a moment while a new revision is applied."
      />
    );
  }
  if (settings?.state === "ambiguous") {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph size={32} />}
        title={`More than one seat is named ${seat.name}`}
        description="This revision was stored before seat names had to be unique, so its settings cannot be attributed to one of them. Rename one of the seats to fix it."
      />
    );
  }
  return <>{children}</>;
}

/**
 * A value from the redacted document, in the form it may be shown.
 *
 * NEVER A CREDENTIAL: a literal in a credential field arrives as the mask and
 * says only that something is set, and a whole `${VAR}` names an entry in the
 * secret store. `secret` marks a credential field, where anything that is not
 * one whole reference is hidden here too, WHATEVER THE ENGINE SENT — see
 * [configValueKind], which records the redaction bug that makes that
 * necessary. A plain literal is shown only in a field that is not a
 * credential, such as a contact identity.
 */
function ConfigValue({ value, secret = false }: { value: string | undefined; secret?: boolean }) {
  switch (configValueKind(value, { secret })) {
    case "hidden":
      return <span className="t-caption">A literal value is set (hidden)</span>;
    case "reference":
      return <InlineCode>{value}</InlineCode>;
    case "literal":
      return <InlineCode>{value}</InlineCode>;
    default:
      return <span className="muted">not set</span>;
  }
}

/** A provider chain, in the order the fallback walks it. */
function ModelChain({ keys }: { keys: string[] }) {
  return (
    <span className="row gap-1" style={{ flexWrap: "wrap" }}>
      {keys.map((key, i) => (
        <span key={key} className="row gap-1">
          {i > 0 && (
            <span className="faint" title="falls back to">
              →
            </span>
          )}
          <code className="inline">{key}</code>
        </span>
      ))}
    </span>
  );
}

/**
 * The seat a handle names, resolved the ONE way.
 *
 * Three lookups rather than one, because a handle reaches this screen spelled
 * three ways: a link built from the roster carries the handle, a link built
 * from a config field carries the ROLE NAME, and a pasted URL carries whatever
 * somebody typed. Written here rather than at each caller so the page and the
 * peek can never disagree about which seat a `peek=` token names — the rail
 * and the page its `Open ↗` leads to must be the same seat.
 */
function findSeat(index: OrgIndex, handle: string): Seat | null {
  return (
    index.byHandle.get(handle) ??
    index.byName.get(handle) ??
    [...index.byHandle.values()].find((s) => s.handle.toLowerCase() === handle.toLowerCase()) ??
    null
  );
}

/** The live row for a seat, matched every way the roster and the overlay agree. */
function liveRow(agents: AgentRow[], handle: string, seat: Seat | null): AgentRow | undefined {
  return agents.find((a) => a.handle === handle || a.id === handle || a.role === seat?.name);
}

/**
 * The facts a seat wears, in the one order.
 *
 * ONE BUILDER FOR THE PAGE AND THE PEEK. The header exists so a reader scans
 * the same facts in the same order wherever the object appears, and two lists
 * written separately drift on the first field somebody adds to one of them.
 *
 * RUNTIME IS NOT A NODE ID, and the label says the smaller true thing rather
 * than the larger convenient one. `runtime_id` is the agent INSTANCE the live
 * projection minted, and that projection belongs to the node this dashboard is
 * attached to — so an empty one means "no instance here", never "this seat is
 * placed nowhere". Which node HOLDS the seat's lease is a fleet fact and lives
 * on `#/admin/fleet`; asking the fleet in order to label one seat would make
 * opening a peek a company-wide read.
 *
 * A HUMAN SEAT'S RUNTIME AND MODEL ARE EMPTY ON PURPOSE. `FactLine` drops a
 * fact whose value is empty, so the two rows that describe a runtime are
 * absent for a seat that has none — rather than present and dashed, which
 * would claim the engine failed to record something it will never record.
 */
function seatFacts({
  seat,
  agent,
  reading,
  hierarchy,
  human,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  /** What this reader can say about the seat's guarded half. See [seatReading]. */
  reading: SeatReading;
  /** Whether the engine reported its derived hierarchy at all. */
  hierarchy: boolean;
  human: boolean;
}): Fact[] {
  const unit = seat.unit;
  const manager = seat.manager;
  return [
    { label: "Kind", value: human ? "human teammate" : "agent seat" },
    {
      label: "Unit",
      value: seat.unitChain.length > 0 ? seat.unitChain.map((u) => u.name).join(" › ") : "org-wide",
      path: unit ? ["company", "units", unit.name] : undefined,
    },
    {
      // NOBODY AND NOT REPORTED ARE DIFFERENT FACTS. An engine that sends no
      // derived hierarchy has not said who manages this seat, and "nobody"
      // there is a claim nothing on the wire supports.
      label: "Reports to",
      value: manager ? manager.name : hierarchy ? "nobody" : "not reported by this engine",
      path: manager ? seatPath(manager) : undefined,
    },
    {
      label: "Runtime",
      value: human ? (
        ""
      ) : agent?.runtime_id ? (
        <code className="inline">{agent.runtime_id}</code>
      ) : (
        "not running on this node"
      ),
    },
    {
      // THE FLATTENED CHAIN, in the order the fallback walks it, from the
      // GUARDED document: `llm:` is not on the anonymous org projection, so a
      // reader without a token is told the chain is unreadable rather than
      // shown the default provider as though that were the setting.
      //
      // THE OUTCOME IS RESOLVED BEFORE IT REACHES HERE, by [seatReading],
      // because a refusal, an absence and an unasked question are three
      // different facts and a nullable role can only carry one of them. The
      // words are [modelFact]'s.
      label: "Model",
      value: human ? "" : modelFact(reading),
    },
  ];
}

/**
 * The MODEL fact's words, one per outcome of the guarded read.
 *
 * `unread` IS THE EMPTY STRING, so [FactLine] drops the fact entirely — the
 * same rule the Runtime row above follows for a human seat. A screen that has
 * not asked has nothing to report about the chain, and every sentence it could
 * print instead is a claim nothing on the wire supports: "unknown" says the
 * engine failed to answer, an em dash says the field is empty, and "needs an
 * operator token" says the reader is missing a credential they may be holding.
 * That last one is what this used to print, on five of the eight tabs and in
 * every peek.
 */
function modelFact(reading: SeatReading): string {
  switch (reading.state) {
    case "read": {
      // AN EMPTY ARRAY IS TRUTHY, which is why this is a length test.
      const chain = llmChain(reading.role.llm);
      return chain.length > 0 ? chain.join(" → ") : "default provider";
    }
    case "absent":
      return "not in the active revision";
    case "refused":
      return "needs an operator token";
    case "unread":
      return "";
  }
}

/**
 * Why the configured cap is not a number, as a clause completing "…could not be
 * read: ".
 *
 * ONE SENTENCE PER OUTCOME, shared by the tile's note and the meter's callout so
 * the two can never disagree about one seat. Both printed "needs an operator
 * token" for every outcome that was not a value — a claim about the READER,
 * wrong for a document still in flight and wrong for a revision whose roles do
 * not name this seat.
 */
function capNote(reading: SeatReading): string {
  switch (reading.state) {
    case "read":
      return "";
    case "absent":
      return "the active revision has no single seat by this name";
    case "refused":
      return "it needs an operator token";
    case "unread":
      return "it has not been read yet";
  }
}

/**
 * What the company document configures for this seat — and ONLY what can apply
 * to it.
 *
 * THE MODEL ROWS ARE AN AGENT'S. `org.Role.humanForbidden` refuses `llm` and
 * every per-phase chain on a human seat and `Organization.Validate` runs it over
 * every role, so on a human seat those two rows can only ever draw their own
 * fallbacks: "default provider" is a MODEL for a seat that runs none, and "none,
 * reflection uses the default" is a reflection pass that never happens. Both
 * read as a setting somebody chose, on the one card whose whole job is to say
 * what was chosen.
 *
 * So they are DROPPED rather than drawn empty — the first of [PropertiesRail]'s
 * three absences, and the same call [seatFacts], the tab strip and the peek
 * already make: a row that cannot have content is not an empty state, it is a
 * claim that the reader is missing something.
 *
 * EMAIL IS ON BOTH KINDS, and survives the same refusal: a human seat's address
 * is indexed so work addressed to it resolves to the person.
 */
function configuredProperties(role: ConfigRole | null, human: boolean): Property[] {
  const email: Property = { label: "Email", value: <ConfigValue value={role?.email} /> };
  if (human) return [email];
  // A CHAIN, DRAWN AS ONE. `llm:` accepts a key, a list or a per-phase mapping,
  // so this is the flattened order the provider chain actually walks — and an
  // empty ARRAY is truthy, which is why the fallback is an explicit length test
  // rather than `||`.
  const chain = llmChain(role?.llm);
  const auxiliary = llmChain(role?.llm_auxiliary);
  return [
    email,
    {
      label: "Model",
      value: chain.length ? (
        <ModelChain keys={chain} />
      ) : (
        <span className="muted">default provider</span>
      ),
    },
    {
      label: "Auxiliary model",
      value: auxiliary.length ? (
        <ModelChain keys={auxiliary} />
      ) : (
        <span className="muted">none, reflection uses the default</span>
      ),
    },
  ];
}

export function SeatScreen({ handle }: { handle: string }) {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const tokens = useTokens();
  const now = useNow();

  // WHICH THREAD IS OPEN, as a filter rather than a section: opening one
  // replaces the history entry, so Back leaves the seat rather than walking
  // every thread the reader glanced at.
  const [thread, setThread] = useParam("conversation", "", "filter");

  const phaseEvents = usePhaseEvents();

  const index = useMemo(() => indexOrg(org), [org]);
  // A HANDLE IS NOT A NAME. The tracker's rows carry handles and a reader
  // knows people by name, so every row renderer takes the resolution rather
  // than each one doing its own lookup.
  const chrome: RowChrome = useMemo(
    () => ({ seatName: (h: string) => index.byHandle.get(h)?.name ?? h }),
    [index],
  );
  const seat = findSeat(index, handle);
  // THE TRAIL NAMES THE SEAT, not the slug the URL addresses it by. A handle is
  // DERIVED from the name — `agent-cto` for "Chief Technology Officer" — so with
  // nothing published the page bar read "Company / People / agent-cto" over a
  // header titled with the name, and `Shell` titles the browser tab from the
  // same trail, so a reader with four tabs open had four slugs. Everywhere else
  // a seat is drawn by NAME with the handle beside it as the identifier:
  // `SeatChip`, the People rows, `seatLookup`, and this screen's own
  // `ObjectHeader`.
  //
  // NOT THE RULE `WorkItem` FOLLOWS, and the difference is what a person types.
  // `ENG-42` is an address somebody reads out of chat and pastes, so that crumb
  // spends itself on the key and leaves the title to the header. A handle is a
  // routing slug the engine mints; nobody quotes one, so this crumb spends
  // itself on the name.
  //
  // KEYED ON THE RAW SEGMENT rather than on `seat.handle`: `seatPath` addresses
  // a seat the engine reported no handle for BY NAME, and `findSeat` also
  // resolves a role name and a mis-cased handle, so the key has to be the string
  // the URL actually carries or the lookup in `crumbsFor` misses.
  //
  // AND NOTHING AT ALL FOR A SEAT WITH NO NAME. `labels` falls back to the
  // segment, which is an identifier a reader can still act on; publishing ""
  // would put a blank crumb in the bar and title the tab " · Crewlet".
  usePageLabels(seat?.name ? { [handle]: seat.name } : {});

  const human = seat?.kind === "human";
  // AFTER THE SEAT RESOLVES, because the tab set is a property of the seat's
  // kind. The list changes between renders and the hook does not, so the
  // resolution follows the seat rather than a cast made before it was known.
  const [tab, setTab] = useTab<Tab>("tab", human ? HUMAN_TABS : AGENT_TABS);
  // The pair of ids the strip and its panel are wired together by — minted
  // here rather than taken from a caller, for the reason ours minted them: a
  // caller asked to supply both is a caller who wires one end and forgets the
  // other, which reads exactly like a complete widget and is not one.
  const panelId = useId();

  const agent = liveRow(agents, handle, seat);
  const sandbox = sandboxes.find((s) => s.role === seat?.name) ?? null;
  // The ROLE NAME, which is what a phase record carries — the URL and every
  // link into this screen carry the handle. Empty for a handle that resolves to
  // nothing, and the stream filter below reads it as "match no phase" rather
  // than as "match every phase that named no role".
  const role = agent?.role ?? seat?.name ?? "";

  // The seat's own phase history. Its `live` half is deliberately NOT read:
  // the projection already pushes it onto the roster, and reading it here too
  // would give one screen two sources for one fact.
  // NOT FOR A HUMAN SEAT. The Turns tab is not in its set and the overview tile
  // this answer fed is gone with it, so the only thing left to ask for is a
  // phase history the engine can never have written.
  const history = useQuery(
    "agent",
    { id: handle },
    { enabled: !human && (tab === "overview" || tab === "turns") },
  );
  const memory = useQuery("agent_memory", { id: handle }, { enabled: tab === "memory" });
  // WHAT THIS SEAT HAS SAID ON A SURFACE THE ENGINE DOES NOT OWN. The ledger
  // is what stops it replying twice in one chat thread, and it has been
  // written since the runtime landed with nothing on any screen reading it.
  //
  // SCOPED, so a seat's threads are readable by that seat's own person and by
  // an operator — the same rule every other per-seat question follows. The
  // handle is sent explicitly because this screen is about somebody else's
  // seat as often as the reader's own.
  const threads = useQuery(
    "conversations",
    { handle, ...(thread ? { conversation: thread } : {}) },
    { enabled: tab === "threads" },
  );
  // A PERSON RECORD IS A HUMAN'S. A seat has a MAILBOX — the durable
  // subscription the engine attaches when it acquires the seat — and nothing
  // on a person's record describes one, so this is read only for a human.
  // AND ONLY WHERE THE READER MAY HAVE IT. A person record is somebody's
  // unread notices, the order they mean to work in and who set it — the
  // engine scopes it to the seat the caller's own credential is bound to, and
  // a colleague reading it needs an operator one. Asking anyway would put a
  // refusal on the screen where the honest answer is that this is theirs.
  const viewer = useViewer();
  const mayReadPerson = viewer.operator || (viewer.handle !== "" && viewer.handle === handle);
  const person = useQuery(
    "work_person",
    { handle },
    { enabled: tab === "overview" && seat?.kind === "human" && mayReadPerson, pollMs: 60_000 },
  );
  // WHAT IS ON THIS SEAT'S PLATE. The ungated `work_items` list is the floor
  // every reader gets; the scoped blocks below are the extra a person reading
  // their own seat, or an operator, is entitled to.
  const items = useQuery(
    "work_items",
    {
      assignee: handle,
      status_group: "not_started,active",
      // EVERY ROW FILTERED ON ITS OWN. The grammar's default is `collapsed`,
      // where the filter is a predicate on the ROOT and its whole subtree
      // rides along UNFILTERED — right for the board, which draws a tree, and
      // wrong for a card headed "Assigned and open" whose empty state reads
      // "Nothing open is assigned to them". Measured against a running
      // engine: @agent-cto's card said 9 and listed six tasks assigned to
      // Backend Engineer, which are the subtasks of an epic the CTO owns.
      subtasks: "separate",
      sort: "-priority,updated",
      limit: 50,
    },
    { enabled: tab === "work", pollMs: 30_000 },
  );
  // THE SEVEN CLAIMS, and only where the reader may have them. `work_my_work`
  // is scoped by the engine to the seat the caller's own credential is bound
  // to — the same rule `work_person` above follows — so asking for somebody
  // else's without an operator token puts a refusal where the honest answer
  // is that this is their queue. The guard is the one already derived for the
  // person record rather than a second copy of the rule.
  const mine = useQuery(
    "work_my_work",
    { handle },
    { enabled: tab === "work" && mayReadPerson, pollMs: 30_000 },
  );
  // THE TURN LIST ABOVE THE TRANSCRIPT. `turns` is keyed on the ROLE NAME
  // rather than the handle — a phase record carries the role, which is why
  // `role` is derived above — and a seat whose handle resolves to no role
  // would otherwise ask for every turn in the company.
  const turnList = useQuery(
    "turns",
    { role, limit: 50 },
    // TWENTY SECONDS, the cadence `routes/activity/Turns.tsx` already gives the
    // same question — a store aggregate with no push behind it. Asked once at
    // mount, this table froze its iterations, tokens and running flag at
    // whatever the turn looked like when the tab opened, beside cards that keep
    // ticking; and the turn a reader opened the tab to watch was never in it.
    { enabled: tab === "turns" && role !== "", pollMs: 20_000 },
  );
  const spend = useQuery(
    "tokens",
    { agent_role: seat?.name ?? "", since_days: 7, recent_turns: 50 },
    { enabled: tab === "cost" && !!seat },
  );
  // THE GUARDED HALF. A seat's email, model chain, token budget, contact
  // identities, tool credentials, integrations and schedules are NOT on the
  // anonymous org projection — `internal/api/orgprojection.go` spells out what
  // is, field by field, and everything else stays behind the operator token —
  // so this screen reads them from the company document.
  // ...AND ON EVERY TAB, because the HEADER reads the model chain out of this
  // same answer and renders above the strip on all eight of them. `enabled` is
  // the guard for a question whose PARAMETER is not chosen yet; gating it on
  // which panel is open made the object describe itself by what was below it,
  // and the five tabs that did not ask rendered the absence as "needs an
  // operator token" to a reader already holding one.
  //
  // Leaving the gate and having the header say nothing on those five tabs is
  // the smaller change and it is the wrong one: a fact that appears on Overview
  // and vanishes on Work is still a header that moves when the panel does. A
  // header is a property of the OBJECT.
  //
  // It costs no extra asking either — it saves it. Toggling `enabled` re-runs
  // the effect and clears the answer, so Overview → Work → Overview used to
  // fetch the whole document twice.
  const config = useQuery("config", undefined, { enabled: !!seat });
  // NOTHING FROM THE DOCUMENT BESIDE A REFUSAL. `useQuery` keeps its last good
  // answer through a failed ask, which suits a poll and is wrong for a guarded
  // read: once a token is cleared or refused, the email, model, budget and
  // schedules it had been allowed to read stayed on the overview and the cost
  // tab, beside a banner saying the answer needs a token.
  const settings = useMemo<SeatSettings | null>(
    () => (seat && config.data && !config.error ? seatSettings(config.data, seat) : null),
    [seat, config.data, config.error],
  );
  // WHAT THIS READER CAN SAY ABOUT THE GUARDED HALF, as a named outcome rather
  // than a nullable role: [seatReading] carries the four this screen has to tell
  // apart, and `configured` is the one of them that holds a document.
  const reading = useMemo(() => seatReading(settings, config.error), [settings, config.error]);
  const configured = reading.state === "read" ? reading.role : null;
  const credentials = useMemo(
    () => (settings && seat ? mcpEnvOf(settings, seat.kind) : {}),
    [settings, seat],
  );
  /** The seat's configured cap. 0 or absent is unlimited; no document at all is unknown. */
  const budget = configured?.token_budget ?? 0;
  // THIS SEAT'S RECURRING WORK, FROM THE RESOLVED ROWS.
  //
  // It was `schedulesOf(settings)`, which reads the `schedules:` a seat
  // AUTHORED out of the company document. Three things were wrong with that,
  // and the third is the one a reader would never guess:
  //
  //  - The document is an operator read, so a reader without a token saw no
  //    recurring work at all rather than a seat that has none.
  //  - A `ScheduleSpec` carries name, cron and task. The engine's own answer
  //    carries the EFFECTIVE timezone, the `next_run` it worked out, and
  //    `problem` when a cron or a zone cannot be read — so a schedule that
  //    can never fire looked exactly like one that fires tomorrow.
  //  - It only ever found schedules this seat DECLARED. A unit schedule
  //    reaches every seat in the unit, and `runners` is the engine's resolved
  //    answer to whose day it lands in — so the rows that actually wake this
  //    seat were the ones it could not see.
  //
  // The rows are already here: the handshake and every config apply push them
  // onto the store, which is what `useSchedules` reads.
  const pushed = useSchedules();
  const schedules = useMemo(
    () =>
      pushed.filter(
        (row) =>
          (row.scope_type === "role" && row.scope_id === handle) || row.runners.includes(handle),
      ),
    [pushed, handle],
  );

  const phases = useMemo<PhaseRecord[]>(() => {
    const stored = (history.data?.llm_history ?? [])
      .map((ev) => fromPhaseEvent(ev as EventRecord))
      .filter((r): r is PhaseRecord => r !== null);
    // The query above is answered ONCE, at mount. Every phase that finishes
    // after it — which is every phase of the turn a reader opened this tab to
    // watch — reaches the tab only here.
    const streamed = streamedPhases(phaseEvents, (r) => role !== "" && r.role === role);
    const live = agent?.live_call ? [fromLiveCall(agent.live_call, agent.role)] : [];
    // Streamed FIRST so the query's own copy of the same phase wins the key:
    // both are the same durable record, and preferring the one that came
    // through the paged, authoritative answer keeps one source in charge.
    return mergePhases([...streamed, ...stored], live);
  }, [history.data, phaseEvents, agent, role]);

  const turns = useMemo(() => groupTurns(phases), [phases]);
  // THE ENGINE'S OWN ROW FOR EACH CARD. One turn, one set of figures: the card
  // reads its start and its duration off the same record the table above draws,
  // and falls back to its phases only where there is no row. They disagreed
  // three ways — "started 6m ago" beside "running for 3m 15s", a length shorter
  // than a phase inside it, and one word over two quantities.
  const turnRows = useMemo(
    () => new Map((turnList.data?.turns ?? []).map((t) => [t.turn_id, t])),
    [turnList.data],
  );
  const liveTurns = useMemo(() => turns.filter((g) => g.live), [turns]);
  const doneTurns = useMemo(() => turns.filter((g) => !g.live), [turns]);
  const liveTurnKeys = useMemo(() => liveTurns.map(seatTurnKey), [liveTurns]);
  // The turns this reader has watched RUN.
  //
  // A turn card is REMOUNTED when it crosses from the live region into the
  // settled list — two different lists, so React builds a new component — and
  // its latched open state goes with the old one. `i === 0 && !liveTurns.length`
  // then closes it whenever the seat has already started another turn, which
  // collapsed the transcript the reader had open at the exact moment its last
  // phase landed. Accumulated in a ref, and written during render for the same
  // reason `useSettled` does it: adding to a set is idempotent, so a render
  // React discards and repeats leaves the same set behind.
  const watched = useRef<Set<string>>(new Set());
  for (const key of liveTurnKeys) watched.current.add(key);
  // A turn the reader has been watching run is not a NEW row when it finishes:
  // it moves out of the live list into this one, and holding it behind "1 new
  // turn finished while you were reading" is the same disappearance from the
  // reader's side.
  const settled = useSettled(doneTurns, seatTurnKey, liveTurnKeys);

  if (!seat) {
    return (
      <>
        <EmptyState
          icon={<PersonGlyph size={32} />}
          title={`No seat called “${handle}”`}
          description="Seats are addressed by handle. If a company revision was just applied, this seat may have been renamed or removed."
          action={
            <Button variant="primary" onClick={() => nav.to(["company", "people"])}>
              All seats
            </Button>
          }
        />
      </>
    );
  }

  const manager = seat.manager;
  const reports = seat.reports;
  const state = runState(agent, sandboxes);
  const seatSpend = tokens?.by_agent?.find((a) => a.role === seat.name);

  return (
    <>
      <PageActions>
        {
          <Button
            leadingIcon={<TimelineGlyph size="xs" />}
            size="small"
            variant="secondary"
            onClick={() => nav.to(["activity"], { actor: seat.name })}
          >
            Its events
          </Button>
        }
      </PageActions>

      {/* THE HANDLE, THE STATE AND THE UNIT ARE THE HEADER'S NOW. They were
          three badges in the page bar, which is where a screen's CONTROLS
          live — so the seat's identity was rendered in the one strip that is
          not about the object, and the peek would have had to spell it a
          second way. Nothing is lost: the handle is the identifier, the state
          is the status, and the unit is the fact it always was. */}
      <ObjectHeader
        kind="Seat"
        icon={human ? "person" : "memory"}
        identifier={seat.handle ? `@${seat.handle}` : undefined}
        title={seat.name}
        status={
          human ? (
            <Tag appearance="outline">human seat</Tag>
          ) : (
            <StateBadge agent={agent} sandboxes={sandboxes} />
          )
        }
        facts={seatFacts({ seat, agent, reading, hierarchy: index.hierarchy, human })}
      />
      <PageNote>{seat.goal || statusLine(agent, { sandbox, seat })}</PageNote>

      {agent?.last_error && (
        <Callout
          variant="danger"
          action={
            agent.last_error.event_id ? (
              <a className="t-link" href={href(["activity", "events", agent.last_error.event_id])}>
                event →
              </a>
            ) : undefined
          }
        >
          <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
          {agent.last_error.phase && ` (during ${agent.last_error.phase})`}
          {agent.last_error.at && ` · ${relTime(agent.last_error.at, now)}`}
        </Callout>
      )}
      {state === "afk" && (
        <Callout variant="warning">This seat is AFK: {afkReason(agent?.afk_reason)}.</Callout>
      )}
      {sandbox && awaitingPerson(sandbox.status) && (
        <Callout
          variant="warning"
          icon={<HelpGlyph size="md" />}
          action={
            <Button
              size="small"
              variant="secondary"
              onClick={() => nav.to(["activity", "runs", sandbox.turn_id])}
            >
              The run
            </Button>
          }
        >
          A coding run is paused on a question: {sandbox.question || "(no question recorded)"}
        </Callout>
      )}

      {/* THEIR STRIP, OUR PANEL. `Tabs` is a real port: its roving focus is
          MANUAL — `useRoving(items.length, null)` passes no selector, so the
          arrows move focus and commit nothing, which is the contract ours was
          written for and the one this screen needs, since every tab pushes a
          history entry and three of them fire a query.

          What it does not have is the PANEL. `TabPanel` takes `id`, `value`,
          `children` and `className` and spreads nothing else, so it cannot be
          given the `tabIndex={0}` that puts the content in the tab order — and
          without that a reader who selects a tab and presses Tab leaves the
          widget entirely, landing past everything they just chose. So the
          panel stays ours, wired to their strip through their own `tabId`.
          See the report. */}
      <Tabs
        ariaLabel="Seat sections"
        variant="underline"
        value={tab}
        onValueChange={(next) => setTab(next as Tab)}
        panelId={panelId}
        // THE KIND DECIDES THE SET. Model activity, Memory and Cost are
        // properties of a RUNTIME, and a human seat has none: it is
        // addressable and never spawned. All five were rendered
        // unconditionally, so a human teammate's Cost tab read "TOKENS · 7D —
        // 0 · INPUT / OUTPUT — 0 / 0 · CONFIGURED BUDGET — unlimited", which
        // is three measurements of a thing that cannot be measured, and their
        // Memory tab offered a diary nothing will ever write.
        //
        // Not disabled — absent. A tab that cannot have content is not an
        // empty state, it is a claim that the reader is missing something.
        items={
          human
            ? [
                { value: "overview", label: "Overview", icon: <PersonGlyph size="sm" /> },
                { value: "work", label: "Work", icon: <LayersGlyph size="sm" /> },
                { value: "access", label: "Access", icon: <KeyGlyph size="sm" /> },
              ]
            : [
                { value: "overview", label: "Overview", icon: <PersonGlyph size="sm" /> },
                { value: "work", label: "Work", icon: <LayersGlyph size="sm" /> },
                { value: "turns", label: "Turns", icon: <NeurologyGlyph size="sm" /> },
                { value: "threads", label: "Conversations", icon: <ChatGlyph size="sm" /> },
                { value: "memory", label: "Memory", icon: <DatabaseGlyph size="sm" /> },
                { value: "cost", label: "Cost", icon: <TokenGlyph size="sm" /> },
                { value: "access", label: "Access", icon: <KeyGlyph size="sm" /> },
                {
                  value: "schedules",
                  label: "Schedules",
                  icon: <CalendarTodayGlyph size="sm" />,
                },
              ]
        }
      />
      <div
        className="tabpanel"
        role="tabpanel"
        id={panelId}
        aria-labelledby={tabId(panelId, tab)}
        tabIndex={0}
      >
        {/* WITHHELD, and said so. A panel that simply is not there reads as
            a person with nothing on their plate, which is the one thing it
            must not read as. */}
        {tab === "overview" && human && !mayReadPerson && (
          <Card>
            <Card.Header icon={<CheckGlyph size="sm" />}>
              <Card.Title>Their day</Card.Title>
            </Card.Header>
            <p className="t-body">
              Their inbox, their queue and their pinned views are theirs. Reading another person's
              record needs an operator credential.
            </p>
          </Card>
        )}

        {tab === "overview" && human && person.data?.held && (
          <Card>
            <Card.Header
              icon={<CheckGlyph size="sm" />}
              subtitle="Read-only here: an inbox is moved on by the person whose it is, through their own assistant."
            >
              <Card.Title>Their day</Card.Title>
            </Card.Header>
            <StatGroup columns={3}>
              <StatCard
                icon={<InboxGlyph size="xs" />}
                label="Unread"
                value={person.data.unread?.length ?? 0}
                sub={
                  person.data.due?.length
                    ? `${person.data.due.length} snoozed and now due`
                    : "nothing snoozed is due"
                }
              />
              <StatCard
                icon={<LayersGlyph size="xs" />}
                label="Queue"
                value={person.data.priorities?.length ?? 0}
                // WHO CHOSE IT is the one thing a queue cannot say for
                // itself. A lead may set what somebody in their line does
                // next, and a person who starts the day on work they did
                // not choose should be able to tell.
                // AND WHEN. A queue somebody else ordered three weeks ago
                // is a different fact from one they ordered this morning,
                // and the name alone cannot tell them apart — which is
                // what carrying the instant the whole way and rendering
                // nothing amounted to.
                sub={
                  person.data.priorities_set_by
                    ? `set by ${person.data.priorities_set_by}${
                        person.data.priorities_set_at
                          ? ` ${relTime(person.data.priorities_set_at, now)}`
                          : ""
                      }`
                    : "their own order"
                }
              />
              <StatCard
                icon={<FlagGlyph size="xs" />}
                label="Pinned views"
                value={person.data.pinned_views?.length ?? 0}
                sub={`${person.data.favorites?.length ?? 0} starred`}
              />
            </StatGroup>
          </Card>
        )}

        {tab === "overview" && (
          <>
            {/* The flush Panel is gone: StatGroup draws that surface itself.

                THE KIND DECIDES THE TILES, for the reason it decides the tab
                set above. Spend and turns are measurements of a RUNTIME and a
                human seat has none, so "Tokens — Nothing recorded" and "Turns in
                the record — 0" were two measurements of a thing that cannot be
                measured — and the second's caption points at a phase history
                this kind's overview does not have. The column count goes with
                them, because a fixed grid with two tiles missing is two holes
                rather than a shorter row. */}
            <StatGroup columns={human ? 2 : 4}>
              <StatCard
                icon={<BoltGlyph size="xs" />}
                label="State"
                value={human ? "human" : state}
                sub={statusLine(agent, { sandbox, seat })}
              />
              {!human && (
                <StatCard
                  icon={<TokenGlyph size="xs" />}
                  // THE WINDOW THE ROLLUP ITSELF REPORTS, never a second
                  // hardcoded one. This tile is fed by the PUSHED rollup —
                  // which is why Overview fires no query for it — and that
                  // covers `livestate.LiveSpendWindow`, currently a day. Under
                  // a literal "7d" it was a day's spend beneath a week's
                  // heading, disagreeing by a factor of several with the Cost
                  // tab's tile of the same name one click away. The engine
                  // states the window on the answer for exactly this reason,
                  // and refuses to relabel a rollup it did not take.
                  label={tokens ? `Tokens · ${spanWords(tokens.since, tokens.until)}` : "Tokens"}
                  value={
                    seatSpend ? (
                      fmtCount(seatSpend.total_tokens)
                    ) : (
                      <EmptyValue label="Nothing recorded" />
                    )
                  }
                  sub={
                    seatSpend
                      ? `${seatSpend.calls.toLocaleString()} model calls`
                      : "nothing recorded"
                  }
                />
              )}
              {!human && (
                <StatCard
                  icon={<LayersGlyph size="xs" />}
                  label="Turns in the record"
                  // Zero is a MEASUREMENT — this seat has taken no turns — and
                  // an em dash would claim nobody looked.
                  value={turns.length}
                  sub="the phase history loaded below"
                />
              )}
              <StatCard
                icon={<GroupGlyph size="xs" />}
                label="Direct reports"
                // THE CAPTION IS THIS NUMBER'S FOOTNOTE, so it breaks this
                // number down. It read "reports to <manager>" — who manages this
                // seat, under a count of who this seat manages — so one tile
                // carried two opposite relations, and the manager is a header
                // fact and a "Who this is" row already.
                //
                // AND A ZERO IS A MEASUREMENT. Without the engine's derived
                // block `reports` is empty because nothing was said, not because
                // nobody reports here; lib/seats.ts keeps the two apart and this
                // is where the difference gets drawn, exactly as the three rows
                // below do.
                value={
                  index.hierarchy ? (
                    reports.length
                  ) : (
                    <EmptyValue label="Not reported by this engine" />
                  )
                }
                sub={reportsCaption(seat, index.hierarchy)}
              />
            </StatGroup>

            <div className="grid grid-auto-lg">
              <Card>
                <Card.Header icon={<PersonGlyph size="sm" />}>
                  <Card.Title>Who this is</Card.Title>
                </Card.Header>
                <PropertiesRail
                  groups={[
                    {
                      properties: [
                        { label: "Role", value: seat.name },
                        {
                          label: "Handle",
                          // NOT DERIVED HERE. An engine that reports no
                          // hierarchy leaves the handle of a seat that
                          // declares none unknown, and the rule that keys a
                          // seat's memory is the engine's alone.
                          value: seat.handle ? (
                            <code className="inline">@{seat.handle}</code>
                          ) : (
                            <EmptyValue label="Not reported by this engine" />
                          ),
                        },
                        {
                          label: "Kind",
                          value: human
                            ? "human teammate — never spawned by the engine"
                            : "agent seat",
                        },
                        {
                          label: "Goal",
                          value: seat.goal || <span className="muted">not set</span>,
                        },
                        {
                          label: "Unit",
                          value: seat.unitChain.length ? (
                            seat.unitChain.map((u) => u.name).join(" › ")
                          ) : (
                            <span className="muted">org-wide</span>
                          ),
                        },
                        {
                          label: "Unit lead",
                          value: seat.unitLead ? (
                            <SeatChip
                              name={seat.unitLead}
                              handle={index.byName.get(seat.unitLead)?.handle}
                            />
                          ) : index.hierarchy ? (
                            <span className="muted">none</span>
                          ) : (
                            <EmptyValue label="Not reported by this engine" />
                          ),
                        },
                        {
                          label: "Reports to",
                          value: manager ? (
                            <SeatChip name={manager.name} handle={manager.handle} />
                          ) : index.hierarchy ? (
                            <span className="muted">nobody</span>
                          ) : (
                            <EmptyValue label="Not reported by this engine" />
                          ),
                        },
                      ],
                    },
                  ]}
                />
              </Card>

              {/* THE DOCUMENT'S HALF, BEHIND THE TOKEN. Email, the model chain
                  and the auxiliary chain are not on the anonymous projection,
                  so they are read from the company document and the panel says
                  so when it could not be read — rather than drawing "not set"
                  over a setting nobody was allowed to see. */}
              <Card>
                <Card.Header
                  icon={<NeurologyGlyph size="sm" />}
                  subtitle="from the company document"
                >
                  <Card.Title>Configured</Card.Title>
                </Card.Header>
                <SettingsState
                  error={config.error}
                  loading={config.loading}
                  doc={config.data ?? null}
                  settings={settings}
                  seat={seat}
                >
                  <div className="col gap-3">
                    <PropertiesRail
                      groups={[{ properties: configuredProperties(configured, human) }]}
                    />
                    {human && (
                      // WHY THE PANEL IS SHORT. An absent row must not read as
                      // one this token was not allowed to see — telling those
                      // two apart is the rest of this card's job — so the reason
                      // is written where the rows would have been.
                      <p className="t-caption">
                        A human seat runs no model: the engine never spawns one, so the document
                        refuses <code className="inline">llm</code> and every per-phase chain on it.
                        Their contact identities are on Access.
                      </p>
                    )}
                  </div>
                </SettingsState>
              </Card>

              <Card>
                <Card.Header icon={<Book2Glyph size="sm" />}>
                  <Card.Title>Profile</Card.Title>
                </Card.Header>
                <div className="col gap-3">
                  {seat.backstory && (
                    <div className="col gap-1">
                      <div className="t-label">Backstory</div>
                      <p className="t-body measure">{seat.backstory}</p>
                    </div>
                  )}
                  {seat.responsibilities.length > 0 && (
                    <div className="col gap-1">
                      <div className="t-label">Responsibilities</div>
                      <ul
                        className="col gap-1"
                        style={{ paddingLeft: "var(--space-4)", margin: 0 }}
                      >
                        {seat.responsibilities.map((r, i) => (
                          <li key={i} className="t-cell">
                            {r}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  {seat.guidelines.length > 0 && (
                    <div className="col gap-1">
                      <div className="t-label">Behavioural guidelines</div>
                      <ul
                        className="col gap-1"
                        style={{ paddingLeft: "var(--space-4)", margin: 0 }}
                      >
                        {seat.guidelines.map((r, i) => (
                          <li key={i} className="t-cell">
                            {r}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  {!seat.backstory && !seat.responsibilities.length && !seat.guidelines.length && (
                    <span className="t-caption">
                      No profile is set. Backstory, responsibilities and guidelines render straight
                      into this seat's executor prompt.
                    </span>
                  )}
                </div>
              </Card>
            </div>

            {reports.length > 0 && (
              <Section title="Direct reports" hint={`${reports.length}`}>
                <div className="seat-grid">
                  {reports.map((r) => {
                    const about = r.goal || r.unit?.name || "";
                    return (
                      // `seatPath` rather than a handle spelled out again: it is
                      // the one place that knows how a seat the engine reported
                      // no handle for is addressed, and the rail below already
                      // goes through it.
                      <a key={r.key} className="seat-card" href={href(seatPath(r))}>
                        <div className="row">
                          {/* Their `variant="dashed"` is our `human`: a human
                              seat is drawn rather than tinted, because the
                              engine does not run it. Their `md` is 32px where
                              ours was 26, so the step moves down one. */}
                          <Avatar
                            name={r.name}
                            size="sm"
                            variant={r.kind === "human" ? "dashed" : "solid"}
                            decorative
                            title={r.name}
                          />
                          <span className="truncate t-cell" style={{ flex: 1, minWidth: 0 }}>
                            {r.name}
                          </span>
                          {r.kind === "human" ? (
                            <Tag appearance="outline">human</Tag>
                          ) : (
                            <StateBadge
                              agent={agents.find((a) => a.role === r.name)}
                              sandboxes={sandboxes}
                            />
                          )}
                        </div>
                        {/* A GOAL IS PROSE, SO IT GETS THE CARD'S OWN WIDTH.
                            Between the avatar and the state badge it had about
                            160px of a 300px card and one line of it, so
                            `.truncate` — `white-space: nowrap`, a cut at
                            whichever PIXEL came next — ended it mid-word with
                            the card underneath empty. Below the row it has the
                            whole card and two lines. `title` carries the rest
                            for a goal longer than two lines; a screen reader
                            reads it in full either way, because a clamp is
                            visual and takes nothing out of the DOM. */}
                        {about && (
                          <span className="clamp t-caption" title={about}>
                            {about}
                          </span>
                        )}
                      </a>
                    );
                  })}
                </div>
              </Section>
            )}
          </>
        )}

        {/* RECURRING WORK, ON ITS OWN TAB. It was a card at the foot of the
            overview, under everything else a seat is — which is the wrong
            altitude for the one thing on this page that will wake the seat
            again without anybody asking. */}
        {tab === "schedules" && (
          <>
            {schedules.length === 0 ? (
              <EmptyState
                size="compact"
                icon={<CalendarTodayGlyph size={28} />}
                title="Nothing recurring reaches them"
                description="A schedule wakes a seat on a cron, scoped to a role or to a unit. This seat declares none and is named by none."
              />
            ) : (
              <Card padding="none">
                <Card.Header
                  icon={<CalendarTodayGlyph size="sm" />}
                  count={schedules.length}
                  subtitle="Their own, and every unit schedule a fire actually reaches them through."
                >
                  <Card.Title>Recurring work</Card.Title>
                </Card.Header>
                <DataGrid
                  rows={schedules}
                  rowKey={(row) => [row.scope_type, row.scope_id, row.name].join("/")}
                  rowHref={(row) =>
                    peekHref({
                      kind: "schedule",
                      id: [row.scope_type, row.scope_id, row.name].join("/"),
                    })
                  }
                  columns={[
                    {
                      key: "name",
                      header: "Name",
                      cell: (row) => <TextCell icon="calendar_today">{row.name}</TextCell>,
                      sortValue: (row) => row.name,
                    },
                    {
                      // NOT A KEY CELL. A cron expression is a five-field
                      // schedule rather than an identifier — nothing is
                      // addressed by it — so it keeps the code face it reads
                      // in everywhere else in the product.
                      key: "cron",
                      header: "Cron",
                      shrink: true,
                      cell: (row) => <code className="inline nowrap">{row.cron}</code>,
                    },
                    {
                      // WHOSE SCHEDULE IT IS, which is the column the authored
                      // read could not have: a unit schedule is not this
                      // seat's and still lands in its day.
                      key: "scope",
                      header: "Scope",
                      shrink: true,
                      cell: (row) =>
                        row.scope_type === "role" ? (
                          <span className="muted">theirs</span>
                        ) : (
                          <TextCell icon="apartment">{row.scope_id}</TextCell>
                        ),
                      sortValue: (row) => `${row.scope_type}/${row.scope_id}`,
                    },
                    {
                      // THE ENGINE'S OWN ANSWER, and the REASON where it has
                      // none. A disabled schedule and one whose timezone was
                      // renamed both have an empty `next_run`; only the second
                      // is a defect, and the row says which in `problem`.
                      key: "next",
                      header: "Next",
                      shrink: true,
                      sortValue: (row) => tsKey(row.next_run),
                      cell: (row) =>
                        row.problem ? (
                          <Tag variant="danger" title={row.problem}>
                            cannot fire
                          </Tag>
                        ) : !row.enabled ? (
                          <Tag appearance="outline">disabled</Tag>
                        ) : row.next_run ? (
                          <DateCell at={row.next_run} now={now} />
                        ) : (
                          <EmptyValue label="Not recorded" />
                        ),
                    },
                    {
                      key: "task",
                      header: "Task",
                      cell: (row) => <TextCell>{row.task}</TextCell>,
                    },
                  ]}
                />
              </Card>
            )}
          </>
        )}

        {/* WHAT IS ON THIS SEAT'S PLATE.
            TWO HALVES, AND THEY ARE NOT THE SAME READ. The list is
            `work_items {assignee}`, which is ungated — every reader of this
            page gets it, and it is the floor. The blocks under it are
            `work_my_work`, which the engine scopes to the caller's own seat,
            so they arrive for a person reading their own page and for an
            operator and for nobody else. Rendered as one tab because the
            question a reader has is "what is this seat doing", and the answer
            is simply fuller when they are entitled to more of it. */}
        {tab === "work" && (
          <div className="col gap-4">
            <Coverage answer={items.data} />
            {items.loading && !items.data && (
              <Skeleton variant="text" rows={4} rowHeight={44} label="Loading this seat's work" />
            )}
            <QueryState
              error={items.error}
              loading={items.loading}
              empty={
                items.data && !(items.data.items ?? []).length
                  ? {
                      title: "Nothing open is assigned to them",
                      hint: "Work reaches a seat by assignment, and closed work is not counted here. Their turns and the tracker's own activity say what they have been doing.",
                    }
                  : undefined
              }
            >
              <Card padding="none">
                <Card.Header count={(items.data?.items ?? []).length}>
                  <Card.Title>Assigned and open</Card.Title>
                </Card.Header>
                <RowList
                  rows={items.data?.items ?? []}
                  now={now}
                  chrome={chrome}
                  hrefOf={(row) => href(["work", row.key])}
                />
              </Card>
            </QueryState>

            {/* THE SCOPED HALF, and its absence is a sentence rather than a
                gap: a reader without the credential is not missing a feature,
                they are reading somebody else's queue. */}
            {mayReadPerson ? (
              <>
                <Asks rows={mine.data?.asked_of_me ?? []} now={now} chrome={chrome} />
                <TaskBlock
                  title="What they mean to do first"
                  hint="Their own order, as they set it."
                  rows={mine.data?.priorities ?? []}
                  now={now}
                  chrome={chrome}
                />
                <TaskBlock
                  title="Collaborating"
                  hint="Tasks they are named on without owning."
                  rows={mine.data?.collaborating ?? []}
                  now={now}
                  chrome={chrome}
                />
                <Checklist rows={mine.data?.checklist_items ?? []} />
              </>
            ) : (
              <p className="t-caption">
                Their own queue — what they mean to do first, the questions put to them and their
                checklist items on other seats&apos; tasks — is theirs to read. An operator
                credential, or their own, shows it here.
              </p>
            )}
          </div>
        )}

        {tab === "turns" && (
          <>
            {/* THE SETTLED RECORD, ABOVE THE TRANSCRIPT. The panel below is
                this seat's phase history merged with what the projection is
                pushing right now, which is the right shape for reading one
                turn and the wrong one for finding a turn: it holds what the
                event store answered for this seat and nothing older. The
                `turns` question is the paged, sortable record — keyed on the
                ROLE, which is what a phase record carries — so a reader
                looking for the turn that failed last Tuesday has a list to
                look in, and each row opens that turn in the rail. */}
            {(turnList.data?.turns ?? []).length > 0 && (
              <Card padding="none">
                <Card.Header
                  icon={<NeurologyGlyph size="sm" />}
                  count={(turnList.data?.turns ?? []).length}
                  subtitle="Every turn the event store holds for this seat, newest first."
                >
                  <Card.Title>Turns</Card.Title>
                </Card.Header>
                <DataGrid
                  rows={turnList.data?.turns ?? []}
                  rowKey={(t) => t.turn_id}
                  rowHref={(t) => peekHref({ kind: "turn", id: t.turn_id })}
                  columns={[
                    {
                      key: "started",
                      header: "Started",
                      shrink: true,
                      sortValue: (t) => tsKey(t.started_at),
                      cell: (t) => <DateCell at={t.started_at} now={now} />,
                    },
                    {
                      key: "summary",
                      header: "What it did",
                      sortValue: (t) => t.summary ?? "",
                      cell: (t) => (
                        <span className="row gap-1">
                          <span className="truncate">
                            {t.summary || <span className="muted">no summary recorded</span>}
                          </span>
                          {t.task_id && (
                            <span
                              className="mono t-caption"
                              title="the work item this turn was about"
                            >
                              {t.task_id}
                            </span>
                          )}
                        </span>
                      ),
                    },
                    {
                      key: "iterations",
                      // SELF-ITERATE ROUNDS, and the word says so. Headed "Rounds" this
                      // column sat directly above phase rows printing TOOL rounds under
                      // the same word — "Rounds 1" over a 3r execute and a 1r review.
                      header: (
                        <span title="self-iterate rounds — the tool rounds each phase used are on the phase row">
                          Iterations
                        </span>
                      ),
                      label: "Iterations",
                      shrink: true,
                      sortValue: (t) => t.iterations,
                      cell: (t) => <NumberCell value={t.iterations} />,
                    },
                    {
                      key: "tokens",
                      header: "Tokens",
                      shrink: true,
                      sortValue: (t) => t.total_tokens,
                      cell: (t) => <TokenCell value={t.total_tokens} />,
                    },
                    {
                      key: "state",
                      header: "",
                      label: "State",
                      shrink: true,
                      cell: (t) => (
                        <span className="row gap-1">
                          {/* A RUNNING TURN IS NOT A ZERO-LENGTH ONE.
                              `duration_ms` is 0 until a completion record
                              exists, and `complete` is what tells a turn in
                              flight from one that died mid-flight. */}
                          {!t.complete && (
                            <Tag variant="info" title="no completion record — running, or it died">
                              running
                            </Tag>
                          )}
                          {t.failed && <Tag variant="danger">failed</Tag>}
                        </span>
                      ),
                    },
                  ]}
                />
              </Card>
            )}
            {turnList.error && <QueryState error={turnList.error} loading={turnList.loading} />}
            {history.loading && !turns.length && (
              <Skeleton variant="text" rows={4} rowHeight={44} label="Loading this seat's turns" />
            )}
            {/* The QUERY'S OWN STATE, BESIDE THE TURNS RATHER THAN IN PLACE OF
              THEM. It used to wrap them, and `QueryState` renders NOTHING while
              a query is in flight and a banner INSTEAD of its children when one
              fails — so the turn happening right now was hidden until the event
              store answered, and hidden for good on a node that keeps no event
              log at all. Only the settled half of this screen comes from that
              query; the running half is pushed. */}
            {history.error && <QueryState error={history.error} loading={history.loading} />}
            {!history.loading && !history.error && !turns.length && (
              <EmptyState
                size="compact"
                icon={<NeurologyGlyph size={32} />}
                title="No phases in the record for this seat"
                description="A phase is recorded when it completes. A seat that has not taken a turn has nothing here."
              />
            )}
            {/* The same split the Model screen makes, for the same reason:
              a running turn changes every couple of hundred milliseconds,
              and letting that churn sit inside the settled history reflowed
              whatever the reader was working through. Here it also answers
              "which of these is happening right now", which used to be
              readable only off a badge. */}
            {liveTurns.length > 0 && (
              <section className="col gap-1 live-region">
                <div className="t-label">
                  Running now
                  <span className="muted"> · updates as each round is written</span>
                </div>
                <div className="col gap-2">
                  {liveTurns.map((g) => (
                    <TurnCard key={g.turnId} group={g} row={turnRows.get(g.turnId)} defaultOpen />
                  ))}
                </div>
              </section>
            )}
            {settled.pending > 0 && (
              <button className="new-rows" onClick={settled.flush}>
                {plural(settled.pending, "new turn")} finished while you were reading — show
              </button>
            )}
            <div className="col gap-2">
              {settled.items.map((g, i) => (
                <TurnCard
                  key={g.turnId}
                  group={g}
                  row={turnRows.get(g.turnId)}
                  defaultOpen={(i === 0 && !liveTurns.length) || watched.current.has(g.turnId)}
                />
              ))}
            </div>
            {turns.length > 0 && (
              <Card padding="tight">
                <div className="row">
                  <span className="t-caption">
                    Showing the most recent phases the engine holds for this seat.
                  </span>
                  <span className="spacer" />
                  <Button
                    size="small"
                    variant="secondary"
                    onClick={() =>
                      nav.to(["activity", "turns"], { view: "phases", role: seat.name })
                    }
                  >
                    All model activity for {seat.name}
                  </Button>
                </div>
              </Card>
            )}
          </>
        )}

        {tab === "threads" && (
          <>
            <PageNote>
              Every thread this seat holds a record in, and what it said there. The ledger is what
              stops it replying twice in one conversation — it is the engine&rsquo;s only account of
              what a seat said on a surface it does not own, and until now nothing read it.
            </PageNote>
            <QueryState error={threads.error} loading={threads.loading}>
              <div className="split">
                <Card padding="none">
                  <Card.Header
                    icon={<ChatGlyph size="sm" />}
                    count={threads.data?.conversations?.length ?? 0}
                  >
                    <Card.Title>Threads</Card.Title>
                  </Card.Header>
                  {threads.data?.conversations?.length ? (
                    <div className="list">
                      {threads.data.conversations.map((row) => (
                        <button
                          key={row.key}
                          type="button"
                          className={`thread-entry as-row${row.key === thread ? " selected" : ""}`}
                          onClick={() => setThread(row.key === thread ? "" : row.key)}
                        >
                          <span className="row gap-1">
                            <span className="mono truncate t-cell" style={{ flex: 1 }}>
                              {row.key}
                            </span>
                            <Tag appearance="outline">
                              {row.turns} {row.turns === 1 ? "turn" : "turns"}
                            </Tag>
                            <span className="t-caption">{fmtDateTime(row.last_at)}</span>
                          </span>
                        </button>
                      ))}
                    </div>
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<ChatGlyph size={32} />}
                      title="No conversations recorded"
                      description="A seat writes one entry per turn that took part in a thread — a chat message, an issue comment, a page discussion."
                    />
                  )}
                </Card>

                <Card padding="none">
                  <Card.Header
                    icon={<ScheduleGlyph size="sm" />}
                    // A COUNT IS A FACT ABOUT THE THREAD THE READER OPENED.
                    // `entries` is what this seat said in that ONE thread, and
                    // the answer carries `entries: []` whenever no
                    // `conversation` was asked for — `queries.conversations`
                    // fills it only when one is named — so with nothing open the
                    // old `?? 0` drew a chip reading 0 beside a title asking the
                    // reader to pick a thread: a quantity stated about a thread
                    // nobody had named, and one that could never have been
                    // anything else.
                    //
                    // Three states, not two: nothing open, open but no answer
                    // back yet, and open and answered — where a 0 IS the fact,
                    // because the ledger is trimmed per conversation and the
                    // empty state below says so. `undefined` is what
                    // `Card.Header` reads as "no count" (it draws every other
                    // value, 0 included), and dropping the `??` gives it for the
                    // first two: `threads.data` is null until the answer lands.
                    count={thread ? threads.data?.entries?.length : undefined}
                  >
                    {/* THE TITLE NAMES THE PANEL; IT DOES NOT INSTRUCT. The
                        instruction is the empty state's, one line below, and a
                        header carrying it too said the same thing twice — while
                        renaming the panel on every click, so the chip beside it
                        changed what it was counting with nothing to say so. A
                        stable noun, like "Threads" on the card beside it. */}
                    <Card.Title>Thread turns</Card.Title>
                  </Card.Header>
                  {!thread ? (
                    <EmptyState
                      size="compact"
                      icon={<ScheduleGlyph size={32} />}
                      title="Nothing selected"
                      description="Choose a thread to see the turns this seat recorded in it."
                    />
                  ) : threads.data?.entries?.length ? (
                    <div className="list">
                      {threads.data.entries.map((entry, i) => (
                        <ThreadTurn key={entry.turn_id || i} entry={entry} />
                      ))}
                    </div>
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<ScheduleGlyph size={32} />}
                      title="No turns in this thread"
                      description="The ledger is trimmed per conversation, so an old thread can list a count it no longer carries the turns for."
                    />
                  )}
                </Card>
              </div>
            </QueryState>
          </>
        )}

        {tab === "memory" && (
          <>
            {memory.loading && (
              <Skeleton variant="text" rows={5} label="Loading this seat's memory" />
            )}
            <QueryState error={memory.error} loading={memory.loading}>
              <div className="col gap-4">
                <Card padding="none">
                  <Card.Header
                    icon={<Book2Glyph size="sm" />}
                    count={memory.data?.diary?.length ?? 0}
                    subtitle="what this seat chose to remember"
                  >
                    <Card.Title>Private diary</Card.Title>
                  </Card.Header>
                  {memory.data?.diary?.length ? (
                    <div className="list">
                      {memory.data.diary.map((d, i) => (
                        <div key={d.id ?? i} className="thread-entry">
                          <div className="row gap-1">
                            <Tag appearance="outline">{d.retention || d.scope || "note"}</Tag>
                            <span className="spacer" />
                            <span className="t-caption">{fmtDateTime(d.created_at)}</span>
                          </div>
                          <p className="t-body">{d.content}</p>
                        </div>
                      ))}
                    </div>
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<Book2Glyph size={32} />}
                      title="Nothing written yet"
                      description="A seat writes here by calling reflect_and_persist during a turn."
                    />
                  )}
                </Card>

                <Card padding="none">
                  <Card.Header
                    icon={<LayersGlyph size="sm" />}
                    count={memory.data?.episodes?.length ?? 0}
                    subtitle="one row per completed turn, searched by similarity at turn start"
                  >
                    <Card.Title>Past turns</Card.Title>
                  </Card.Header>
                  <DataGrid
                    name="episodes"
                    rows={memory.data?.episodes ?? []}
                    rowKey={(e) => e.id ?? e.turn_id ?? e.created_at}
                    defaultSort="-at"
                    empty={{
                      title: "No episodes recorded",
                      hint: "An episode is written when a turn completes.",
                    }}
                    columns={[
                      {
                        key: "at",
                        header: "When",
                        shrink: true,
                        // THROUGH `tsKey`, never `<` on the string. The engine
                        // sends both encodings of an instant and trims
                        // trailing zeros, so a raw compare puts `:07Z` before
                        // `:07.42Z` — the later episode first, in a list read
                        // newest-first.
                        sortValue: (e) => tsKey(e.created_at),
                        cell: (e) => <DateCell at={e.created_at} now={now} />,
                      },
                      {
                        key: "task",
                        header: "What it did",
                        // `||` rather than `??`: an episode that recorded an
                        // EMPTY summary has none, and a dash that says so
                        // beats a blank cell nobody can tell from a fault.
                        cell: (e) =>
                          e.task_summary || e.content ? (
                            <TextCell>{e.task_summary || e.content}</TextCell>
                          ) : (
                            <EmptyValue label="The episode recorded no summary" />
                          ),
                      },
                      {
                        key: "outcome",
                        header: "Outcome",
                        shrink: true,
                        // NULL, not "": an outcome nothing recorded sorts
                        // last in both directions rather than ahead of every
                        // recorded one, which is what the grid does with an
                        // absent value and what the dash below claims.
                        sortValue: (e) => e.review_outcome ?? e.outcome ?? null,
                        cell: (e) =>
                          e.review_outcome || e.outcome ? (
                            <Tag
                              variant={
                                (e.review_outcome ?? e.outcome) === "done" ? "success" : "warning"
                              }
                            >
                              {e.review_outcome ?? e.outcome}
                            </Tag>
                          ) : (
                            <EmptyValue label="The turn ended without a review outcome" />
                          ),
                      },
                      {
                        key: "dur",
                        header: "Took",
                        align: "right",
                        shrink: true,
                        sortValue: (e) => e.duration_ms ?? null,
                        cell: (e) => <DurationCell ms={e.duration_ms} />,
                      },
                      {
                        key: "conv",
                        header: "Conversation",
                        cell: (e) =>
                          e.conversation_key ? (
                            <KeyCell value={e.conversation_key} />
                          ) : (
                            <EmptyValue label="Not part of a conversation" />
                          ),
                      },
                    ]}
                  />
                </Card>

                <Card padding="none">
                  <Card.Header
                    icon={<BoltGlyph size="sm" />}
                    // `?? 0` for the ANSWER, never for the field:
                    // `skills_total` is always sent, so falling back to
                    // `skills.length` would only ever substitute the page size
                    // for the total.
                    count={memory.data?.skills_total ?? 0}
                    subtitle="drafted from its own past work, loadable mid-turn"
                  >
                    <Card.Title>Skills it taught itself</Card.Title>
                  </Card.Header>
                  {memory.data?.skills?.length ? (
                    <div className="list">
                      {memory.data.skills.map((s, i) => (
                        <div key={s.id ?? s.key ?? i} className="thread-entry">
                          <div className="row gap-1">
                            <strong className="t-body">{s.title}</strong>
                            {s.version != null && <Tag appearance="outline">v{s.version}</Tag>}
                            <span className="spacer" />
                            {s.updated_at && (
                              <span className="t-caption">{fmtDateTime(s.updated_at)}</span>
                            )}
                          </div>
                          {s.summary && <p className="t-caption">{s.summary}</p>}
                        </div>
                      ))}
                      {memory.data.skills_total > memory.data.skills.length && (
                        // THE CUT, SAID. The count above is the seat's whole
                        // set and this list is a page of it, so without a line
                        // here the two silently disagree.
                        <div className="thread-entry t-caption">
                          {memory.data.skills.length} of {memory.data.skills_total} shown
                        </div>
                      )}
                    </div>
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<BoltGlyph size={32} />}
                      title="No synthesised skills"
                      description="The learning loop drafts these from repeated work. A young company has none."
                    />
                  )}
                </Card>

                <Card padding="none">
                  <Card.Header
                    icon={<GroupGlyph size="sm" />}
                    count={memory.data?.counterparties?.length ?? 0}
                  >
                    <Card.Title>Who it has worked with</Card.Title>
                  </Card.Header>
                  {memory.data?.counterparties?.length ? (
                    <div className="list">
                      {memory.data.counterparties.map((c, i) => (
                        <CounterpartyRow key={`${counterpartyKey(c)}-${i}`} profile={c} />
                      ))}
                    </div>
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<GroupGlyph size={32} />}
                      title="No counterparty profiles"
                      description="Built up from observed interactions. Nothing read these until now — the key was on the answer and the store behind it was never asked."
                    />
                  )}
                </Card>
              </div>
            </QueryState>
          </>
        )}

        {tab === "cost" && (
          <>
            {/* The flush Panel is gone: StatGroup draws that surface itself. */}
            <StatGroup columns={3}>
              <StatCard
                icon={<TokenGlyph size="xs" />}
                label="Tokens · 7d"
                value={
                  spend.data ? (
                    fmtCount(spend.data.totals.total_tokens)
                  ) : (
                    <EmptyValue label="Nothing recorded" />
                  )
                }
                sub={spend.data ? `${spend.data.totals.calls.toLocaleString()} model calls` : ""}
              />
              <StatCard
                icon={<ArrowForwardGlyph size="xs" />}
                label="Input / output"
                value={
                  spend.data ? (
                    `${fmtCount(spend.data.totals.input_tokens)} / ${fmtCount(spend.data.totals.output_tokens)}`
                  ) : (
                    <EmptyValue label="Nothing recorded" />
                  )
                }
                sub="input includes any cached prefix, as the provider reports it"
              />
              <StatCard
                icon={<TargetGlyph size="xs" />}
                label="Configured budget"
                // UNKNOWN IS NOT UNLIMITED, and WHY it is unknown is four
                // answers rather than one. `token_budget` is on the guarded
                // document, so a reader without a token is told the cap could
                // not be read rather than shown "unlimited" — a statement about
                // the company nothing on the wire supports. A read still in
                // flight and a document that does not name this seat are
                // absences too, and telling either reader to fetch a token they
                // may already hold is that same wrong statement aimed at them
                // instead.
                value={
                  reading.state === "read" ? (budget ? fmtCount(budget) : "unlimited") : "Unknown"
                }
                sub={
                  reading.state === "read"
                    ? budget
                      ? "token_budget on this role in the company config"
                      : "token_budget is 0 or unset on this role"
                    : `token_budget could not be read: ${capNote(reading)}`
                }
              />
            </StatGroup>

            {/* The live meter and the configured budget are DIFFERENT facts and
              the screen says so. The previous seat page printed "no budget is
              set" in one tab while another printed the budget from the same
              config, because one read a field the server never sent. */}
            {agent?.budget ? (
              <Card>
                <Card.Header
                  icon={<TargetGlyph size="sm" />}
                  subtitle="process-lifetime, not the 7-day window"
                >
                  <Card.Title>Live budget meter</Card.Title>
                </Card.Header>
                {/* THEIR `label` IS THE ACCESSIBLE NAME, tied to the bar, so
                    the separate `ariaLabel` ours needed is gone — and with it
                    the reason the visible word could not be the name. "Used"
                    was never a name for anything; the seat's budget is.
                    `fullMeans="spent"` is gone because that is the only
                    reading theirs has, and it is the right one here. */}
                <Meter
                  value={agent.budget.used}
                  max={agent.budget.max}
                  label={`${agent.role}'s token budget`}
                  valueText={`${fmtCount(agent.budget.used)} of ${fmtCount(agent.budget.max)} tokens`}
                  hint={`${fmtCount(agent.budget.used)} / ${fmtCount(agent.budget.max)}`}
                  tone={agent.budget.used >= agent.budget.max ? "danger" : undefined}
                />
                {agent.budget.used >= agent.budget.max && (
                  <p className="t-caption" style={{ marginTop: "var(--space-2)" }}>
                    This seat&rsquo;s meter is at its cap, so its turns are being declined at the
                    gate.
                  </p>
                )}
              </Card>
            ) : (
              <Callout variant="neutral">
                {reading.state === "read"
                  ? budget
                    ? "This role has a token_budget in the config, but no engine is currently reporting a meter for it, so there is nothing measured to draw."
                    : "No per-seat budget meter. This role has no token_budget, so its spend is bounded only by the company-wide one."
                  : `No engine is reporting a meter for this seat, and its configured cap could not be read: ${capNote(reading)}.`}
              </Callout>
            )}

            {spend.loading && (
              <Skeleton variant="text" rows={4} label="Loading this seat's spend" />
            )}
            <QueryState error={spend.error} loading={spend.loading}>
              <div className="grid grid-auto-lg">
                <Card>
                  <Card.Header icon={<LayersGlyph size="sm" />}>
                    <Card.Title>By phase</Card.Title>
                  </Card.Header>
                  <BarList
                    data={(spend.data?.by_phase ?? []).map((p) => ({
                      id: p.phase,
                      label: p.phase,
                      value: p.total_tokens,
                      display: fmtCount(p.total_tokens),
                      color: phaseColor(p.phase),
                      sub: `${p.calls} calls`,
                    }))}
                    emptyLabel="No calls in the window."
                  />
                </Card>
                <Card>
                  <Card.Header icon={<MemoryGlyph size="sm" />}>
                    <Card.Title>By model</Card.Title>
                  </Card.Header>
                  <BarList
                    data={(spend.data?.by_model ?? []).map((m) => ({
                      id: m.model,
                      label: m.model,
                      value: m.total_tokens,
                      display: fmtCount(m.total_tokens),
                      sub: `${m.calls} calls`,
                    }))}
                    emptyLabel="No calls in the window."
                  />
                </Card>
              </div>

              <Card padding="none">
                <Card.Header icon={<LayersGlyph size="sm" />}>
                  <Card.Title>Recent turns</Card.Title>
                </Card.Header>
                <DataGrid
                  name="turns"
                  rows={spend.data?.by_turn ?? []}
                  rowKey={(t) => t.turn_id}
                  defaultSort="-started"
                  onRowActivate={(t) => nav.to(["activity", "turns", t.turn_id])}
                  empty={{ title: "No turns in the window" }}
                  columns={[
                    {
                      key: "started",
                      header: "Started",
                      shrink: true,
                      // `tsKey`, for the reason the episodes grid above gives.
                      sortValue: (t) => tsKey(t.started_at),
                      cell: (t) => <DateCell at={t.started_at} now={now} />,
                    },
                    {
                      // NO PATH ON THE CELL: the whole row already activates
                      // to the turn, and a link inside it would fire both.
                      key: "id",
                      header: "Turn",
                      cell: (t) => <KeyCell value={t.turn_id.slice(0, 8)} />,
                    },
                    {
                      key: "tokens",
                      header: "Tokens",
                      align: "right",
                      sortValue: (t) => t.total_tokens,
                      cell: (t) => <TokenCell value={t.total_tokens} />,
                    },
                    {
                      key: "calls",
                      header: "Calls",
                      align: "right",
                      sortValue: (t) => t.calls,
                      cell: (t) => <NumberCell value={t.calls} />,
                    },
                  ]}
                />
              </Card>
            </QueryState>
          </>
        )}

        {tab === "access" && (
          <div className="col gap-4">
            <Card>
              <Card.Header icon={<LinkGlyph size="sm" />} subtitle="from the company document">
                <Card.Title>Identity on other surfaces</Card.Title>
              </Card.Header>
              <SettingsState
                error={config.error}
                loading={config.loading}
                doc={config.data ?? null}
                settings={settings}
                seat={seat}
              >
                {Object.keys(configured?.contact ?? {}).length ? (
                  <PropertiesRail
                    groups={[
                      {
                        properties: Object.entries(configured?.contact ?? {}).map(([k, v]) => ({
                          label: k.replace(/_/g, " "),
                          // NOT A CREDENTIAL. A contact identity is a public
                          // handle at a vendor — a Slack member id, a GitHub
                          // login — so a literal is the value and is shown.
                          value: <ConfigValue value={v} />,
                        })),
                      },
                    ]}
                  />
                ) : (
                  <EmptyState
                    size="compact"
                    icon={<LinkGlyph size={32} />}
                    title="No contact identities"
                    description="A human seat needs at least one so inbound activity can be attributed to them. An agent seat's identities are derived from its handle and email."
                  />
                )}
              </SettingsState>
            </Card>

            {/* AN AGENT'S, AND ONLY AN AGENT'S. `mcp_env` is refused on a human
                seat and a human member inherits none of its unit's
                (`mcpEnvOf`), so this card could only ever draw its own empty
                state — whose sentence, "this seat uses whatever the shared MCP
                servers were configured with", is false of a seat that runs no
                tools at all. Absent rather than empty, as above. */}
            {!human && (
              <Card>
                <Card.Header
                  icon={<KeyGlyph size="sm" />}
                  subtitle="merged down the unit chain, this seat's own entries winning"
                  count={Object.keys(credentials).length}
                >
                  <Card.Title>Tool credentials</Card.Title>
                </Card.Header>
                <SettingsState
                  error={config.error}
                  loading={config.loading}
                  doc={config.data ?? null}
                  settings={settings}
                  seat={seat}
                >
                  {Object.keys(credentials).length ? (
                    <div className="col gap-3">
                      {Object.entries(credentials).map(([server, vars]) => (
                        <div key={server} className="col gap-1">
                          <div className="t-label">{server}</div>
                          <PropertiesRail
                            groups={[
                              {
                                properties: Object.entries(vars).map(([k, v]) => ({
                                  label: k,
                                  code: true,
                                  // A CREDENTIAL FIELD, so anything that is not
                                  // one whole `${VAR}` is hidden whatever the
                                  // engine sent. Values are pointers in the
                                  // config and stored verbatim; the engine
                                  // resolves them only where a transport is
                                  // constructed.
                                  value: <ConfigValue secret value={v} />,
                                })),
                              },
                            ]}
                          />
                        </div>
                      ))}
                      <p className="t-caption">
                        These are the <code className="inline">${"{VAR}"}</code> references the
                        config carries, not resolved values: the engine resolves them when it builds
                        this seat&rsquo;s MCP children, and the API redacts anything literal.
                      </p>
                    </div>
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<KeyGlyph size={32} />}
                      title="No per-seat tool credentials"
                      description="This seat uses whatever the shared MCP servers were configured with."
                    />
                  )}
                </SettingsState>
              </Card>
            )}
          </div>
        )}
      </div>
    </>
  );
}

/**
 * One seat, beside the list it was found in.
 *
 * # It asks nothing
 *
 * Every fact a peek needs about a seat is already pushed: the ROSTER carries
 * who it is and the AGENTS slice carries what it is doing, both over the socket
 * the shell already holds. So opening this costs no request, and a seat that is
 * working updates in the rail while the reader watches it — where a query would
 * answer once and then be stale for exactly as long as the peek is interesting.
 * The seat PAGE asks two further questions (`agent`, `agent_memory`); neither of
 * them answers "is this the one I meant".
 *
 * # The last turn is the STREAM's, and it says so
 *
 * `usePhaseEvents` holds the phases that completed while this tab has been
 * open. That is a smaller claim than the page's Model activity tab, which
 * queries the event store, and the empty state below says which of the two it
 * is — "nothing streamed here yet" rather than "this seat has never run",
 * because the second would be a lie the page immediately disproves.
 *
 * # A human seat has no runtime
 *
 * So it gets no runtime panels at all, in the peek exactly as in the tab strip
 * on the page: a panel that cannot have content is not an empty state, it is a
 * claim that the reader is missing something.
 */
export function SeatPeek({ handle }: { handle: string }) {
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const phaseEvents = usePhaseEvents();
  const now = useNow();

  const index = useMemo(() => indexOrg(org), [org]);
  const seat = findSeat(index, handle);
  const agent = liveRow(agents, handle, seat);
  // The ROLE NAME, which is what a phase record carries. `role !== ""` below is
  // load-bearing rather than defensive: an unresolved handle must match NO
  // phase, where an empty role compared against a record's own empty one would
  // match every phase the engine recorded without one.
  const role = agent?.role ?? seat?.name ?? "";

  const lastTurn = useMemo(() => {
    const streamed = streamedPhases(phaseEvents, (r) => role !== "" && r.role === role);
    const live = agent?.live_call ? [fromLiveCall(agent.live_call, agent.role)] : [];
    // Newest turn first, so the head of the list is the one being asked about.
    return groupTurns(mergePhases(streamed, live))[0] ?? null;
  }, [phaseEvents, agent, role]);

  // NOT AN EMPTY RAIL. A `peek=seat:` reaches this from a pasted or hand-edited
  // URL as often as from a row, so the honest answer names the handle that
  // resolved to nothing rather than drawing a header over no seat.
  if (!seat) {
    return (
      <EmptyState
        size="compact"
        icon={<PersonGlyph size={32} />}
        title={`No seat called “${handle}”`}
        description="Seats are addressed by handle. A company revision may have renamed or removed this one."
      />
    );
  }

  const human = seat.kind === "human";
  const reports = seat.reports;
  const sandbox = sandboxes.find((s) => s.role === seat.name) ?? null;

  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Seat"
        icon={human ? "person" : "memory"}
        identifier={`@${seat.handle}`}
        title={seat.name}
        status={
          human ? (
            <Tag appearance="outline">human seat</Tag>
          ) : (
            <StateBadge agent={agent} sandboxes={sandboxes} />
          )
        }
        // THE RAIL READS NOTHING GUARDED. It is opened from a row in a list,
        // and a per-peek read of the whole company document would be an
        // operator-gated fetch on every `[`/`]` step through one — so the model
        // fact is `unread`, which [FactLine] DROPS.
        //
        // It used to pass a NULL ROLE, which the fact line rendered as "needs an
        // operator token": a rail that had asked nobody telling every reader,
        // holding a token or not, that they were missing one. The comment here
        // already claimed this said "unread". Now it does.
        facts={seatFacts({
          seat,
          agent,
          reading: { state: "unread" },
          hierarchy: index.hierarchy,
          human,
        })}
      />

      <div className="col gap-3">
        <section className="col gap-2">
          <div className="t-label">Doing now</div>
          {/* THE SAME SENTENCE THE PAGE PRINTS, out of the same function. A
              rail and the page behind it describing one seat in two different
              words is how a reader comes to believe they are two seats. */}
          <p className="t-body">{statusLine(agent, { sandbox, seat })}</p>
          {seat.goal && <p className="t-caption">Standing goal: {seat.goal}</p>}
          {human && (
            // WHY THERE IS NOTHING BELOW, rather than a second sentence about
            // what this seat is. The status line above already says that; what
            // a reader cannot see is the reason the runtime panels are missing.
            <p className="t-caption">
              No runtime here: the engine never spawns a human seat, so there are no turns, no model
              and no spend for it to report.
            </p>
          )}
          {agent?.last_error && (
            <Callout variant="danger">
              <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
              {agent.last_error.at && ` · ${relTime(agent.last_error.at, now)}`}
            </Callout>
          )}
          {sandbox && awaitingPerson(sandbox.status) && (
            // THE ONE THING A READER CAN ACT ON from a list. A run parked on a
            // question stops this seat until somebody answers it, and a peek
            // that showed "writing code in a sandbox" and nothing else would
            // hide the half that needs them.
            <Callout variant="warning" icon={<HelpGlyph size="md" />}>
              A coding run is paused on a question: {sandbox.question || "(no question recorded)"}
            </Callout>
          )}
        </section>

        {!human && (
          <section className="col gap-2">
            <div className="t-label">Last turn</div>
            {lastTurn ? (
              <div className="thread-entry">
                <div className="row gap-1">
                  {lastTurn.live ? (
                    <Tag variant="info" dot>
                      running
                    </Tag>
                  ) : lastTurn.failed ? (
                    <Tag variant="danger">failed</Tag>
                  ) : (
                    <Tag appearance="outline">finished</Tag>
                  )}
                  <span className="truncate t-cell">
                    {lastTurn.trigger?.summary || lastTurn.trigger?.type || "turn"}
                  </span>
                  <span className="spacer" />
                  <span className="t-caption">{relTime(lastTurn.at, now)}</span>
                </div>
                <div className="row gap-1 wrap">
                  {lastTurn.phases.map((p) => (
                    <PhaseTag key={p.key} phase={p.phase} />
                  ))}
                  <span className="spacer" />
                  <span className="t-caption">{fmtCount(lastTurn.totalTokens)} tokens</span>
                  <a className="t-link" href={href(["activity", "turns", lastTurn.turnId])}>
                    turn ↗
                  </a>
                </div>
              </div>
            ) : (
              <p className="t-caption">
                Nothing has streamed to this tab yet. What the engine has RECORDED for this seat is
                on its own Model activity tab — this panel only ever shows what completed while the
                tab was open.
              </p>
            )}
          </section>
        )}

        <section className="col gap-2">
          <div className="t-label">Direct reports</div>
          {reports.length > 0 ? (
            <div className="list">
              {reports.map((r) => {
                const about = r.goal || r.unit?.name || "";
                return (
                  <a key={r.key} className="thread-entry" href={href(seatPath(r))}>
                    <div className="row gap-2">
                      <Avatar
                        name={r.name}
                        size="xs"
                        variant={r.kind === "human" ? "dashed" : "solid"}
                        decorative
                        title={r.name}
                      />
                      <span className="truncate t-cell" style={{ flex: 1, minWidth: 0 }}>
                        {r.name}
                      </span>
                      {r.kind === "human" ? (
                        <Tag appearance="outline">human</Tag>
                      ) : (
                        <StateBadge
                          agent={agents.find((a) => a.role === r.name)}
                          sandboxes={sandboxes}
                        />
                      )}
                    </div>
                    {/* THE SAME CORRECTION THE CARD ABOVE CARRIES, and the rail
                        is where it bit hardest: this panel is 360px at its
                        narrowest, so a one-line cut lost the goal's subject
                        after a few words — which is exactly the question a peek
                        is opened to answer. `.thread-entry` is already a flex
                        column with its own gap, so the clamped line needs no
                        wrapper. */}
                    {about && (
                      <span className="clamp t-caption" title={about}>
                        {about}
                      </span>
                    )}
                  </a>
                );
              })}
            </div>
          ) : index.hierarchy ? (
            <p className="t-caption">
              Nobody reports to this seat. Delegation follows the chart, so work it cannot do itself
              goes sideways or nowhere.
            </p>
          ) : (
            // NOT NOBODY: THE ENGINE DID NOT SAY. The header fact three inches
            // above this already reads "not reported by this engine" out of the
            // same `index.hierarchy`, and a rail asserting both in one breath is
            // a rail lying in one of them.
            <p className="t-caption">{reportsCaption(seat, index.hierarchy)}.</p>
          )}
        </section>
      </div>
    </>
  );
}

/** A counterparty's stable identity, for a key and for a link.
 *
 *  THE NAME IS NOT IT. A profile's identity is the seat handle, or the
 *  platform and external id for somebody this company has not mapped —
 *  the display name is deliberately excluded, because a person renaming
 *  themselves on a chat surface must not look like a different colleague.
 */
export function counterpartyKey(profile: CounterpartyProfile): string {
  const { handle, platform, external_id } = profile.subject;
  return handle || `${platform ?? "?"}:${external_id ?? "?"}`;
}

/** One colleague this seat has learned about.
 *
 *  # The two instants are both here, and that is the point
 *
 *  `last_updated_at` moves on every interaction; `last_corroborated_at` only
 *  when the traits actually changed. A colleague seen daily whose profile has
 *  not moved in months is one this seat has STOPPED learning about, and the
 *  Plan phase's own prefetch demotes stale traits on exactly that gap — so a
 *  panel carrying one number would disagree with the prompt the agent reads.
 *
 *  # The traits are a bag, not a schema
 *
 *  The model invents the keys. Rendering them as a fixed set of fields would
 *  show whichever three this company happened to produce first and silently
 *  drop the rest, so they are listed as they come.
 */
export function CounterpartyRow({ profile }: { profile: CounterpartyProfile }) {
  const traits = Object.entries(profile.traits ?? {});
  // THE GAP IS DERIVED, not rendered as two dates a reader has to subtract.
  const stale =
    profile.last_corroborated_at &&
    profile.last_updated_at &&
    new Date(profile.last_updated_at).getTime() - new Date(profile.last_corroborated_at).getTime() >
      STALE_TRAIT_MS;
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {profile.subject.handle ? (
          <a className="t-cell" href={href(["company", "people", profile.subject.handle])}>
            <strong>{profile.subject.name || profile.subject.handle}</strong>
          </a>
        ) : (
          <strong className="t-cell">{profile.subject.name || counterpartyKey(profile)}</strong>
        )}
        {!profile.resolved && (
          <Tag appearance="outline" title="not mapped to a seat in this company">
            {profile.subject.platform || "external"}
          </Tag>
        )}
        <span className="spacer" />
        <span className="t-caption">
          {profile.interactions} {profile.interactions === 1 ? "interaction" : "interactions"}
        </span>
        <span className="t-caption">{fmtDateTime(profile.last_updated_at)}</span>
      </div>
      {traits.length > 0 ? (
        <div className="row gap-1 wrap">
          {traits.map(([key, value]) => (
            <Tag key={key} appearance="outline" title={key}>
              {key}: {typeof value === "string" ? value : JSON.stringify(value)}
            </Tag>
          ))}
        </div>
      ) : (
        <p className="t-caption">Seen, and nothing believed about them yet.</p>
      )}
      {stale && (
        <p className="t-caption">
          Last corroborated {fmtDateTime(profile.last_corroborated_at)} — this seat is still working
          with them and has stopped learning about them.
        </p>
      )}
    </div>
  );
}

/** How far `last_updated_at` may run ahead of `last_corroborated_at` before
 *  the profile is called stale.
 *
 *  THIRTY DAYS, which is the shortest inbox retention this engine allows and
 *  therefore the shortest span over which "still working together" is a fact
 *  the company still holds evidence for. Shorter and every colleague seen
 *  twice in a week reads as stale; longer and a profile nobody has corroborated
 *  since last quarter looks current. */
const STALE_TRAIT_MS = 30 * 24 * 60 * 60 * 1000;

/** One recorded turn in one conversation.
 *
 *  # `reply` and `unsent` are NOT the same field rendered twice
 *
 *  Both carry the turn's final artifact, and which one holds it is the whole
 *  record of whether anybody received it. A turn can end with real work done
 *  and no way to say so — the round budget ran out, the loop broke, the
 *  reviewer closed it — and a panel that rendered the two alike would show
 *  work announced to nobody as announced. That is not a cosmetic difference:
 *  it is the exact confusion that made a seat answer a follow-up against a
 *  message it had never sent.
 */
export function ThreadTurn({ entry }: { entry: ConversationEntry }) {
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {entry.trigger && <Tag appearance="outline">{entry.trigger}</Tag>}
        {entry.decision && <Tag appearance="outline">{entry.decision}</Tag>}
        <span className="spacer" />
        {entry.turn_id && (
          <a className="t-link mono t-caption" href={href(["activity", "turns", entry.turn_id])}>
            turn
          </a>
        )}
        <span className="t-caption">{entry.at ? fmtDateTime(entry.at) : ""}</span>
      </div>
      {entry.intent && <p className="t-body">{entry.intent}</p>}
      {entry.reply && <p className="t-caption">{entry.reply}</p>}
      {entry.unsent && (
        // THE ONE THAT REACHED NOBODY, marked. See the doc above.
        <Callout variant="warning" icon={<ErrorGlyph size="md" />}>
          <strong>Nothing was delivered.</strong> {entry.unsent}
        </Callout>
      )}
      {entry.completed_work && <p className="t-caption">{entry.completed_work}</p>}
      {/* A BLOCK, NOT A BARE `pre`. This was hand-written markup carrying the
          block stylesheet's own class, which is `overflow: auto` under a
          ceiling — so a long tool log was a scroll container with no tab stop
          and no accessible name, reachable by pointer and by nothing else.
          `focusWhenScrollable` measures the rendered box and names the ones
          that scroll; `plain` drops the header, since the entry above already
          says whose calls these are. */}
      {entry.tool_calls && (
        <CodeBlock
          plain
          maxHeight={RECORD_MAX_HEIGHT}
          focusWhenScrollable
          label="Tool calls in this turn"
          code={entry.tool_calls}
        />
      )}
    </div>
  );
}
