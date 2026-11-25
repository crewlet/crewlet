/**
 * The Fleets card: one table of parents and children, as the org chart's
 * Structure table is (the kit's `OrgTable`). Each fleet is a parent row (its
 * status and usage rolled up from its members, its strategy, who it serves
 * now, its Settings and Delete); its members are the child rows, each read as
 * the Models table reads its model. A Sequential fleet's members keep their
 * order handle, and Alt with Up or Down moves the focused one.
 */

import { useCallback, useMemo, type ReactNode } from "react";
import {
  Button,
  Card,
  IconButton,
  Menu,
  OrgTable,
  OrgTableActions,
  OrgTableAdd,
  OrgTableName,
  ReorderHandle,
  StatusDot,
  Tag,
  Text,
  VisuallyHidden,
  useReorderLists,
  type TreeGridColumn,
  type TreeGridContext,
  type TreeInput,
} from "@crewlethq/ui";
import { EllipsisGlyph, LayersGlyph, PlusGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import type { Fleet, FleetMember, FleetsAnswer } from "~/protocol/fleets.ts";
import type { ModelsData } from "../data.ts";
import { modelMark } from "../modelPicker.tsx";
import { isReady } from "~/lib/fleets.ts";
import { STRATEGY_WORDS } from "./data.ts";
import { FleetLimits, MemberLimits, MemberStatus, ServingFaces } from "./parts.tsx";

const COLUMNS: TreeGridColumn[] = [
  { key: "name", header: "Name", floor: "11rem", sortable: false },
  { key: "status", header: "Status", width: "6rem", drop: 5, size: "sm", sortable: false },
  {
    key: "strategy",
    header: "Utilization strategy",
    width: "9.75rem",
    align: "center",
    drop: 4,
    size: "sm",
    sortable: false,
  },
  // THE MODELS TABLE'S WIDTH, so the two tables' meters are one width.
  {
    key: "limits",
    header: "Usage limits",
    width: "var(--models-limits-width)",
    drop: 3,
    sortable: false,
  },
  { key: "serving", header: "Serving now", width: "6.625rem", drop: 2, sortable: false },
];

/** The actions column: a Sequential member's handle and trash at most. */
const ACTIONS_COLUMN: TreeGridColumn = {
  key: "actions",
  header: "Actions",
  action: true,
  sortable: false,
  width: "calc(var(--size-control-sm) * 2 + var(--spacing-1) + var(--spacing-3) * 2)",
};

type RowId =
  | { kind: "fleet"; fleet: string }
  | { kind: "member"; fleet: string; entry: string }
  | { kind: "empty"; fleet: string };

function parse(id: string): RowId {
  const [kind, fleet = "", entry = ""] = id.split("|");
  if (kind === "member") return { kind, fleet, entry };
  if (kind === "empty") return { kind, fleet };
  return { kind: "fleet", fleet };
}

/** A member's key in the reorder lists: its fleet and its entry. */
const orderKey = (fleet: string, entry: string) => `${fleet}|${entry}`;
const entryOf = (key: string) => key.split("|")[1] ?? key;

/** A fleet's status from its members: how many are ready, toned by how many; Empty with none. */
function fleetStatus(f: Fleet): {
  tone: "success" | "warning" | "danger" | "neutral";
  words: string;
} {
  if (f.members.length === 0) return { tone: "neutral", words: "Empty" };
  const ready = f.members.filter(isReady).length;
  const words = `${ready}/${f.members.length} ready`;
  if (ready === 0) return { tone: "danger", words };
  return { tone: ready < f.members.length ? "warning" : "success", words };
}

/** A member's model as the Models table names it (mono where that is its id), then its effort. */
function MemberModel({ entry, data }: { entry: string; data: ModelsData }) {
  const model = data.models.find((x) => x.key === entry);
  if (!model) return null;
  return (
    <Text
      as="span"
      variant="caption"
      tone="secondary"
      className="models-fleet-model"
      title={model.effort ? `${model.modelName} · ${model.effort}` : model.modelName}
    >
      <span className={model.modelName === model.model ? "mono" : undefined}>
        {model.modelName}
      </span>
      {model.effort ? ` · ${model.effort}` : null}
    </Text>
  );
}

export function FleetsTree({
  fleets,
  data,
  onSettings,
  onCreate,
  onDelete,
  onRemove,
  onReorder,
}: {
  fleets: FleetsAnswer;
  data: ModelsData;
  onSettings: (f: Fleet) => void;
  onCreate: () => void;
  onDelete: (f: Fleet) => void;
  onRemove: (f: Fleet, m: FleetMember) => void;
  onReorder: (f: Fleet, members: string[]) => void;
}) {
  const admin = data.admin;
  const list = fleets.fleets;
  const byKey = useMemo(() => new Map(list.map((f) => [f.key, f])), [list]);
  // SEQUENTIAL FLEETS' ORDERS, one list each: a member moves within its fleet only.
  const lists = useMemo(
    () =>
      Object.fromEntries(
        list
          .filter((f) => f.strategy === "sequential")
          .map((f) => [f.key, f.members.map((m) => orderKey(f.key, m.entry))]),
      ),
    [list],
  );
  const reorder = useReorderLists<string>({
    lists,
    accepts: (key, into) => key.startsWith(`${into}|`),
    nameOf: entryOf,
    labels: {
      hint: "Drag a member by its handle, or press Alt with Up or Down on its row, to change the order.",
    },
    onReorder: (next) => {
      for (const [key, keys] of Object.entries(next)) {
        const f = byKey.get(key);
        if (f && keys.join() !== (lists[key] ?? []).join()) onReorder(f, keys.map(entryOf));
      }
    },
  });
  const shown = reorder.lists;
  const rows: TreeInput[] = useMemo(
    () =>
      list.map((f) => {
        const entries =
          f.strategy === "sequential"
            ? (shown[f.key] ?? []).map(entryOf)
            : f.members.map((m) => m.entry);
        return {
          id: `fleet|${f.key}`,
          label: f.name,
          children:
            entries.length > 0
              ? entries.map((e) => ({ id: `member|${f.key}|${e}`, label: e }))
              : [{ id: `empty|${f.key}`, label: "No members yet" }],
        };
      }),
    [list, shown],
  );

  const cell = useCallback(
    (id: string, column: number, grid: TreeGridContext): ReactNode => {
      const row = parse(id);
      const f = byKey.get(row.fleet);
      if (!f) return null;
      const key = grid.columnKey(column);
      if (row.kind === "empty")
        return key === "name" ? (
          <Text as="span" variant="caption" tone="tertiary">
            No members yet
          </Text>
        ) : null;
      if (row.kind === "fleet") {
        switch (key) {
          case "name":
            return (
              <OrgTableName
                icon={<LayersGlyph size="sm" />}
                name={f.name}
                {...(admin
                  ? {
                      nameAction: (
                        <OrgTableAdd
                          placement="name"
                          face={false}
                          label={`Add members to ${f.name}`}
                          onAdd={() => onSettings(f)}
                        />
                      ),
                    }
                  : {})}
              />
            );
          case "status": {
            const s = fleetStatus(f);
            return (
              <span className="models-one-line">
                <StatusDot tone={s.tone}>{s.words}</StatusDot>
              </span>
            );
          }
          case "strategy":
            return (
              <span className="models-fleet-strategy">
                <Tag size="sm" variant="neutral">
                  {STRATEGY_WORDS[f.strategy].name}
                </Tag>
                {f.strategy === "sequential" || f.spread !== null ? (
                  <Text as="span" variant="caption" tone="tertiary">
                    {f.strategy === "sequential" ? "In order" : `Within ${f.spread} points`}
                  </Text>
                ) : null}
              </span>
            );
          case "limits":
            return <FleetLimits fleet={f} data={data} />;
          case "serving":
            return (
              <ServingFaces
                handles={f.members.flatMap((m) => m.serving)}
                unlisted={f.members.reduce((n, m) => n + m.unlisted_serving, 0)}
                data={data}
              />
            );
          // The default fleet is never deleted, so its menu holds Settings alone.
          case "actions":
            return (
              <OrgTableActions shown slots={2}>
                <Menu
                  label="More actions"
                  align="end"
                  activeDescendant
                  entrance="instant"
                  renderTrigger={(props) => (
                    <IconButton
                      {...props}
                      label={`Actions for ${f.name}`}
                      icon={<EllipsisGlyph size="sm" />}
                      size="sm"
                      variant="ghost"
                    />
                  )}
                  items={[
                    { key: "settings", label: "Settings", onSelect: () => onSettings(f) },
                    ...(f.is_default
                      ? []
                      : [
                          { kind: "separator" as const, key: "sep" },
                          {
                            key: "delete",
                            label: "Delete fleet",
                            danger: true,
                            onSelect: () => onDelete(f),
                          },
                        ]),
                  ]}
                />
              </OrgTableActions>
            );
          default:
            return null;
        }
      }
      const m = f.members.find((x) => x.entry === row.entry);
      if (!m) return null;
      const sequential = admin && f.strategy === "sequential";
      const k = orderKey(f.key, m.entry);
      switch (key) {
        case "name": {
          const item = sequential ? reorder.itemProps(k) : null;
          const model = data.models.find((x) => x.key === m.entry);
          return (
            <span
              className="models-entry-cell"
              {...(item
                ? {
                    ...item,
                    // THE WHOLE ROW GLIDES: the kit animates the element it is handed.
                    ref: (node: HTMLElement | null) =>
                      item.ref(node?.closest<HTMLElement>('[role="row"]') ?? null),
                  }
                : {})}
            >
              <OrgTableName
                icon={model ? modelMark(model) : undefined}
                name={<span className="mono">{m.entry}</span>}
                caption={<MemberModel entry={m.entry} data={data} />}
              />
            </span>
          );
        }
        case "status":
          return <MemberStatus entry={m.entry} data={data} />;
        case "limits":
          return <MemberLimits entry={m.entry} data={data} />;
        case "serving":
          return <ServingFaces handles={m.serving} unlisted={m.unlisted_serving} data={data} />;
        case "actions":
          return (
            <OrgTableActions shown slots={2}>
              {sequential ? <ReorderHandle {...reorder.handleProps(k)} pointerOnly /> : null}
              <IconButton
                label={`Remove ${m.entry} from ${f.name}`}
                icon={<TrashGlyph size="sm" />}
                size="sm"
                variant="ghost"
                onClick={() => onRemove(f, m)}
              />
            </OrgTableActions>
          );
        default:
          return null;
      }
    },
    [admin, byKey, data, onDelete, onRemove, onSettings, reorder],
  );

  return (
    <Card as="section">
      <Card.Header
        icon={<LayersGlyph size="sm" />}
        subtitle="Seats on a fleet run on whichever member has room."
        subtitlePlacement="below"
        {...(admin
          ? {
              actions: (
                <Button
                  leadingIcon={<PlusGlyph size="sm" />}
                  variant="primary"
                  size="small"
                  onClick={onCreate}
                >
                  Create fleet
                </Button>
              ),
            }
          : {})}
      >
        <Card.Title>Fleets</Card.Title>
      </Card.Header>
      <Card.Body padding="none">
        <VisuallyHidden id={reorder.hintId}>{reorder.hint}</VisuallyHidden>
        <OrgTable
          label="Fleets and their members"
          layout="container"
          phoneRows="labelled"
          flush
          controls={false}
          columns={admin ? [...COLUMNS, ACTIONS_COLUMN] : COLUMNS}
          rows={rows}
          renderCell={cell}
          onRowKeyDown={(id, event) => {
            if (!event.altKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown"))
              return false;
            const row = parse(id);
            if (row.kind !== "member" || !admin) return false;
            if (byKey.get(row.fleet)?.strategy !== "sequential") return false;
            reorder.move(orderKey(row.fleet, row.entry), event.key === "ArrowUp" ? -1 : 1);
            return true;
          }}
          cellHasControl={(id, _column, key) => {
            const row = parse(id);
            if (key === "actions") return true;
            if (key === "name") return admin && row.kind === "fleet";
            return key === "limits" && row.kind === "member";
          }}
        />
      </Card.Body>
    </Card>
  );
}
