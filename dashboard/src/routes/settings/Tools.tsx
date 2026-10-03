/**
 * Every tool a seat can call, grouped by where it came from.
 *
 * The origin grammar is two-valued — `builtin`, `mcp:<server>`, `a2a` — and it
 * is recorded at registration, which is the only frame that knows and the last
 * that can say.
 */

import { useCallback, useMemo, useRef, type CSSProperties } from "react";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { plural } from "~/lib/format.ts";
import { useNavigator, useParam, useRoute } from "~/app/router.tsx";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  Disclosure,
  EmptyState,
  FilterChip,
  FilterChipGroup,
  InlineCode,
  Input,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  WrenchGlyph,
  PlugGlyph,
  PlusGlyph,
  PackageGlyph,
  SearchGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { RECORD_MAX_HEIGHT, Section, SeatChip } from "~/components/common.tsx";
import { uiletTone } from "~/ui/primitives.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { NumberCell, KeyCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { needsSentence } from "~/lib/refusal.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  grantedSeats,
  nodeOnlyNames,
  serverOrigin,
  serversUpLine,
  takenNames,
} from "~/lib/mcpServers.ts";
import { AddMcpServerDialog } from "./AddMcpServerDialog.tsx";
import { failureLine, McpServers, ServerHeader } from "./McpServers.tsx";
import { useOrg, useTools } from "~/lib/store-hooks.ts";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import type { Capability } from "~/lib/tools.ts";
import {
  capabilityOf,
  capabilityTone,
  hintSentence,
  hintsAdvertised,
  schemaFields,
} from "~/lib/tools.ts";
import type { ToolAnnotations, ToolHint, ToolRow } from "~/protocol/index.ts";
import type { McpServerStatus } from "~/contract/mcp.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageLabels } from "~/app/Shell.tsx";

/**
 * What a capability is drawn as.
 *
 * `capabilityTone` in `~/lib/tools.ts` answers in OUR tone vocabulary, and
 * that module is not this port's to change, so the spelling is translated at
 * the one seam that renders it. An unadvertised capability has no tone at all
 * and takes the neutral pill, which is the same nothing our badge drew.
 */
function capabilityVariant(c: Capability) {
  const tone = capabilityTone(c);
  return tone ? uiletTone(tone) : "neutral";
}

/** Worst first: what an operator auditing a new server reads down. */
const CAPABILITY_ORDER: Record<Capability, number> = {
  destroys: 0,
  writes: 1,
  unknown: 2,
  reads: 3,
};

function originOf(source: string): { kind: string; detail: string } {
  const idx = source.indexOf(":");
  if (idx < 0) return { kind: source || "unknown", detail: "" };
  return { kind: source.slice(0, idx), detail: source.slice(idx + 1) };
}

/** The server behind an `mcp:<server>` origin, empty for a builtin or an A2A tool. */
function mcpServerOf(source: string): string {
  const { kind, detail } = originOf(source);
  return kind === "mcp" ? detail : "";
}

/**
 * How many arguments a tool takes, or null where this build cannot say.
 *
 * ABSENT AND `{}` ARE DIFFERENT, which is the whole of it and is `ToolRow`'s
 * own rule: a tool with no `input_schema` is one this build sent no schema
 * for, and a schema with no properties is a tool that genuinely takes no
 * arguments. Collapsed — as the column did, rendering both as "none" — an
 * operator auditing a new server reads "takes nothing" about a tool whose
 * arguments simply did not reach them.
 */
function argumentCount(tool: ToolRow): number | null {
  return tool.input_schema ? schemaFields(tool).length : null;
}

/**
 * The facts a tool is recognised by, in the catalogue's own column order.
 *
 * ONE FUNCTION for the grid's columns and the rail's header, so a reader who
 * peeks a tool reads the same things in the same order they were scanning a
 * moment earlier. What it CAN do is not among them — that is the header's own
 * pill, and a capability spelled in colour and again in a list reads as two
 * facts about one tool.
 */
function toolFacts(tool: ToolRow): Fact[] {
  return [
    { label: "Origin", value: <span className="mono">{tool.source}</span> },
    {
      label: "Delivers to",
      // NOT A DASH. A tool that reaches nobody outside the turn is a positive
      // fact the registry recorded, not a value somebody forgot to set.
      value: tool.delivers || "nobody",
    },
    { label: "Arguments", value: <NumberCell value={argumentCount(tool)} /> },
    { label: "Advertised", value: `${hintsAdvertised(tool.annotations)} of 4 hints` },
  ];
}

/**
 * One behavioural hint, as a word.
 *
 * THE THIRD VALUE SURVIVES. "The server said no" and "the server said
 * nothing" are different facts — `lib/tools.ts` exists to keep them apart —
 * so an unadvertised hint is named as unadvertised rather than rendered as a
 * denial. A word this build does not know renders as ITSELF, for the reason
 * the event registry gives: a rolling upgrade puts a newer node's value on an
 * older node's screen.
 */
function hintWord(hint: ToolHint | undefined): string {
  if (hint === "yes") return "yes";
  if (hint === "no") return "no";
  if (hint === undefined || hint === "unknown") return "not advertised";
  return hint;
}

/** The four hints as their own rows — the audit an operator actually reads. */
function hintRows(ann: ToolAnnotations | undefined): { label: string; value: string }[] {
  return [
    { label: "Read-only", value: hintWord(ann?.read_only) },
    { label: "Destructive", value: hintWord(ann?.destructive) },
    { label: "Idempotent", value: hintWord(ann?.idempotent) },
    { label: "Open-world", value: hintWord(ann?.open_world) },
  ];
}

/**
 * Which seats can call a tool, and the rule that decides it.
 *
 * THE ENGINE'S GRANT, NEVER A CLIENT'S. A builtin and an A2A tool are
 * registered on every agent seat the engine spawns; an MCP server's tools
 * reach exactly the seats the engine GRANTS that server — every agent seat for
 * a shared server, and for a per-seat template only a seat declaring
 * credentials for it under `mcp_env`, its own or its unit's. That grant is on
 * the pushed org as each agent seat's `tool_sources`, resolved by
 * `config.MCPServer.Grants` — the one rule the engine starts children by — so
 * this reads it rather than walking the company document to re-derive it. The
 * walk it replaced asked for the active configuration twice per addressed tool
 * (the server's entity and the whole document) and was a second copy of the
 * `mcp_env` inheritance rule.
 *
 * NULL IS UNKNOWN: a roster whose agent seats carry no `tool_sources` came
 * from a node older than the field, and a tool whose holders are unknown must
 * not be drawn as a tool nobody holds.
 */
interface Holders {
  /** The seats granted it. */
  seats: Seat[];
  /** Every agent seat holds it. */
  everyone: boolean;
  /** One sentence naming the rule this answer came from. */
  why: string;
}

function holdersOf(tool: ToolRow, seats: Seat[]): Holders | null {
  const agents = seats.filter((s) => s.kind === "agent");
  const server = mcpServerOf(tool.source);
  if (!server) {
    return {
      seats: agents,
      everyone: true,
      why:
        tool.source === "a2a"
          ? "An A2A tool is the seat's own way to reach a colleague, so every agent seat carries one."
          : "The engine registers its builtins on every agent seat it spawns.",
    };
  }
  const granted = grantedSeats(seats, server);
  if (granted === null) return null;
  return {
    seats: granted,
    everyone: agents.length > 0 && granted.length === agents.length,
    why: `The engine grants ${server} to every agent seat when it is shared, and otherwise only to a seat that declares credentials for it under mcp_env.`,
  };
}

/**
 * The grant a seat's `tool_sources` is published under: it is derived from the
 * seat's runtime half (the credentials it declares under `mcp_env`), which the
 * org projection carries only for an audience that may read the company's
 * configuration.
 */
const GRANT_SOURCES = "config:read";

/**
 * One tool, resolved from its name, wherever it is being shown.
 *
 * THE PAGE AND THE RAIL RESOLVE IT THE SAME WAY, which is the only reason the
 * two can agree: a tool is addressed by NAME, a name is unique in one registry
 * but not across two servers, and deciding what to do about that in two places
 * is how one surface comes to pick a row silently while the other says the
 * name is shared.
 *
 * IT ASKS THE ENGINE NOTHING. Everything here comes off the pushed catalogue,
 * and who holds a tool off the pushed org ([holdersOf]), so a tool costs no
 * request wherever it is shown — a header, a peek, every `[`/`]` step through
 * the peek's neighbours.
 */
function useTool(name: string): {
  /** Every registration under this name — see [ToolBody]. */
  matches: ToolRow[];
  tool: ToolRow | null;
  server: string;
  /** The catalogue itself has not arrived — not a tool that does not exist. */
  cold: boolean;
} {
  const tools = useTools();
  // EVERY REGISTRATION UNDER THIS NAME, not the first. A name is unique in one
  // registry, but two servers may advertise the same bare name and the
  // catalogue carries both rows — a tool is addressed by name alone, so the
  // honest move is to say the name is shared rather than to pick one silently.
  const matches = useMemo(() => tools.filter((t) => t.name === name), [tools, name]);
  const tool = matches[0] ?? null;
  return {
    matches,
    tool,
    server: tool ? mcpServerOf(tool.source) : "",
    cold: tools.length === 0,
  };
}

/**
 * What a tool IS, under whichever header named it.
 *
 * ONE BODY FOR THE PAGE AND THE RAIL. `#/settings/tools/{name}` is a tool's
 * address in `objects.ts` and therefore where the rail's `Open ↗` goes — the
 * screen accepted the segment and rendered the unfiltered catalogue, so that
 * link led nowhere in particular and a reader who followed it lost the tool
 * they were reading. Shared rather than duplicated because the two surfaces
 * answer the same question and a second copy would answer it differently
 * within a release.
 */
function ToolBody({ name }: { name: string }) {
  const org = useOrg();
  const viewer = useViewer();
  const index = useMemo(() => indexOrg(org), [org]);
  const { matches, tool, server, cold } = useTool(name);

  // THE CATALOGUE HAS NOT ARRIVED YET, which is not the same screen as a tool
  // that does not exist. An engine registers its builtins at boot, so an empty
  // catalogue in a connected browser is a snapshot still in flight — and an
  // empty state here would tell a reader their tool is gone every time they
  // open one on a cold tab.
  if (cold) return <Skeleton variant="text" rows={6} label="Loading" />;

  if (!tool) {
    return (
      <EmptyState
        size="compact"
        icon={<WrenchGlyph size="xl" />}
        title={`No tool called “${name}”`}
        description="Builtins register at boot and MCP tools are discovered from the servers in mcp_servers. A tool whose server failed to start, or whose name has changed, is not in this node's registry."
      />
    );
  }

  const fields = schemaFields(tool);
  const holders = holdersOf(tool, index.seats);

  return (
    <div className="col gap-3">
      <section className="col gap-2">
        <div className="t-label">What it does</div>
        {tool.description ? (
          <p className="t-body">{tool.description}</p>
        ) : (
          <p className="t-body muted">
            The server advertised no description, so a model is offered this tool by name alone.
          </p>
        )}
        {matches.length > 1 && (
          // TWO REGISTRATIONS, ONE NAME. `mcp_servers.tool_prefix` exists
          // for exactly this, and until somebody sets one the model's call
          // is resolved by the registry rather than by the operator.
          <p className="t-caption">
            {plural(matches.length, "registration")} advertise this name:{" "}
            {matches.map((t) => t.source).join(", ")}.
          </p>
        )}
      </section>

      <section className="col gap-2">
        <div className="t-label">What it advertises</div>
        <PropertiesRail groups={[{ properties: hintRows(tool.annotations) }]} />
        <p className="t-caption">{hintSentence(tool.annotations)}</p>
      </section>

      <section className="col gap-2">
        <div className="t-label">Arguments</div>
        {!tool.input_schema ? (
          <p className="t-body muted">
            This build sent no schema for this tool, which is not the same as a tool that takes no
            arguments.
          </p>
        ) : fields.length === 0 ? (
          <p className="t-body">It takes no arguments.</p>
        ) : (
          <>
            <PropertiesRail
              groups={[
                {
                  properties: fields.map((f) => ({
                    label: f.name,
                    identifierLabel: true,
                    // A WORD FOR AN ABSENCE. "type not stated" is what the
                    // server failing to advertise a type actually means; a
                    // dash is read as "dash" or skipped, and here it sat in
                    // the column with the least to say.
                    value: f.required
                      ? `${f.type || "type not stated"} · required`
                      : f.type || "type not stated",
                  })),
                },
              ]}
            />
            <Disclosure title="The schema as JSON" mono>
              {/* BOUNDED, like every other record block. A large MCP server's
                  tool takes a schema of hundreds of lines, and an unbounded
                  block pushes the rest of the screen off under it — which is
                  the case `RECORD_MAX_HEIGHT` is written down for. */}
              <CodeBlock
                plain
                maxHeight={RECORD_MAX_HEIGHT}
                selectable
                label={`${tool.name}'s input schema, as JSON`}
                code={JSON.stringify(tool.input_schema, null, 2)}
              />
            </Disclosure>
          </>
        )}
      </section>

      <section className="col gap-2">
        <div className="t-label">Which seats hold it</div>
        {holders === null ? (
          // WHICH OF TWO ABSENCES, named. A seat's `tool_sources` is derived
          // from its runtime half — the credentials it declares — so the
          // engine sends it only to a reader who may read the company's
          // configuration; a roster without it for anybody else is the grant
          // withheld, and only for a reader who holds it is it a node older
          // than the field. Said as the second to the first, it sent a person
          // looking for an upgrade when what they lacked was a grant.
          <p className="t-body muted">
            {viewer.grants.includes(GRANT_SOURCES) ? (
              <>
                Which seats are granted <span className="mono">{server}</span> is not on the roster
                this engine sent: the node serving it is older than the grant.
              </>
            ) : (
              needsSentence(`Seeing which seats are granted ${server}`, [GRANT_SOURCES])
            )}
          </p>
        ) : holders.everyone ? (
          <>
            <p className="t-body">
              Every agent seat — {plural(holders.seats.length, "seat")} in this company.
            </p>
            <p className="t-caption">{holders.why}</p>
          </>
        ) : holders.seats.length > 0 ? (
          <>
            <div className="row gap-2 wrap">
              {holders.seats.map((seat) => (
                <SeatChip
                  key={seat.handle}
                  name={seat.name}
                  handle={seat.handle}
                  kind={seat.kind}
                />
              ))}
            </div>
            <p className="t-caption">{holders.why}</p>
          </>
        ) : (
          // A TEMPLATE NOBODY INSTANTIATES. The server is configured, the
          // tool is in the registry, and no seat can call it — which is a
          // finding rather than an empty list, so it is said outright.
          <>
            <p className="t-body">
              No seat can call this: no seat is granted <span className="mono">{server}</span>, so
              it launches for nobody.
            </p>
            <p className="t-caption">{holders.why}</p>
          </>
        )}
      </section>
    </div>
  );
}

/**
 * The header over an addressed tool, on the page.
 *
 * SEPARATE FROM THE PEEK'S only in its `size`: both are built from
 * [toolFacts], so a reader who peeks a tool and then opens it reads the same
 * things in the same order. It renders nothing while the catalogue is cold or
 * the name matches nothing — [ToolBody] is what says which of the two it is,
 * and two components saying it would say it twice.
 */
function ToolHeader({ name }: { name: string }) {
  const { tool } = useTool(name);
  // THE TRAIL NAMES THE TOOL AS ITS TITLE DOES — the tool's own title where it
  // has one, its name where it has none — rather than the address's raw name
  // in the mono face over a header saying something else in another.
  usePageLabels(tool ? { [name]: tool.title || tool.name } : {});
  if (!tool) return null;
  const capability = capabilityOf(tool.annotations);
  return (
    <ObjectHeader
      kind="Tool"
      icon="wrench"
      identifier={tool.title ? tool.name : undefined}
      title={tool.title || tool.name}
      status={<Tag variant={capabilityVariant(capability)}>{capability}</Tag>}
      facts={toolFacts(tool)}
    />
  );
}

/**
 * One tool, beside the catalogue it was found in.
 *
 * # It reads the PUSHED catalogue rather than asking for the tool
 *
 * No question answers one tool: the registry arrives whole with the connect
 * snapshot and is re-pushed when a server is re-discovered. So opening this
 * costs no request and a tool whose server has just come up appears in the
 * rail while the reader is looking at it — where a query would answer once
 * and then be stale for exactly as long as the peek is interesting. [SeatPeek]
 * says the same about the roster, for the same reason.
 *
 * # And nothing else either
 *
 * Who holds it is the pushed org's `tool_sources` ([holdersOf]), so opening a
 * tool — and stepping through its neighbours with `[` and `]` — asks the
 * engine nothing at all.
 */
export function ToolPeek({ name }: { name: string }) {
  const { tool, cold } = useTool(name);
  if (cold) return <Skeleton variant="text" rows={6} label="Loading" />;
  if (!tool) return <ToolBody name={name} />;
  const capability = capabilityOf(tool.annotations);
  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Tool"
        icon="wrench"
        // THE NAME IN ONE PLACE. A server that advertised a human title gets
        // both — the identifier a config names and the words a person reads —
        // and one that did not gets the name as the title alone, rather than
        // the same string printed twice a line apart.
        identifier={tool.title ? tool.name : undefined}
        title={tool.title || tool.name}
        status={<Tag variant={capabilityVariant(capability)}>{capability}</Tag>}
        facts={toolFacts(tool)}
      />
      <ToolBody name={name} />
    </>
  );
}

/**
 * Why a server's slice of the catalogue is empty, from what the engine
 * reports about it.
 *
 * A server's tools reach the registry only from an instance that STARTED, so
 * an empty slice is always one of the engine's states — and saying which
 * turns "nothing here" into what to go and fix. Null status (refused, or not
 * yet answered) says only what the registry itself can.
 */
export function emptyOriginReason(
  server: string,
  servers: readonly McpServerStatus[] | null,
): string {
  if (!server) return "The registry holds no tool from this origin.";
  const row = servers?.find((s) => s.name === server);
  if (!row) {
    return servers
      ? `No configuration or live node carries a server called ${server}.`
      : "A server's tools are listed once an instance of it starts on a node.";
  }
  const failed = row.nodes.find((n) => n.error);
  switch (row.state) {
    case "failing":
      return failed
        ? `Every instance a node launched failed to start. On ${failed.node}, ${failureLine(failed)}`
        : "Every instance a node launched failed to start.";
    case "partial":
      return failed
        ? `It started somewhere, but not on the node serving this page. On ${failed.node}, ${failureLine(failed)}`
        : "It started somewhere, but not on the node serving this page.";
    case "not_started":
      return row.shared
        ? "No node has started it yet: a change no node has applied."
        : "No node started it: no seat on a live node declares credentials for it under mcp_env.";
    case "unreported":
      return "No live node reports its MCP servers, so whether it started is unknown.";
    default:
      return "It is running, and the node serving this page has not listed its tools yet.";
  }
}

// The catalogue's columns, which close over nothing on the screen — so a module
// constant: a tools push that moved one tool draws that row, and typing in the
// search box above draws only the rows it brings back.
const TOOL_COLUMNS: GridColumn<ToolRow>[] = [
  {
    key: "name",
    header: "Tool",
    // SIZED TO THE NAME. With the columns this table grew, the
    // name column took whatever was left and broke
    // `comment_on_work_item` across three lines mid-word — an
    // identifier a reader is matching against a config file, so
    // a break in the middle of one is worse than a narrower
    // description beside it. A shrunk cell never wraps, which is
    // what `KeyCell` inherits here rather than restating.
    shrink: true,
    // TWICE A SHRINK COLUMN'S SHARE. The name is the identifier the
    // list is scanned by, and at a shrink column's fifth of the grid
    // — beside Settings' column at 1280, 150px — the long ones were
    // clipped mid-word ("comment_on_work_it"). What gives way
    // instead is below: the description to its floor, then the
    // columns a tool's own page answers.
    width: "fit-content(40%)",
    sortValue: (t) => t.name,
    cell: (t) => <KeyCell value={t.name} />,
  },
  {
    key: "origin",
    header: "Origin",
    shrink: true,
    drop: 2,
    sortValue: (t) => t.source,
    cell: (t) => {
      const { kind, detail } = originOf(t.source);
      return (
        <span className="row gap-1">
          <Tag appearance="outline">{kind}</Tag>
          {detail && <span className="t-caption mono">{detail}</span>}
        </span>
      );
    },
  },
  {
    key: "desc",
    header: "What it does",
    floor: "8rem",
    // CLAMPED, with the whole sentence on the tool's own page and
    // in the title. A description is written for a MODEL and runs
    // to a paragraph, and beside Settings' column at 1280 this
    // track is a sixth of the grid: unclamped, every row was a
    // 350px column of prose and the table showed two tools a
    // screen. The list is for finding a tool; its page is for
    // reading one.
    cell: (t) => (
      <span className="col" style={{ gap: 2 }}>
        <span className="t-caption clamp" title={t.description || undefined}>
          {t.description || <span className="muted">no description</span>}
        </span>
        <span
          className="t-caption clamp"
          style={{ "--clamp-lines": 1 } as CSSProperties}
          title={hintSentence(t.annotations)}
        >
          {hintSentence(t.annotations)}
        </span>
      </span>
    ),
  },
  {
    key: "can",
    header: "Can",
    shrink: true,
    // Sorted worst-first, which is the order an operator
    // auditing a new server reads: the tools that can destroy
    // something are the ones they have to decide about.
    sortValue: (t) => CAPABILITY_ORDER[capabilityOf(t.annotations)],
    cell: (t) => {
      const c = capabilityOf(t.annotations);
      return (
        <span className="row gap-1">
          <Tag variant={capabilityVariant(c)}>{c}</Tag>
          {t.annotations?.open_world === "yes" && <Tag appearance="outline">outside</Tag>}
        </span>
      );
    },
  },
  {
    key: "delivers",
    header: "Delivers to",
    shrink: true,
    drop: 1,
    sortValue: (t) => t.delivers ?? "",
    cell: (t) =>
      t.delivers ? (
        <Tag appearance="outline">{t.delivers}</Tag>
      ) : (
        <span className="t-caption">nobody</span>
      ),
  },
  {
    key: "args",
    header: "Arguments",
    shrink: true,
    drop: 3,
    align: "right",
    // ABSENT SORTS LAST in both directions, which is the grid's
    // own rule for a null — "this build sent no schema" is not
    // the smallest count, it is not a count.
    sortValue: (t) => argumentCount(t),
    cell: (t) => {
      const fields = schemaFields(t);
      const required = fields.filter((f) => f.required).length;
      return (
        <span className="row gap-1">
          <NumberCell
            value={argumentCount(t)}
            title={
              fields.length
                ? fields
                    .map((f) => `${f.name}${f.required ? "*" : ""}: ${f.type || "type not stated"}`)
                    .join("\n")
                : undefined
            }
          />
          {required > 0 && <span className="t-caption">{required} required</span>}
        </span>
      );
    },
  },
];

/**
 * How often the servers' status is asked again, in ms.
 *
 * FIFTEEN SECONDS, the Nodes screen's cadence and for its reason: the answer
 * is the lease table, and a poll slower than the presence TTL would show a
 * server running on a node whose lease had already expired. It also bounds
 * how long a server just added reads "not started" after a node applied it.
 */
const SERVERS_POLL_MS = 15_000;

export function Tools({ server, tool }: { server?: string; tool?: string }) {
  // `/` FOCUSES THIS SCREEN'S SEARCH rather than opening the palette over it.
  const searchBox = useRef<HTMLInputElement>(null);
  useSearchTarget(searchBox);
  const tools = useTools();
  const route = useRoute();
  const nav = useNavigator();
  const org = useOrg();
  const seats = useMemo(() => indexOrg(org).seats, [org]);
  const [q, setQ] = useParam("q", "");
  const [chosen, setChosen] = useParam("origin", "");
  // THE ADD FORM IS AN ADDRESS (`?add=server`), so Settings › Integrations'
  // "Add an MCP server" tile can open it and a reload keeps it open.
  const [adding, setAdding] = useParam("add", "");
  const status = useQuery("mcp_servers_status", undefined, { pollMs: SERVERS_POLL_MS });
  const access = useConfigWriteAccess();
  const addressedStatus = useMemo(
    () => (server ? (status.data?.servers.find((s) => s.name === server) ?? null) : null),
    [server, status.data],
  );
  const taken = useMemo(() => takenNames(status.data?.servers), [status.data]);
  const nodeOnly = useMemo(() => nodeOnlyNames(status.data?.servers), [status.data]);

  // `#/settings/tools/servers/{name}` IS A FILTER ON THE ORIGIN, and this screen
  // accepted the segment and dropped it: the crumb trail said "Servers /
  // github" over the whole unfiltered catalogue, and `Open ↗` from a tool's
  // rail landed on the same place. The path wins where it names one, and the
  // chips write the query key as before.
  const addressed = server ? serverOrigin(server) : "";
  const origin = chosen || addressed;
  const pick = useCallback(
    (next: string) => {
      // THE PATH IS THE FILTER on that URL, so changing it has to LEAVE the
      // path rather than write a query beside it — a `?origin=` under
      // `/servers/github` would be two filters with the narrower one winning
      // silently, and `All` would clear the one that is not there.
      //
      // CARRYING THE REST OF THE QUERY, because leaving the path is this
      // screen's own chip rather than a new screen: a search, a sort and an
      // open rail all live in keys beside `origin`, and rebuilding the URL
      // from that one key would drop every one of them.
      if (addressed) {
        const query = new URLSearchParams(route.query);
        if (next) query.set("origin", next);
        else query.delete("origin");
        nav.replace(["settings", "tools"], query);
        return;
      }
      setChosen(next);
    },
    [addressed, nav, route.query, setChosen],
  );

  // THE ORIGIN IN FORCE ALWAYS HAS A CHIP. The chips are built from the
  // registry, and a server that registered nothing (it failed, or no node
  // started it) has no row there — so `servers/{name}` filtered by an origin
  // no chip named, and the group showed nothing selected at all: neither
  // `All` nor the filter, with no way to see what was narrowing the list.
  const origins = useMemo(() => {
    const map = new Map<string, number>();
    for (const t of tools) map.set(t.source, (map.get(t.source) ?? 0) + 1);
    if (origin && !map.has(origin)) map.set(origin, 0);
    return [...map.entries()].sort((a, b) => b[1] - a[1]);
  }, [tools, origin]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return tools
      .filter((t) => !origin || t.source === origin)
      .filter(
        (t) =>
          !needle ||
          t.name.toLowerCase().includes(needle) ||
          (t.description ?? "").toLowerCase().includes(needle),
      );
  }, [tools, q, origin]);

  // THE ORDER `[` AND `]` WALK — the catalogue as this reader filtered and
  // searched it, rather than the order the registry happened to push.
  usePeekNeighbours(
    useMemo(() => rows.map((t) => ({ kind: "tool" as const, id: t.name })), [rows]),
  );

  const { open: openPeek } = usePeekControls();

  /**
   * A row's click.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one copy
   * of which clicks mean elsewhere and reads a mouse event; the `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  const openTool = useCallback(
    (tool: ToolRow, e: React.MouseEvent | React.KeyboardEvent) => {
      const go = () => openPeek({ kind: "tool", id: tool.name });
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek],
  );

  const builtins = tools.filter((t) => t.source === "builtin").length;
  const mcp = tools.filter((t) => t.source.startsWith("mcp")).length;
  const mcpOrigins = new Set(tools.filter((t) => t.source.startsWith("mcp")).map((t) => t.source))
    .size;
  // HOW MANY OF THESE CAN WRITE, which is the number an operator opens this
  // screen for after adding a server — and which the catalogue could not
  // answer at all while it carried three strings per tool.
  const writes = tools.filter((t) => {
    const c = capabilityOf(t.annotations);
    return c === "writes" || c === "destroys";
  }).length;
  const unannotated = tools.filter((t) => hintsAdvertised(t.annotations) === 0).length;

  return (
    <>
      <PageActions>
        <Tag appearance="outline">{plural(tools.length, "tool")} registered</Tag>
        {/* NEVER HIDDEN: drawn for every reader, and disabled with the reason
            for one whose token cannot change the configuration. */}
        <Button
          variant="primary"
          size="small"
          leadingIcon={<PlusGlyph />}
          disabledReason={access.can ? undefined : access.reason}
          title={access.can ? undefined : access.reason}
          onClick={() => setAdding("server")}
        >
          Add an MCP server
        </Button>
      </PageActions>
      {adding === "server" && (
        <AddMcpServerDialog
          taken={taken}
          nodeOnly={nodeOnly}
          onClose={() => setAdding("")}
          onAdded={() => {
            setAdding("");
            status.refetch();
          }}
        />
      )}
      <PageNote>
        What the models can actually call. An executor is shown the built-in tools and each MCP
        server by name; a server's own tools are discovered and activated during a turn.
      </PageNote>

      {/* THE TOOL THIS PATH NAMES, ABOVE the catalogue rather than under it.
          `Secrets` puts its addressed row below, and the reason it can is that
          the credential list is short; this catalogue is 36 rows in an empty
          company and grows with every MCP server, so a reader following
          `Open ↗` from a tool's rail would land on their tool and have to
          scroll the whole registry to reach it. The catalogue stays below as
          the context the crumb promises. A name matching nothing has to SAY
          so rather than leaving the screen looking like an ordinary listing —
          which is what this path did before it was routed at all, and
          `ToolBody` is what says which of the two it is. */}
      {tool && (
        <>
          <ToolHeader name={tool} />
          <ToolBody name={tool} />
        </>
      )}
      {/* A SERVER'S PAGE: what the engine reports about it, above the
          catalogue narrowed to its tools — so a row's click in the servers
          table opens something, including for a server with no tools. */}
      {server && !tool && (
        <ServerHeader
          name={server}
          server={addressedStatus}
          seats={seats}
          loading={status.loading && !status.data}
          unavailable={status.data ? null : status.error}
        />
      )}

      <Card padding="none">
        <StatGroup columns={4}>
          <StatCard
            icon={<WrenchGlyph size="xs" />}
            label="Total"
            value={tools.length}
            sub="across every origin"
          />
          <StatCard
            icon={<PackageGlyph size="xs" />}
            label="Built in"
            value={builtins}
            sub="shipped by the engine itself"
          />
          <StatCard
            icon={<PlugGlyph size="xs" />}
            label="From MCP servers"
            value={mcp}
            sub={
              // THE SERVERS AS THE ENGINE REPORTS THEM, the same answer the
              // table under this tile draws — counted off the registry, four
              // failing servers read "0 servers" above a table of four. A
              // reader the status is refused to gets the registry's count,
              // which is all this node can say to them.
              status.data
                ? serversUpLine(status.data.servers)
                : `${plural(mcpOrigins, "server")} with tools`
            }
          />
          <StatCard
            icon={<TriangleAlertGlyph size="xs" />}
            label="Can write"
            value={writes}
            sub={
              unannotated
                ? `${unannotated} more advertise nothing at all`
                : "every tool advertises what it does"
            }
          />
        </StatGroup>
      </Card>

      <Section
        title="MCP servers"
        hint="What each server is launched as, which seats it reaches, and what every live node did with it."
      >
        <McpServers
          data={status.data}
          loading={status.loading}
          error={status.error}
          refusal={status.refusal}
          seats={seats}
        />
      </Section>

      <div className="toolbar">
        {/* A BASIS, NOT ZERO: at `flex: 1` the field's basis was 0, so on a
            phone the chips kept the line and squeezed the search to "Sear".
            With a real basis the chips wrap under it instead. */}
        <div style={{ maxWidth: 340, flex: "1 1 14rem" }}>
          <Input
            type="search"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            aria-label="Search tools"
            ref={searchBox}
            placeholder="Search by name or description"
            leading={<SearchGlyph size="sm" />}
            width="full"
          />
        </div>
        <span className="spacer" />
        {/* A NAMED ROW, because a set of chips with nothing to hang them on
            is what a screen reader was handed: six words in a row, none of
            them saying what they filter. The group carries the name. These
            chips were also always mutually exclusive — pressing one replaced
            the other — and announced themselves as independent pressed buttons
            across one tab stop each, so `semantics="radio"` says what they
            are and the arrows move inside one stop.

            `All` is the ABSENCE of a filter rather than one of them, so it is
            a chip with the empty value: the group then always has a chosen
            member and needs no `allowNone`. */}
        <FilterChipGroup
          label="Tool origin"
          hideLabel
          semantics="radio"
          value={origin}
          onValueChange={(next) => pick(next ?? "")}
        >
          <FilterChip value="">All</FilterChip>
          {origins.map(([source, count]) => (
            <FilterChip key={source} value={source} count={count}>
              {source}
            </FilterChip>
          ))}
        </FilterChipGroup>
      </div>

      {tools.length > 0 && rows.length === 0 && !q.trim() && origin ? (
        // AN ORIGIN WITH NO TOOLS, which is not a search that found nothing:
        // there is no query to quote. The sentence names what is filtering
        // and why it is empty, and the way out is a control rather than a
        // hunt for the `All` chip.
        <EmptyState
          size="compact"
          icon={<PlugGlyph size="xl" />}
          title={
            mcpServerOf(origin)
              ? `${mcpServerOf(origin)} has registered no tools`
              : `No tool comes from ${origin}`
          }
          description={emptyOriginReason(mcpServerOf(origin), status.data?.servers ?? null)}
          action={
            <Button variant="secondary" size="small" onClick={() => pick("")}>
              Show all tools
            </Button>
          }
        />
      ) : !tools.length ? (
        <EmptyState
          icon={<WrenchGlyph size="xl" />}
          title="No tools are registered"
          description="Builtins register at boot; MCP tools are discovered from the servers in mcp_servers. An engine with no active configuration has neither."
        />
      ) : (
        <Card padding="none">
          <DataGrid<ToolRow>
            rows={rows}
            rowKey={(t) => `${t.source}:${t.name}`}
            // A ROW IS A REAL LINK AND A PLAIN CLICK PEEKS. The href is the
            // frame's own answer to where a tool lives, so the row's target
            // and the rail's `Open ↗` can never name different pages.
            rowHref={(t) => peekHref({ kind: "tool", id: t.name })}
            onRowActivate={openTool}
            defaultSort="name"
            empty={{ title: `No tool matches “${q.trim()}”` }}
            columns={TOOL_COLUMNS}
          />
        </Card>
      )}

      <Section title="How a model reaches these">
        <Callout variant="neutral">
          <span className="col" style={{ gap: 4 }}>
            <span>
              An executor prompt lists <strong>server names</strong>, not tool names — a role with
              50–150 MCP tools would push 15–25 KB of catalogue into every call.
            </span>
            <span className="t-caption">
              The model calls <InlineCode>list_mcp_server_tools(server)</InlineCode> to see what a
              server offers, then <InlineCode>activate_tool(name)</InlineCode> to promote one into
              the schemas it can actually invoke. It activates what it needs the moment it needs it:
              there is no separate planning pass to name a tool in advance.
            </span>
          </span>
        </Callout>
      </Section>
    </>
  );
}
