/**
 * The pieces every part of Settings › MCP & tools shares: how a flow is
 * opened by the address, a server's mark and condition, a tool's risk, who a
 * server reaches, the last thing that went wrong, and a server's menu.
 */

import {
  Menu,
  RelativeTime,
  Select,
  StatusDot,
  TableTwoLineCell,
  Tag,
  Text,
  Tooltip,
} from "@crewlethq/ui";
import { VENDORS, VendorMark, type Vendor } from "@crewlethq/icons";
import {
  EllipsisGlyph,
  ServerGlyph,
  SquareTerminalGlyph,
  type GlyphSizeStep,
} from "@crewlethq/icons/glyphs";
import type {
  McpMemberServerNode,
  McpMemberServerStatus,
  McpServerNode,
  McpToolLevel,
  McpToolView,
} from "~/contract/mcp.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { listed, plural } from "~/lib/format.ts";
import type { McpData } from "./data.ts";
import { BASELINE, failureWords, LEVEL, LEVELS, RISK, STATE } from "./words.ts";

/**
 * The flows the page opens, by the `flow` parameter. `add` and `edit` are the
 * wizard (`step` names where an edit opens); `test` the Test dialog; `value`
 * the dialog that stores one header or variable (`key`); `signin` and
 * `revoke` a server's OAuth sign-in, made and given up; `approve` the calls
 * a server's tools hold for a person's answer; `drift` the review of the tools
 * a server changed.
 */
export type Flow =
  | "add"
  | "edit"
  | "test"
  | "value"
  | "disable"
  | "remove"
  | "restart"
  | "signin"
  | "revoke"
  | "approve"
  | "drift";

const FLOWS: readonly Flow[] = [
  "add",
  "edit",
  "test",
  "value",
  "disable",
  "remove",
  "restart",
  "signin",
  "revoke",
  "approve",
  "drift",
];

/** The open flow and its arguments, and the ways to open and close one. */
export function useFlow() {
  const [raw] = useParam("flow", "", "section");
  const [server] = useParam("server", "");
  const [seat] = useParam("seat", "");
  const [step] = useParam("step", "");
  const [key] = useParam("key", "");
  const nav = useNavigator();
  return {
    flow: (FLOWS as readonly string[]).includes(raw) ? (raw as Flow) : null,
    server,
    seat,
    step,
    key,
    open: (next: Flow, args: Record<string, string> = {}) => nav.filter({ flow: next, ...args }),
    close: () => nav.filter({ flow: null, server: null, seat: null, step: null, key: null }),
  };
}

/** Opens a flow on a server: from the list it names the server, on its page it is the page's. */
export type OnFlow = (flow: Flow, server?: string, args?: Record<string, string>) => void;

/**
 * A server's mark is drawn at a glyph step, never a number of its own: `xl`
 * (24px) in a row, a card or a page's head, as a model's harness mark is.
 */
type MarkStep = Extract<GlyphSizeStep, "md" | "lg" | "xl">;

const isVendor = (v: string): v is Vendor => (VENDORS as readonly string[]).includes(v);

/**
 * A server's mark: the catalogue's vetted vendor mark, else the kit's server
 * or terminal glyph at the same step, as the Models page draws a harness with
 * no mark. Never a hotlinked icon.
 *
 * ALWAYS IN ITS OWN BOX, decoration for assistive technology and in the
 * primary ink: a kit slot that styles a bare glyph (a list's avatar, which
 * greys it) leaves a mark alone.
 */
export function ServerMark({
  server,
  size = "xl",
}: {
  server: Pick<McpMemberServerStatus, "vendor" | "transport">;
  size?: MarkStep;
}) {
  return (
    <span className="mcp-mark" aria-hidden="true">
      {isVendor(server.vendor) ? (
        <VendorMark vendor={server.vendor} size={size} />
      ) : server.transport === "stdio" ? (
        <SquareTerminalGlyph size={size} />
      ) : (
        <ServerGlyph size={size} />
      )}
    </span>
  );
}

/**
 * A server's condition in a word, with how many of its instances run where
 * that is the news: the engine's state, and its counts beside it.
 */
export function StateLine({ server }: { server: McpMemberServerStatus }) {
  const words = STATE[server.state];
  const launched = server.started + server.failed + server.waiting;
  const detail =
    server.state === "partial"
      ? `${server.started} of ${launched}`
      : server.state === "running" && !server.shared && server.started > 1
        ? `${server.started} agents`
        : null;
  return (
    <span className="mcp-state">
      <StatusDot tone={words.tone}>{words.label}</StatusDot>
      {detail ? (
        <Text as="span" variant="caption" tone="secondary">
          {detail}
        </Text>
      ) : null}
    </span>
  );
}

/**
 * A tool's risk as the engine read its hints, labelled as the server's own
 * hint. `starred` marks a risk the server did not declare with an asterisk
 * (its tooltip says why), for a table's one line of chips; `compact` also
 * leaves out Open world.
 */
export function RiskChips({
  tool,
  compact = false,
  starred = compact,
}: {
  tool: Pick<McpToolView, "risk" | "risk_declared" | "external">;
  compact?: boolean;
  starred?: boolean;
}) {
  const words = RISK[tool.risk];
  const tip = tool.risk_declared
    ? "A hint the server provides."
    : "The server declares nothing, so the tool counts as destructive.";
  return (
    <span className="mcp-risk">
      <Tooltip content={tip} placement="top">
        <span>
          <Tag size="xs" variant={words.tone} appearance="soft">
            {words.label}
            {tool.risk_declared ? "" : starred ? "*" : ", not declared"}
          </Tag>
        </span>
      </Tooltip>
      {tool.external && !compact ? (
        <Tooltip
          content="A hint the server provides: it reaches outside systems, so what it returns may carry text from anybody."
          placement="top"
        >
          <span>
            <Tag size="xs" variant="neutral" appearance="outline">
              Open world
            </Tag>
          </span>
        </Tooltip>
      ) : null}
    </span>
  );
}

/**
 * Who a server reaches, at one height: the first agent's name and a "+N"
 * that never gives way, the count under them, every agent on hover (the
 * Models page's Used by cell). The reach is the engine's grant, pushed on
 * each agent seat.
 */
export function ReachCell({ server, data }: { server: McpMemberServerStatus; data: McpData }) {
  const reach = data.reaches(server.name) ?? [];
  if (reach.length === 0) {
    return (
      <Text as="span" variant="caption" tone="tertiary">
        No agent
      </Text>
    );
  }
  const every = reach.length === data.agents.length;
  const names = reach.map((s) => s.name);
  const lead = every ? "Every agent" : (names[0] ?? "");
  const more = every ? 0 : names.length - 1;
  return (
    <Tooltip
      content={
        <span className="mcp-tip">
          {names.map((n) => (
            <span key={n}>{n}</span>
          ))}
        </span>
      }
      placement="top"
    >
      <span className="mcp-reach" aria-label={`Reaches ${listed(names)}`}>
        <span className="mcp-reach__line">
          <Text variant="inherit" truncate titleWhenCut={false}>
            {lead}
          </Text>
          {more > 0 ? <span className="mcp-reach__more">+{more}</span> : null}
        </span>
        <Text as="span" variant="caption" tone="secondary" truncate titleWhenCut={false}>
          {plural(reach.length, "agent")}
        </Text>
      </span>
    </Tooltip>
  );
}

/** A server's tools in a figure, with how many are not offered under it. */
export function ToolsCell({ server }: { server: McpMemberServerStatus }) {
  if (server.tools === 0) {
    return (
      <Text as="span" variant="caption" tone="tertiary">
        None listed
      </Text>
    );
  }
  return (
    <span className="mcp-tools-cell">
      <span className="mcp-num">{server.tools}</span>
      {server.refused_count > 0 ? (
        <Text as="span" variant="caption" tone="secondary">
          {server.refused_count} not offered
        </Text>
      ) : null}
    </span>
  );
}

/**
 * The node cell that names what is wrong with a server: the longest down,
 * which the engine's heartbeat dates. Null where nothing is down.
 */
function failingCell<T extends McpMemberServerNode>(nodes: readonly T[]): T | null {
  const down = nodes.filter((n) => n.failed > 0 && n.down_since);
  down.sort((a, b) => (a.down_since ?? "").localeCompare(b.down_since ?? ""));
  return down[0] ?? null;
}

/**
 * The last thing that went wrong, in its class's few words; the engine's
 * sentence whole on hover where the reader is an administrator, who alone is
 * answered it. ONE REGISTER DOWN THE COLUMN: a healthy row's line is in the
 * cell's size, quieter.
 */
export function LastError({ server, data }: { server: McpMemberServerStatus; data: McpData }) {
  const row = data.adminRow(server.name);
  const cell = server.enabled
    ? row
      ? failingCell<McpServerNode>(row.nodes)
      : failingCell(server.nodes)
    : null;
  if (!cell) {
    return (
      <Text as="span" variant="cell" tone="secondary">
        None
      </Text>
    );
  }
  const said = "error" in cell ? (cell as McpServerNode).error : "";
  const seat = "error_seat" in cell ? (cell as McpServerNode).error_seat : "";
  const words = failureWords(cell.error_class, said);
  return (
    <Tooltip
      content={
        <span className="mcp-tip">
          <span>{words.title}</span>
          {said ? <span className="mono">{said}</span> : null}
        </span>
      }
      placement="top"
    >
      <a
        className="mcp-row-reveal"
        href={href(["settings", "mcp-tools", "servers", server.name])}
        tabIndex={-1}
        aria-hidden="true"
      >
        <TableTwoLineCell
          value={words.short}
          titleWhenCut={false}
          detail={
            seat ? (
              <>
                {data.seatName(seat)}, <RelativeTime value={cell.down_since ?? ""} />
              </>
            ) : (
              <RelativeTime value={cell.down_since ?? ""} />
            )
          }
        />
      </a>
    </Tooltip>
  );
}

/** Where a server runs and how many instances it has, in one line each. */
export function whereWords(
  server: Pick<McpMemberServerStatus, "transport" | "shared" | "configured">,
) {
  return {
    where: !server.configured ? "Not configured" : server.transport === "http" ? "Remote" : "Local",
    scope: server.shared ? "Company-shared" : "Per agent",
  };
}

/** A server's actions, as one menu: on its row and in its own head. Admin only. */
export function ServerMenu({
  server,
  onFlow,
  label = `Actions for ${server.title}`,
}: {
  server: McpMemberServerStatus;
  onFlow: OnFlow;
  label?: string;
}) {
  return (
    <Menu
      label={label}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="end"
      items={[
        ...(server.enabled
          ? [{ key: "test", label: "Test", onSelect: () => onFlow("test", server.name) }]
          : []),
        { key: "edit", label: "Edit", onSelect: () => onFlow("edit", server.name) },
        ...(server.enabled
          ? [{ key: "restart", label: "Restart", onSelect: () => onFlow("restart", server.name) }]
          : []),
        {
          key: "disable",
          label: server.enabled ? "Disable" : "Enable",
          onSelect: () => onFlow("disable", server.name),
        },
        { kind: "separator" as const, key: "sep" },
        {
          key: "remove",
          label: "Remove",
          danger: true,
          onSelect: () => onFlow("remove", server.name),
        },
      ]}
    />
  );
}

/**
 * A tool's level picker: Always allow, Needs approval or Blocked, and where
 * the tool has a level of its own, the way back to the server's default.
 */
export function LevelSelect({
  value,
  onChange,
  label,
  disabled,
  baseline,
  onDefault,
  held = false,
}: {
  value: McpToolLevel;
  onChange: (level: McpToolLevel) => void;
  label: string;
  disabled?: boolean;
  /** A change holds the tool at Needs approval until it is reviewed, whatever its level. */
  held?: boolean;
  /** The server's default (null: by each tool's hints), offered where the tool has a level of its own. */
  baseline?: McpToolLevel | null;
  onDefault?: () => void;
}) {
  return (
    <Select
      ariaLabel={label}
      size="sm"
      width="auto"
      value={value}
      disabled={disabled}
      options={[
        ...LEVELS.map((l) => ({
          value: l,
          label: LEVEL[l].label,
          ...(held && l === "allow" ? { description: "Once its change is reviewed" } : {}),
        })),
        ...(baseline !== undefined && onDefault
          ? [
              {
                value: "default",
                label: `Server default (${baseline ? LEVEL[baseline].label : BASELINE.hints.label.toLowerCase()})`,
              },
            ]
          : []),
      ]}
      onChange={(v) => (v === "default" ? onDefault?.() : onChange(v as McpToolLevel))}
    />
  );
}

/**
 * A server's default level: one of the three, or none, which starts each
 * tool the entry does not name at the level its hints give it.
 */
export function BaselineSelect({
  value,
  onChange,
  label,
  disabled,
}: {
  value: McpToolLevel | null;
  onChange: (level: McpToolLevel | null) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <Select
      ariaLabel={label}
      size="sm"
      width="auto"
      value={value ?? "hints"}
      disabled={disabled}
      options={[
        { value: "hints", label: BASELINE.hints.label, description: BASELINE.hints.detail },
        ...LEVELS.map((l) => ({ value: l, label: LEVEL[l].label })),
      ]}
      onChange={(v) => onChange(v === "hints" ? null : (v as McpToolLevel))}
    />
  );
}
