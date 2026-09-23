/**
 * Every tool a seat can call, grouped by where it came from.
 *
 * The origin grammar is two-valued — `builtin`, `mcp:<server>`, `a2a` — and it
 * is recorded at registration, which is the only frame that knows and the last
 * that can say.
 */

import { useCallback, useMemo } from "react";
import { plural } from "~/lib/format.ts";
import { useNavigator, useParam, useRoute } from "~/app/router.tsx";
import {
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
  BuildGlyph,
  CableGlyph,
  Package2Glyph,
  SearchGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState, RECORD_MAX_HEIGHT, Section, SeatChip } from "~/components/common.tsx";
import { uiletTone } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { NumberCell, KeyCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useQuery } from "~/lib/useQuery.ts";
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
import type { ConfigUnit, ToolAnnotations, ToolHint, ToolRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

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
 * THREE DIFFERENT RULES, which is why this is a value rather than a list:
 *
 *   - a builtin and an A2A tool are registered on every agent seat the engine
 *     spawns, so naming two hundred of them says less than saying so;
 *   - a SHARED MCP server is one instance serving the company, so its tools
 *     are every agent seat's too;
 *   - a server with `shared: false` is a TEMPLATE — `config.MCPServer`'s own
 *     words — and an instance is launched only for a seat that declares
 *     credentials for it under `mcp_env`. There the credential IS the grant,
 *     and the seats can be named exactly.
 *
 * `shared` is three-valued for the reason everything in this product is: the
 * active configuration may not have been readable, and a tool whose holders
 * are UNKNOWN must not be drawn as a tool nobody holds.
 */
interface Holders {
  /** Named seats, where the grant is a credential and can be enumerated. */
  seats: Seat[];
  /** Every agent seat, where the tool is registered on all of them. */
  everyone: boolean;
  /** One sentence naming the rule this answer came from. */
  why: string;
}

function holdersOf(
  tool: ToolRow,
  seats: Seat[],
  shared: boolean | null,
  /** Which seats declare credentials for a per-seat server, or null when the
   *  company document could not be read. */
  holdersByServer: Map<string, Set<string>> | null,
): Holders | null {
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
  // THE CONFIGURATION DID NOT ANSWER. Not "nobody holds it" — see the type's
  // own comment — so the panel says which question went unanswered instead of
  // drawing an empty list somebody would act on.
  if (shared === null) return null;
  if (shared) {
    return {
      seats: agents,
      everyone: true,
      why: `${server} is a shared server: one instance serves the company, so every agent seat can call its tools.`,
    };
  }
  // AND THE CREDENTIALS ARE GUARDED TOO. `mcp_env` is not on the anonymous org
  // projection — the classification in `internal/api/orgprojection_test.go`
  // calls it "tool credentials inherited by members" — so which seats declare
  // one is a question the company document answers and an anonymous reader
  // cannot. Unanswered is null, exactly as an unread `shared` is, rather than
  // an empty list somebody would act on.
  if (!holdersByServer) return null;
  const declared = holdersByServer.get(server) ?? new Set<string>();
  return {
    seats: agents.filter((s) => declared.has(s.name)),
    everyone: false,
    why: `${server} is a per-seat template, so an instance is launched only for a seat that declares credentials for it under mcp_env.`,
  };
}

/**
 * Whether an `mcp_servers` entry is shared.
 *
 * UNSET IS SHARED, and unset is what the wire carries for it: `config.Toggle`
 * omits an untouched toggle rather than writing today's default into the
 * document, so the field is absent on every server nobody said anything
 * about — and reading absent as `false` would report the ordinary company
 * server as a per-seat template held by nobody.
 */
function sharedFlag(entity: unknown): boolean {
  if (!entity || typeof entity !== "object") return true;
  const value = (entity as Record<string, unknown>)["shared"];
  return typeof value === "boolean" ? value : true;
}

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
 * so every surface that only needs to know WHICH tool this is — a header, a
 * peek deciding whether it has one at all — costs no request. The one question
 * a tool raises that the catalogue cannot answer is [useServerSharing]'s, and
 * it is asked where it is read.
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
 * Whether one MCP server's template is SHARED, from the active configuration.
 *
 * THE HALF THE CATALOGUE CANNOT CARRY — a registration records what a tool IS,
 * not who was given it — and it is what decides whether a tool belongs to
 * every agent seat or to the handful that hold credentials for its server.
 *
 * SEPARATE FROM [useTool] BECAUSE ONLY [ToolBody] READS IT. `useQuery` has no
 * cache and no in-flight dedupe — each instance mints its own frame — so while
 * this sat inside the resolution hook, every addressed tool asked the engine
 * for the same entity twice: once for the header, which throws the answer
 * away, and once for the body, on the page and in the rail alike, again on
 * every `[`/`]` step through the peek's neighbours. A builtin or an A2A tool
 * asks nothing at all: it has no server to ask about.
 */
function useServerSharing(server: string): {
  /** Null until the configuration answers, which is not the same as false. */
  shared: boolean | null;
  entity: { loading: boolean; error: string | null };
} {
  const entity = useQuery(
    "config_entities",
    { kind: "mcp-servers", id: server },
    { enabled: server !== "" },
  );
  return {
    shared: entity.data?.entity ? sharedFlag(entity.data.entity) : null,
    entity: { loading: entity.loading, error: entity.error },
  };
}

/**
 * Which seats declare credentials for a per-seat MCP server, by SEAT NAME.
 *
 * THE COMPANY DOCUMENT ANSWERS THIS AND THE PROJECTION CANNOT. `mcp_env` is
 * guarded — it holds tool credentials — so the anonymous org projection
 * carries none of it, and a reader without an operator token simply does not
 * know which seats a per-seat server is launched for. Null says exactly that,
 * for the same reason an unread `shared` is null: an empty list here reads as
 * "nobody holds this tool", which is a claim about somebody's company.
 *
 * A unit's own `mcp_env` counts, because its direct AGENT members inherit it —
 * that is `org.MCPEnv`'s rule and the merge `lib/seats.ts` performs for the
 * seat screen. A human member inherits none: it runs no tools.
 *
 * Read only for a server, because a builtin and an A2A tool have none to ask
 * about — the same reason [useServerSharing] is gated the same way.
 */
function useServerHolders(server: string): Map<string, Set<string>> | null {
  const doc = useQuery("config", undefined, { enabled: server !== "" });
  return useMemo(() => {
    if (server === "" || doc.error || !doc.data) return null;
    const out = new Map<string, Set<string>>();
    const add = (seat: string, env: Record<string, Record<string, string>> | undefined) => {
      for (const name of Object.keys(env ?? {})) {
        const held = out.get(name) ?? new Set<string>();
        held.add(seat);
        out.set(name, held);
      }
    };
    const visit = (unit: ConfigUnit): void => {
      for (const role of unit.roles ?? []) {
        add(role.name, role.mcp_env);
        if (role.kind !== "human") add(role.name, unit.mcp_env);
      }
      for (const child of unit.children ?? []) visit(child);
    };
    for (const role of doc.data.roles ?? []) add(role.name, role.mcp_env);
    for (const unit of doc.data.units ?? []) visit(unit);
    return out;
  }, [server, doc.data, doc.error]);
}

/**
 * What a tool IS, under whichever header named it.
 *
 * ONE BODY FOR THE PAGE AND THE RAIL. `#/admin/tools/{name}` is a tool's
 * address in `objects.ts` and therefore where the rail's `Open ↗` goes — the
 * screen accepted the segment and rendered the unfiltered catalogue, so that
 * link led nowhere in particular and a reader who followed it lost the tool
 * they were reading. Shared rather than duplicated because the two surfaces
 * answer the same question and a second copy would answer it differently
 * within a release.
 */
function ToolBody({ name }: { name: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const { matches, tool, server, cold } = useTool(name);
  const { shared, entity } = useServerSharing(server);
  const holdersByServer = useServerHolders(server);

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
        icon={<BuildGlyph size="xl" />}
        title={`No tool called “${name}”`}
        description="Builtins register at boot and MCP tools are discovered from the servers in mcp_servers. A tool whose server failed to start, or whose name has changed, is not in this node's registry."
      />
    );
  }

  const fields = schemaFields(tool);
  const holders = holdersOf(tool, index.seats, shared, holdersByServer);

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
                    code: true,
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
        {entity.loading && <Skeleton variant="text" rows={2} label="Loading" />}
        {/* THE REFUSAL GOES WHERE THE ANSWER WOULD HAVE BEEN, and nowhere
            else: the tool's own facts came off a push and are still true, so
            a configuration read that failed must not blank them. */}
        <QueryState error={entity.error} loading={entity.loading}>
          {holders === null ? (
            <p className="t-body muted">
              Who holds this depends on whether <span className="mono">{server}</span> is shared,
              and the active configuration did not answer.
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
                No seat can call this: nothing declares credentials for{" "}
                <span className="mono">{server}</span>, so the template launches nowhere.
              </p>
              <p className="t-caption">{holders.why}</p>
            </>
          )}
        </QueryState>
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
  if (!tool) return null;
  const capability = capabilityOf(tool.annotations);
  return (
    <ObjectHeader
      kind="Tool"
      icon="build"
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
 * # The one thing it does ask
 *
 * Whether the tool's server is SHARED, asked ONCE, by [ToolBody] — see
 * [useServerSharing] for why the header half of this rail must not ask it too.
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
        icon="build"
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

export function Tools({ server, tool }: { server?: string; tool?: string }) {
  const tools = useTools();
  const route = useRoute();
  const nav = useNavigator();
  const [q, setQ] = useParam("q", "");
  const [chosen, setChosen] = useParam("origin", "");

  // `#/admin/tools/servers/{name}` IS A FILTER ON THE ORIGIN, and this screen
  // accepted the segment and dropped it: the crumb trail said "Servers /
  // github" over the whole unfiltered catalogue, and `Open ↗` from a tool's
  // rail landed on the same place. The path wins where it names one, and the
  // chips write the query key as before.
  const addressed = server ? `mcp:${server}` : "";
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
        nav.replace(["admin", "tools"], query);
        return;
      }
      setChosen(next);
    },
    [addressed, nav, route.query, setChosen],
  );

  const origins = useMemo(() => {
    const map = new Map<string, number>();
    for (const t of tools) map.set(t.source, (map.get(t.source) ?? 0) + 1);
    return [...map.entries()].sort((a, b) => b[1] - a[1]);
  }, [tools]);

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
        {<Tag appearance="outline">{plural(tools.length, "tool")} registered</Tag>}
      </PageActions>
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

      <Card padding="none">
        <StatGroup columns={4}>
          <StatCard
            icon={<BuildGlyph size="xs" />}
            label="Total"
            value={tools.length}
            sub="across every origin"
          />
          <StatCard
            icon={<Package2Glyph size="xs" />}
            label="Built in"
            value={builtins}
            sub="shipped by the engine itself"
          />
          <StatCard
            icon={<CableGlyph size="xs" />}
            label="From MCP servers"
            value={mcp}
            sub={`${origins.filter(([s]) => s.startsWith("mcp")).length} server(s)`}
          />
          <StatCard
            icon={<WarningGlyph size="xs" />}
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

      <div className="toolbar">
        <div style={{ maxWidth: 340, flex: 1 }}>
          <Input
            type="search"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            aria-label="Search tools"
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

      {!tools.length ? (
        <EmptyState
          icon={<BuildGlyph size="xl" />}
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
            empty={{ title: `No tool matches “${q}”` }}
            columns={[
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
                sortValue: (t) => t.name,
                cell: (t) => <KeyCell value={t.name} />,
              },
              {
                key: "origin",
                header: "Origin",
                shrink: true,
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
                cell: (t) => (
                  <span className="col" style={{ gap: 2 }}>
                    <span className="t-caption">
                      {t.description || <span className="muted">no description</span>}
                    </span>
                    <span className="t-caption">{hintSentence(t.annotations)}</span>
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
                      {t.annotations?.open_world === "yes" && (
                        <Tag appearance="outline">outside</Tag>
                      )}
                    </span>
                  );
                },
              },
              {
                key: "delivers",
                header: "Delivers to",
                shrink: true,
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
                                .map(
                                  (f) =>
                                    `${f.name}${f.required ? "*" : ""}: ${f.type || "type not stated"}`,
                                )
                                .join("\n")
                            : undefined
                        }
                      />
                      {required > 0 && <span className="t-caption">{required} required</span>}
                    </span>
                  );
                },
              },
            ]}
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
