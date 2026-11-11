/**
 * A server's Tools: every tool its instances list across the fleet, each with
 * the risk the engine read from its hints, how often it was called this week
 * and the level each call is decided at; the server's default for every tool
 * without a level of its own; and the tools no agent is offered under their
 * names, with the engine's reason.
 *
 * AVAILABILITY AND APPROVAL ARE ONE CHOICE HERE, by design: Blocked is "not
 * offered", Needs approval parks the run on a person's answer, Always allow
 * runs. The hints only chose where an undecided tool began; they never loosen
 * a level somebody set. A team's lead may hold a tool back further for their
 * own teams (`mcp_limits`); the engine resolves who it holds and how, per
 * agent (`reaches[].limited`), and the row says so. A tool whose definition changed asks first until the
 * change is reviewed (the engine's `held`), and the Drift dialog reviews it.
 *
 * THE ENGINE DECIDES EACH LEVEL. A tool's `level` and whether the entry names
 * it (`custom`) are the engine's reading of the entry and the tool's hints, and
 * the risk is its reading of the hints (a tool that declares nothing counts as
 * destructive). The page sends a change by the server's own name for the tool
 * (`raw`) and draws what comes back.
 */

import { useMemo, useState } from "react";
import {
  AttentionList,
  Button,
  Card,
  FilterChip,
  FilterChipGroup,
  Input,
  Menu,
  TableTextCell,
  TableTwoLineCell,
  Tag,
  Text,
  Tooltip,
  useToast,
  type DataGridColumn,
} from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  SearchGlyph,
  TriangleAlertGlyph,
  WrenchGlyph,
} from "@crewlethq/icons/glyphs";
import type { McpMemberServerStatus, McpRisk, McpToolView } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useParam } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { sortGridRows, useGridPage } from "~/lib/gridPage.ts";
import { setLevels, type LevelsChange, type ServerDetail } from "../api.ts";
import { BaselineSelect, LevelSelect, RiskChips, type OnFlow } from "../parts.tsx";
import { BASELINE, LEVEL, LEVELS, refusedWords, RISK } from "../words.ts";

type Filter = "all" | McpRisk | "changed" | "custom";

/** A tool's name in its cell: clamped to the column, whole on hover. */
export function ToolName({ name }: { name: string }) {
  return (
    <Tooltip content={<span className="mono">{name}</span>} placement="top">
      <Text as="span" variant="inherit" mono truncate titleWhenCut={false}>
        {name}
      </Text>
    </Tooltip>
  );
}

export function ToolsTab({
  server,
  detail,
  onChanged,
  onFlow,
}: {
  server: McpMemberServerStatus;
  detail: ServerDetail;
  /** Read the server again, after a level was written. */
  onChanged: () => void;
  onFlow: OnFlow;
}) {
  const toast = useToast();
  const admin = detail.reading === "admin" && server.configured;
  const [q, setQ] = useParam("q", "");
  const [filter, setFilter] = useState<Filter>("all");
  const [busy, setBusy] = useState(false);
  const calls = useMemo(
    () => new Map(detail.usage.tools.map((u) => [u.tool, u.calls])),
    [detail.usage.tools],
  );
  const matching = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return detail.tools.filter(
      (t) => !needle || `${t.name} ${t.title} ${t.description}`.toLowerCase().includes(needle),
    );
  }, [detail.tools, q]);
  const changes = detail.reading === "admin" ? detail.changes : [];
  const org = useOrg();
  // WHO HOLDS EACH TOOL BACK, from the engine's resolved limits per agent:
  // the team whose limit does it, or the agent whose own block does.
  const heldBack = useMemo(() => {
    const index = indexOrg(org);
    const out = new Map<string, Map<string, string>>();
    if (detail.reading !== "admin") return out;
    for (const r of detail.reaches) {
      for (const t of r.limited?.tools ?? []) {
        const who = t.by
          ? (index.units.find((u) => u.id === t.by)?.name ?? t.by)
          : (index.byHandle.get(r.seat)?.name ?? r.seat);
        const holders = out.get(t.tool) ?? new Map<string, string>();
        holders.set(who, LEVEL[t.level].label);
        out.set(t.tool, holders);
      }
    }
    return out;
  }, [detail, org]);
  const changeOf = new Map(changes.map((c) => [c.tool, c]));
  const changedTool = (t: McpToolView) => t.held || t.removed || changeOf.has(t.raw);
  const keep = (f: Filter) => (t: McpToolView) =>
    f === "all"
      ? true
      : f === "custom"
        ? t.custom
        : f === "changed"
          ? changedTool(t)
          : t.risk === f;
  const shown = matching.filter(keep(filter));
  const count = (f: Filter) => matching.filter(keep(f)).length;
  const filters: readonly [Filter, string][] = [
    ["all", "All"],
    ["read_only", RISK.read_only.label],
    ["additive", RISK.additive.label],
    ["destructive", RISK.destructive.label],
    ["changed", "Changed"],
    ["custom", "Custom"],
  ];
  const refused = server.refused_tools;
  const unlisted = detail.reading === "admin" ? detail.server.unlisted_annotations : [];
  const unlistedLevels = detail.reading === "admin" ? detail.unlisted_levels : [];

  /** Write a change of levels, said as `said` in the revision and the toast. */
  const write = async (change: LevelsChange, said: string) => {
    setBusy(true);
    try {
      const answer = await setLevels(server.name, change, said);
      if (answer.kind === "ok") {
        toast.show({ variant: "success", title: said });
        onChanged();
      } else {
        toast.show({
          variant: "danger",
          title: "Nothing was changed",
          message: refusedWords(answer),
        });
      }
    } finally {
      setBusy(false);
    }
  };

  // THE TOOL TAKES THE WIDTH, as a model's name does on Models: risk, calls
  // and level are content-sized, so a description is not cut at a quarter of
  // the table while three-digit counts sit in a column as wide.
  const columns: DataGridColumn<McpToolView>[] = [
    {
      key: "tool",
      header: "Tool",
      floor: "18rem",
      sortValue: (t) => t.name,
      cell: (t) => {
        const change = changeOf.get(t.raw);
        const mark = t.removed
          ? { label: "Removed", variant: "neutral" as const }
          : change?.kinds.includes("added")
            ? { label: "New", variant: "warning" as const }
            : t.held || change
              ? { label: "Changed", variant: "warning" as const }
              : null;
        const holders = heldBack.get(t.raw);
        return (
          <TableTwoLineCell
            value={
              <span className="mcp-tool-name">
                <ToolName name={t.name} />
                {mark ? (
                  <Tag size="xs" variant={mark.variant} appearance="soft">
                    {mark.label}
                  </Tag>
                ) : null}
                {holders ? (
                  <Tooltip
                    content={[...holders].map(([who, level]) => `${level} for ${who}`).join("; ")}
                    placement="top"
                  >
                    <span>
                      <Tag size="xs" variant="neutral" appearance="outline">
                        Held back
                      </Tag>
                    </span>
                  </Tooltip>
                ) : null}
              </span>
            }
            detail={t.description || t.title || undefined}
            titleWhenCut={false}
          />
        );
      },
    },
    {
      key: "risk",
      // The head says what the column is; each chip's tooltip says it is the
      // server's own hint.
      header: "Risk",
      shrink: true,
      sortValue: (t) => ["read_only", "additive", "destructive"].indexOf(t.risk),
      cell: (t) => <RiskChips tool={t} />,
    },
    {
      key: "calls",
      header: "Calls, 7 days",
      align: "right",
      shrink: true,
      sortValue: (t) => calls.get(t.name) ?? 0,
      // A zero in the column's own size, quieter: a 12px word among 13px
      // figures stood on another baseline.
      cell: (t) => {
        const n = calls.get(t.name) ?? 0;
        return (
          <Text as="span" variant="cell" tone={n > 0 ? "primary" : "tertiary"} className="mcp-num">
            {n}
          </Text>
        );
      },
    },
    {
      key: "level",
      header: "Level",
      // AS WIDE AS ITS SELECT AND ITS CUSTOM MARK: a content-sized column is
      // held to a fifth of the grid, which cut the mark off at the edge.
      width: "max-content",
      sortValue: (t) => LEVELS.indexOf(t.level),
      cell: (t) =>
        admin && detail.reading === "admin" ? (
          <span className="mcp-level-cell">
            <LevelSelect
              label={`Level for ${t.name}`}
              value={t.level}
              disabled={busy}
              held={t.held}
              onChange={(level) =>
                void write({ levels: { [t.raw]: level } }, `${t.name} set to ${LEVEL[level].label}`)
              }
              {...(t.custom
                ? {
                    baseline: detail.baseline,
                    onDefault: () =>
                      void write(
                        { levels: { [t.raw]: null } },
                        `${t.name} follows the server's default`,
                      ),
                  }
                : {})}
            />
            {t.custom ? (
              <Tag size="xs" variant="neutral" appearance="outline">
                Custom
              </Tag>
            ) : null}
          </span>
        ) : (
          // THE LEVEL IN WORDS, as the select draws it: a chip in the level's
          // tone was the risk chip's green beside it, and read as a second
          // kind of risk rather than as a setting.
          <span>{LEVEL[t.level].label}</span>
        ),
    },
  ];
  // A SERVER LISTING HUNDREDS OF TOOLS is drawn a page at a time, the whole
  // list sorted first so a page reads as part of one order.
  const [sort] = useParam("sort.tools", "");
  const page = useGridPage(sortGridRows(shown, columns, sort), [q, filter, sort].join("\n"));

  return (
    <div className="mcp-stack">
      {admin && changes.length > 0 ? (
        <AttentionList
          items={[
            {
              key: "changed",
              tone: "warning",
              title: `${plural(changes.length, "tool")} changed since ${changes.length === 1 ? "its level was" : "their levels were"} decided`,
              meta: "A changed tool asks first until somebody reviews the change",
              action: (
                <Button size="small" variant="secondary" onClick={() => onFlow("drift")}>
                  Review
                </Button>
              ),
            },
          ]}
        />
      ) : null}
      {refused.length > 0 ? (
        <Card padding="none">
          <Card.Header icon={<TriangleAlertGlyph size="sm" />} count={server.refused_count}>
            <Card.Title>Not offered</Card.Title>
          </Card.Header>
          <DataGrid
            name="refused"
            rows={[...refused]}
            rowKey={(t) => t.tool}
            defaultSort="tool"
            columns={[
              {
                key: "tool",
                header: "Tool",
                shrink: true,
                sortValue: (t) => t.tool,
                cell: (t) => (
                  <TableTextCell>
                    <span className="mono">{t.tool}</span>
                  </TableTextCell>
                ),
              },
              {
                key: "why",
                header: "Why it is not offered",
                // Prose quoting a tool's whole name, which has nowhere to break.
                overflow: "wrap",
                cell: (t) => (
                  <Text as="span" variant="body" tone="secondary">
                    {t.message}
                  </Text>
                ),
              },
            ]}
          />
        </Card>
      ) : null}
      {admin && detail.reading === "admin" ? (
        <Card padding="none">
          <Card.Header
            icon={<WrenchGlyph size="sm" />}
            actions={
              <Button
                size="small"
                variant="secondary"
                onClick={() => onFlow("edit", undefined, { step: "approvals" })}
              >
                Edit
              </Button>
            }
          >
            <Card.Title>Default for this server</Card.Title>
          </Card.Header>
          <Card.Body>
            <div className="mcp-baseline">
              <BaselineSelect
                label="Default level"
                value={detail.baseline}
                disabled={busy}
                onChange={(level) =>
                  void write(
                    { default: level },
                    level
                      ? `${server.title}'s default set to ${LEVEL[level].label}`
                      : `${server.title}'s tools start by their hints`,
                  )
                }
              />
              <Text as="p" variant="caption" tone="secondary">
                For every tool without its own level, and tools added later.
              </Text>
            </div>
            {unlistedLevels.length > 0 ? (
              <Text as="p" variant="caption" tone="tertiary">
                {unlistedLevels.length === 1
                  ? "One level names a tool the server does not list, so it decides nothing: "
                  : `${unlistedLevels.length} levels name tools the server does not list, so they decide nothing: `}
                <span className="mono">{unlistedLevels.join(", ")}</span>
              </Text>
            ) : null}
          </Card.Body>
        </Card>
      ) : null}
      {detail.tools.length === 0 ? (
        <Card padding="none">
          <Card.Body>
            <Text as="p" variant="body" tone="secondary">
              {!server.enabled
                ? "A disabled server lists no tools."
                : server.state === "needs_sign_in"
                  ? `${server.title} lists its tools once somebody signs in. ${BASELINE.hints.detail}`
                  : "The server lists no tools yet. They appear once an instance starts and lists them."}
            </Text>
          </Card.Body>
        </Card>
      ) : (
        <Card padding="none">
          {/* THE SHIPPED TOOLS PAGE'S TOOLBAR: a search field and the kit's
              filter chips, one 26px step, the chosen chip's count in its own
              ink. */}
          <div className="mcp-toolbar">
            <span className="mcp-toolbar__search">
              <Input
                type="search"
                aria-label="Search tools"
                placeholder="Search tools"
                inputSize="sm"
                width="full"
                leading={<SearchGlyph size="sm" />}
                value={q}
                onChange={(e) => setQ(e.target.value)}
              />
            </span>
            <FilterChipGroup
              label="Show"
              hideLabel
              semantics="radio"
              value={filter}
              onValueChange={(next) => setFilter((next ?? "all") as Filter)}
            >
              {filters
                .filter(([v]) => v === "all" || count(v) > 0)
                .map(([value, label]) => (
                  <FilterChip key={value} value={value} count={count(value)}>
                    {label}
                  </FilterChip>
                ))}
            </FilterChipGroup>
            {admin ? (
              <Menu
                label={`Set ${plural(shown.length, "tool")}`}
                trigger={`Set ${plural(shown.length, "tool")}`}
                trailingIcon={<ChevronDownGlyph size="sm" />}
                triggerVariant="secondary"
                align="end"
                items={LEVELS.map((l) => ({
                  key: l,
                  label: LEVEL[l].label,
                  disabled: shown.length === 0 || busy,
                  onSelect: () =>
                    void write(
                      { levels: Object.fromEntries(shown.map((t) => [t.raw, l])) },
                      `${plural(shown.length, "tool")} set to ${LEVEL[l].label}`,
                    ),
                }))}
              />
            ) : null}
          </div>
          <DataGrid<McpToolView>
            name="tools"
            rows={page.rows}
            rowKey={(t) => t.name}
            columns={columns}
            serverSorted
            empty={{ title: "No tool matches", icon: "search" }}
            onLoadMore={page.onLoadMore}
            loadedNote={page.loadedNote}
          />
          {server.shadowed_tools.length > 0 ? (
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              <Tag size="xs" variant="neutral" appearance="outline">
                Shadowed
              </Tag>{" "}
              {server.shadowed_tools.map((t) => t.message).join(" ")}
            </Text>
          ) : null}
          {unlisted.length > 0 ? (
            // AN OVERRIDE THAT MATCHES NOTHING is configuration only an
            // administrator reads: a renamed tool, or a typo, whose hint
            // overrides nothing anywhere.
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              {unlisted.length === 1
                ? "One tool_annotations key matches no tool any instance lists: "
                : `${unlisted.length} tool_annotations keys match no tool any instance lists: `}
              <span className="mono">{unlisted.join(", ")}</span>
            </Text>
          ) : null}
        </Card>
      )}
    </div>
  );
}
