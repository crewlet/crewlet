/**
 * Structure's Table: the org as an indented table of rows, where the running
 * company is both watched and changed.
 *
 * ONE ROW PER COMPANY, TEAM AND SEAT, IN THE CHART'S ORDER. A table of an
 * organization is the one table a sort destroys, so the rows are the tree, the
 * indent is where a node sits, and the wires down the gutter lead back to the
 * team a seat is in. The rows, cells, keys, wires, select boxes and row
 * controls are the design system's `OrgTable`; what is here is what the
 * engine knows about each row and what each action means.
 *
 * WHAT A SEAT IS DOING IS ON ITS ROW. Its Status is one calm line (a dot and
 * its state's word, then what waits on it as one quiet count), with the work
 * it is on, who waits and why it was paused in the line's tooltip; who it
 * Reports to is that seat, drawn as itself; its Problems are tags; and its
 * Controls are the engine's own (Pause or Resume, Pause and stop, Steer) with
 * the remedy its state asks for (Answer, Review call, Raise budget) leading.
 * A team's or the company's Status is the summary of the agents in it.
 *
 * AND WHAT CHANGES IT. Edit, Delete and the row's menu are drawn at rest, and
 * a team's add pill stands beside its name. Changing the org is a draft and a
 * Review and save; controlling a seat acts at once and asks first where it
 * ends work. The two never look alike: the edits are the Edit column's, the
 * run-time controls the Controls column's.
 *
 * A BOX BEFORE EACH NAME picks a seat, or every agent a team or the company
 * holds, for the selection bar's Pause, Resume and Pause and stop.
 *
 * COLUMNS FOLLOW THE TABLE'S OWN WIDTH, not the window's, so a seat panel
 * opened beside it narrows it as a window would: Reports to and Problems go
 * first, then Status, whose words move under the name; on a phone a row keeps
 * its name, its one control, and its menu, which holds the rest.
 *
 * MOVING A ROW AMONG ITS SIBLINGS is Alt with an arrow, and the same two
 * entries in the row's menu. The engine's primary manager of a seat is the
 * FIRST seat that lists it, so passing a sibling can change who a seat reports
 * to, which `reorder` announces when it happens.
 */

import { useCallback, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { CompassGlyph, PencilGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  EmptyValue,
  EntityChip,
  IconButton,
  Link,
  Menu,
  OrgTable,
  OrgTableActions,
  OrgTableAdd,
  OrgTableName,
  StatusDot,
  StatusLine,
  Tag,
  type MenuEntry,
  type TreeGridColumn,
  type TreeGridContext,
  type TreeInput,
  type TreeItemAction,
  type TreeViewHandle,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import {
  SeatControlButtons,
  SeatRemedy,
  SeatSelectionBar,
  controlHeld,
  hasRemedy,
  isRunning,
  useSeatControls,
  type SeatControls,
} from "~/components/seatControls.tsx";
import { useAdoptedMark } from "~/lib/brand/useBrand.ts";
import { plural } from "~/lib/format.ts";
import { waitingWords } from "~/lib/inboxWaiting.ts";
import {
  doingLine,
  matches,
  PAUSE_NOTE_WORDS,
  summaryOf,
  type SeatLive,
  type SeatProblem,
} from "~/lib/seatLive.ts";
import { seatLookup, seatPath, type OrgIndex } from "~/lib/seats.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import { faceOf } from "~/ui/SeatAvatar.tsx";
import { NodeGlyph, seatMark, unitTypeLabel } from "~/ui/orgNodes.tsx";
import { FilterEmpty, LimitBanner, StatusChips, useDrawnSeatCount } from "../structureParts.tsx";
import { liveHandleOf, useStatusFilter, useStructureLive } from "../structureLive.tsx";
import { addLabel } from "./AddNodeDialog.tsx";
import { useBuilder, useBuilderView, type BuilderApi } from "./BuilderContext.tsx";
import type { NodeView, SeatView } from "./chartModel.ts";
import type { NodeKey } from "./model/keys.ts";
import {
  addSections,
  editLabel,
  editsInPlace,
  heldEdits,
  isDeletable,
  leadLabel,
  rowMenu,
  type OpenScreen,
} from "./nodeActions.tsx";
import {
  handleLabel,
  managerLabel,
  nodeGlyphKind,
  nodeTone,
  SeatMarks,
  UnitMarks,
} from "./nodeMarks.tsx";
import { useClosedBranches } from "./chartPlace.ts";
import { useReorder, type Reorder } from "./reorder.ts";
import { useOpenScreen, useStructure } from "./useCharts.ts";

/**
 * The table's own widths, in CSS pixels, below which a column is left out:
 * Reports to first, then Problems (each still said in the row's panel), then
 * Status, whose words move under the name. Below the last a row folds its
 * edits and its Pause and stop and Steer into its menu.
 */
const WIDTHS = { lead: 980, problems: 900, state: 600, phone: 560 } as const;

/**
 * Why a control on a row refuses while the draft is read-only for a moment
 * (a save whose outcome is unknown, a conflict, a kept draft waiting): the
 * builder's own sentence for it. A reader who may not change the org is told
 * the gate's reason instead ([BuilderApi.editsHeld]).
 */
const REFUSED = "This draft is read-only here.";

/** "company", a unit's own type and size, or a seat's kind and handle: the line under a name. */
function captionOf(view: NodeView, seats: number): string {
  if (view.type === "company") return "company";
  if (view.type === "unit")
    return `${unitTypeLabel(view).toLowerCase()} · ${plural(seats, "seat")}`;
  return `${view.kind === "human" ? "human" : "agent"} · ${handleLabel(view.handle)}`;
}

export function TableView() {
  useDrawnSeatCount();
  const api = useBuilder();
  const open = useOpenScreen();
  const structure = useStructure(api.state);
  const reorder = useReorder(api, structure);
  const live = useStructureLive();
  const controls = useSeatControls();
  const [show] = useStatusFilter();
  const { mark: logo } = useAdoptedMark();
  const [width, setWidth] = useState(0);
  const phone = width > 0 && width < WIDTHS.phone;
  // BELOW THE PROBLEMS COLUMN'S WIDTH THE NAME NEEDS THE ROOM: a seat's
  // controls fold to Pause or Resume, its remedy stacks under its name as on a
  // phone, and Pause and stop and Steer move into the row's menu. Left at its
  // full width, the column cut names at 768 px and beside the seat panel.
  const narrow = width > 0 && width < WIDTHS.problems;
  const [picking, setPicking] = useState(false);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());

  /*
   * THE VIEW ON SCREEN IS WHAT FOCUSES A NODE. Which node to focus after an
   * operation, an undo or a redo is the Builder's decision, and performing it
   * belongs to whichever view is mounted: here it is a row taking focus.
   */
  const grid = useRef<TreeViewHandle | null>(null);
  const handle = useMemo(
    () => ({
      focusNode: (key: NodeKey) => grid.current?.focusNode(key),
      expandAll: () => grid.current?.expandAll(),
      collapseAll: () => grid.current?.collapseAll(),
    }),
    [],
  );
  useBuilderView(handle);

  /** A seat node's live reading, by the handle the running company knows it by. */
  const liveOf = useCallback(
    (view: NodeView | undefined): SeatLive | undefined =>
      view?.type === "seat" ? live?.live.get(liveHandleOf(view)) : undefined,
    [live],
  );
  /** Every seat under a row, the row itself for a seat, by the tree the table draws. */
  const seatsUnder = useMemo(() => {
    const under = new Map<string, SeatView[]>();
    const walk = (node: TreeInput): SeatView[] => {
      const view = structure.nodes.get(node.id as NodeKey);
      const own = view?.type === "seat" ? [view] : [];
      const all = [...own, ...(node.children ?? []).flatMap(walk)];
      under.set(node.id, all);
      return all;
    };
    structure.tree.forEach(walk);
    return under;
  }, [structure]);
  /** The agents under a row that the chip on screen keeps. */
  const agentsUnder = useCallback(
    (id: string): SeatLive[] =>
      (seatsUnder.get(id) ?? [])
        .map(liveOf)
        .filter((s): s is SeatLive => !!s && s.seat.kind === "agent" && matches(s, show)),
    [seatsUnder, liveOf, show],
  );

  // A CHIP PRUNES THE TREE to the seats it keeps, a team kept while it holds one.
  const rows = useMemo(() => {
    if (show === "all") return structure.tree;
    return pruned(structure.tree, (id) => {
      const s = liveOf(structure.nodes.get(id as NodeKey));
      return !!s && matches(s, show);
    });
  }, [show, structure, liveOf]);

  // WHICH TEAMS ARE CLOSED, kept in the address for a return.
  const branches = useClosedBranches(rows);
  // BEFORE THE FIRST STATE the table is the document alone: no summaries, no controls.
  const ready = live?.loaded ?? false;
  const pickable = useMemo(
    () => (live ? [...live.live.values()].some((s) => s.controllable) : false),
    [live],
  );
  // A PHONE PICKS ROWS IN A SELECT MODE, so a box costs no row its name until asked for.
  const selecting = ready && pickable && (!phone || picking || selected.size > 0);
  const chosen = useMemo(
    () => [...selected].map((h) => live?.live.get(h)).filter((s): s is SeatLive => s !== undefined),
    [selected, live],
  );
  const pick = (handles: readonly string[], on: boolean) =>
    setSelected((was) => {
      const next = new Set(was);
      for (const h of handles) {
        if (on) next.add(h);
        else next.delete(h);
      }
      return next;
    });
  const pickableUnder = (id: string): SeatLive[] => {
    const view = structure.nodes.get(id as NodeKey);
    const own = view?.type === "seat" ? [liveOf(view)].filter(Boolean) : agentsUnder(id);
    return (own as SeatLive[]).filter((s) => s.seat.kind === "agent" && s.controllable);
  };

  const anyRemedy = useMemo(() => (live ? [...live.live.values()].some(hasRemedy) : false), [live]);
  const columns: TreeGridColumn[] = [
    // THE NAME TAKES THE LARGEST SHARE: it also holds the mark, the indent and the box.
    { key: "name", header: "Name", width: "minmax(14rem, 3fr)" },
    // ROOM FOR A COMPANY'S SUMMARY ON ONE LINE at a wide window.
    // VALUES READ AFTER THE NAME are a step smaller (`size: "sm"`), so a row reads name first.
    {
      key: "state",
      header: "Status",
      width: "minmax(9.5rem, 2.2fr)",
      hideBelow: WIDTHS.state,
      size: "sm",
    },
    {
      key: "lead",
      header: "Reports to",
      width: "minmax(9rem, 1.2fr)",
      hideBelow: WIDTHS.lead,
      size: "sm",
    },
    {
      key: "problems",
      header: "Problems",
      width: "minmax(6.5rem, 1fr)",
      hideBelow: WIDTHS.problems,
      size: "sm",
    },
    {
      key: "controls",
      header: "Controls",
      headerHidden: phone,
      // ROOM FOR A REMEDY BESIDE PAUSE where any seat has one, else for the
      // three icons at the gap `.seat-controls` sets them apart by, so a
      // paused seat still on its turn (Resume, Pause and stop, Steer) never
      // wraps. Narrow, Pause or Resume alone, in room enough for its header.
      width: phone
        ? "calc(var(--size-control-sm) + var(--spacing-2) * 2)"
        : narrow
          ? "calc(var(--size-control-sm) * 2 + var(--spacing-2) * 4)"
          : anyRemedy
            ? "calc(var(--size-control-sm) + 7rem + var(--spacing-1) + var(--spacing-2) * 2)"
            : "calc(var(--size-control-sm) * 3 + var(--spacing-2) * 2 + var(--spacing-2) * 2)",
    },
    {
      key: "actions",
      header: "Edit",
      headerHidden: phone,
      width: phone
        ? "var(--size-control-sm)"
        : "calc(var(--crewlet-org-table-strip) + var(--spacing-2) * 2)",
    },
  ];

  const cell = (id: string, column: number, context: TreeGridContext) => {
    const view = structure.nodes.get(id as NodeKey);
    if (!view) return null;
    const s = liveOf(view);
    switch (context.columnKey(column)) {
      case "name":
        return (
          <RowName
            api={api}
            view={view}
            s={s}
            logo={logo}
            context={context}
            column={column}
            seats={seatsUnder.get(id)?.length ?? 0}
            summary={
              ready && !context.shown("state") && view.type !== "seat"
                ? summaryOf(agentsUnder(id), view.type === "unit" ? "No agents" : "No agents yet")
                : ""
            }
            statusUnder={!context.shown("state")}
            remedyUnder={narrow}
            problems={foldedProblems(s, context.shown("problems"))}
          />
        );
      case "state":
        if (s) return <SeatStatus s={s} problems={foldedProblems(s, context.shown("problems"))} />;
        if (view.type === "seat" || !ready) return null;
        return (
          <span className="btable-value btable-summary">
            {summaryOf(agentsUnder(id), view.type === "unit" ? "No agents" : "No agents yet")}
          </span>
        );
      case "lead":
        return <ReportsTo view={view} index={live?.index ?? null} />;
      case "problems":
        return <RowProblems api={api} view={view} s={s} agents={agentsUnder(id)} />;
      case "controls": {
        if (!ready || !s || s.seat.kind !== "agent") return null;
        // THE STATE'S REMEDY LEADS, beside Pause; Pause and stop and Steer move to the row's menu.
        // EVERY READER SEES IT: a reader who may not take it is shown it held, with its reason.
        const remedy = !narrow && hasRemedy(s);
        return (
          <span className="btable-controls">
            {remedy && <SeatRemedy s={s} compact />}
            <SeatControlButtons s={s} controls={controls} compact={narrow || remedy} />
          </span>
        );
      }
      default:
        return (
          <RowEdits
            api={api}
            grid={context}
            open={open}
            reorder={reorder}
            view={view}
            folded={phone}
            seat={s && ready && (narrow || hasRemedy(s)) ? s : null}
            controls={controls}
          />
        );
    }
  };

  // ENTER EDITS AND DELETE REMOVES, while the row itself holds focus, as on the chart.
  const onRowKey = useCallback(
    (id: string, action: Exclude<TreeItemAction, "menu">) => {
      const node = structure.nodes.get(id as NodeKey);
      if (!node) return false;
      if (action === "activate") {
        if (!editsInPlace(node) || api.editsHeld) return false;
        api.openEditor(node.key);
        return true;
      }
      if (api.readOnly || !isDeletable(node)) return false;
      api.openDelete(node.key);
      return true;
    },
    [api, structure],
  );
  const onRowKeyDown = useCallback(
    (id: string, event: KeyboardEvent<HTMLElement>) => {
      if (!event.altKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return false;
      reorder.move(id as NodeKey, event.key === "ArrowUp" ? -1 : 1);
      return true;
    },
    [reorder],
  );

  return (
    <div className="btable" data-selecting={chosen.length > 0 ? "" : undefined}>
      {ready && <LimitBanner />}
      <div className="btable-bar">
        <StatusChips />
        {ready && pickable && phone && selected.size === 0 && (
          <Button
            size="small"
            variant={picking ? "ghost" : "secondary"}
            onClick={() => setPicking(!picking)}
          >
            {picking ? "Done selecting" : "Select agents"}
          </Button>
        )}
      </div>
      {/* BEFORE THE ROWS, so the kit's sticky bar stays in view over them. */}
      {chosen.length > 0 && (
        <SeatSelectionBar
          selected={chosen}
          onClear={() => setSelected(new Set())}
          controls={controls}
        />
      )}
      {rows.length === 0 ? (
        <FilterEmpty />
      ) : (
        <OrgTable
          label="Organization"
          layout="container"
          onWidthChange={setWidth}
          // THE PAIR LIVES IN THE PAGE TOOLBAR, on both tabs.
          controls={false}
          columns={columns}
          rows={rows}
          tone={(id) => nodeTone(structure.nodes.get(id as NodeKey))}
          ref={grid}
          expanded={branches.expanded}
          onExpandedChange={branches.onExpandedChange}
          renderCell={cell}
          // A CELL HOLDING A LINK OR A CONTROL: a team's add pill or a saved
          // seat's name, a Reports to seat, the controls and the edits.
          cellHasControl={(id, _column, key) => {
            const view = structure.nodes.get(id as NodeKey);
            if (key === "name") return view?.type !== "seat" || !!view.saved;
            if (key === "lead") return view?.type !== "company";
            return key === "actions" || key === "controls";
          }}
          {...(selecting
            ? {
                selection: {
                  checked: (id: string) => {
                    const under = pickableUnder(id);
                    if (under.length === 0) return null;
                    const on = under.filter((s) => selected.has(s.seat.handle)).length;
                    return on === 0 ? false : on === under.length ? true : "mixed";
                  },
                  label: (id: string) => {
                    const view = structure.nodes.get(id as NodeKey);
                    const name = view?.name || "the company";
                    return view?.type === "seat"
                      ? `Select ${name}`
                      : `Select every agent in ${name}`;
                  },
                  onCheckedChange: (id: string, on: boolean) =>
                    pick(
                      pickableUnder(id).map((s) => s.seat.handle),
                      on,
                    ),
                },
              }
            : {})}
          onRowKey={onRowKey}
          onRowKeyDown={onRowKeyDown}
          hasRowMenu={(id) => {
            const node = structure.nodes.get(id as NodeKey);
            return node !== undefined && rowMenu(api, node, open, reorder).length > 0;
          }}
          // SELECTION IS THE BUILDER'S, so the row a link names is marked here.
          selectedId={api.selection.key}
          onSelect={(id) => api.selection.select(id as NodeKey)}
          readOnly={api.readOnly}
        />
      )}
      {controls.dialog}
    </div>
  );
}

/** The tree pruned to the rows `keep` names, a branch kept while it holds one. */
export function pruned(nodes: readonly TreeInput[], keep: (id: string) => boolean): TreeInput[] {
  const out: TreeInput[] = [];
  for (const node of nodes) {
    const children = node.children ? pruned(node.children, keep) : [];
    if (keep(node.id) || children.length > 0) {
      out.push({ ...node, children: children.length ? children : undefined });
    }
  }
  return out;
}

/**
 * A row's name cell: its mark (a seat's figure, the company's own logo where
 * its brand has one), its name (a saved seat's a link to its page), the line
 * under it, and on a team or the company the add pill beside the name.
 */
function RowName({
  api,
  view,
  s,
  logo,
  context,
  column,
  seats,
  summary,
  statusUnder,
  remedyUnder,
  problems,
}: {
  api: BuilderApi;
  view: NodeView;
  s: SeatLive | undefined;
  logo: string | null;
  context: TreeGridContext;
  column: number;
  seats: number;
  /** A team's summary, under its name while the Status column is left out. */
  summary: string;
  /** Whether the seat's status is said under its name, the Status column being left out. */
  statusUnder: boolean;
  /** Whether the state's remedy is drawn under its name: a narrow table's row. */
  remedyUnder: boolean;
  /** The seat's problems, said under its name with its state where their column is left out. */
  problems: readonly SeatProblem[];
}) {
  const page = view.type === "seat" && view.saved ? view.saved.handle : "";
  const label = page ? (
    <Link
      variant="plain"
      size="inherit"
      className="btable-label"
      href={href(seatPath({ handle: page, name: view.name }))}
      title={view.name || undefined}
    >
      {view.name}
    </Link>
  ) : (
    <span className="btable-label" title={view.name || undefined}>
      {view.name || "Unnamed company"}
    </span>
  );
  // A TEAM'S ADD PILL STANDS BESIDE ITS NAME; a reader who may not change the org has none, as on the chart.
  const add =
    view.type !== "seat" && !api.editsHeld ? (
      <OrgTableAdd
        placement="name"
        size="xs"
        label={addLabel(view.name)}
        onOpen={() => context.opened(view.key, column)}
        sections={addSections(api, view).map((section) =>
          section.disabled && api.readOnly ? { ...section, disabledReason: REFUSED } : section,
        )}
      />
    ) : undefined;
  const mark =
    view.type === "seat"
      ? seatMark(view.kind, view.avatar)
      : view.type === "company" && logo
        ? { logo: { src: logo, alt: view.name || "The company" } }
        : { icon: <NodeGlyph kind={nodeGlyphKind(view)} /> };
  const worst = problems[0]?.tag;
  const caption =
    s && statusUnder && s.seat.kind === "agent" ? (
      <span
        className="btable-caption"
        title={[s.word, ...problems.map((p) => p.sentence), captionOf(view, seats)].join(" · ")}
      >
        <StatusDot tone={s.tone} pulse={s.state === "working"}>
          {s.word}
        </StatusDot>{" "}
        · {worst ? `${worst} · ` : ""}
        {captionOf(view, seats)}
      </span>
    ) : summary ? (
      <span className="btable-caption" title={summary}>
        {summary}
      </span>
    ) : (
      <span className="btable-caption" title={captionOf(view, seats)}>
        {captionOf(view, seats)}
      </span>
    );
  // `btable-name` is what the stylesheet sizes and what a suite finds a row by.
  const name = (
    <OrgTableName
      className="btable-name"
      {...mark}
      tone={nodeTone(view)}
      name={label}
      nameAction={add}
      caption={caption}
      captionMarks={
        <>
          {view.type === "unit" && <UnitMarks view={view} />}
          {view.type === "seat" && <SeatMarks view={view} />}
        </>
      }
    />
  );
  // THE CELL LAYS ITS CHILDREN IN A ROW, so a phone's remedy is stacked with
  // the name rather than set beside it, where it squeezed the name to a letter.
  if (!(remedyUnder && s && hasRemedy(s))) return name;
  return (
    <span className="btable-name-stack">
      {name}
      <span className="btable-under">
        <SeatRemedy s={s} compact />
      </span>
    </span>
  );
}

/**
 * A seat's problems where its row must say them beside its state: the
 * Problems column is left out (below 900 px), and a seat that failed its
 * last turn read "Idle" with nothing else. None where the column is drawn.
 */
function foldedProblems(s: SeatLive | undefined, shown: boolean): readonly SeatProblem[] {
  return !s || shown ? [] : s.problems;
}

/**
 * A seat's Status, on one calm line: its state's dot and word, a pause it
 * has not reached yet, and what waits on it as one quiet count. What it is
 * doing, asking or was paused for, and who waits, are the line's tooltip, the
 * seat panel's, and read out with the line (the kit's `StatusLine`).
 */
function SeatStatus({ s, problems }: { s: SeatLive; problems: readonly SeatProblem[] }) {
  const human = s.seat.kind === "human";
  const count = s.waiting.priority + s.waiting.background;
  const note = s.pauseNote;
  const detail = [
    human ? s.seat.availability || "Human teammate" : doingLine(s),
    ...waitingWords(s.row?.waiting).map((w) => w.text),
    note ? PAUSE_NOTE_WORDS[note].sentence : "",
    ...problems.map((p) => p.sentence),
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <span className="btable-value">
      <StatusLine
        label={s.word}
        // A PERSON'S SEAT HAS NO DOT: its word alone, in the secondary ink.
        {...(human ? {} : { tone: s.tone, pulse: s.state === "working" })}
        notes={[
          note ? PAUSE_NOTE_WORDS[note].note : "",
          problems[0]?.tag ?? "",
          count > 0 ? `${count.toLocaleString()} waiting` : "",
        ]}
        detail={detail}
      />
    </span>
  );
}

/** Who a row reports to: the manager (a team's lead) as their own seat, or the words for none. */
function ReportsTo({ view, index }: { view: NodeView; index: OrgIndex | null }) {
  if (view.type === "company") return <EmptyValue label="Not applicable" />;
  const handle = view.type === "seat" ? view.managerHandle : view.lead?.handle;
  const seat = handle && index ? index.byHandle.get(handle) : undefined;
  if (!seat || !index) {
    const words = view.type === "seat" ? managerLabel(view.manager) : leadLabel(view);
    const known = view.type === "seat" ? !!view.manager : !!view.lead;
    return (
      <span
        className={known ? "btable-value btable-clip" : "btable-value btable-clip muted"}
        title={words}
      >
        {words}
      </span>
    );
  }
  const badge = seatLookup(index)(seat.handle);
  const kind = badge.kind === "human" ? "human" : "agent";
  // THE SEAT AS ITSELF: an agent's character and hue, a person's initials.
  return (
    <EntityChip
      className="btable-chip"
      name={seat.name}
      href={href(seatPath(seat))}
      kind={kind}
      size="xs"
      {...faceOf(kind, badge.avatar)}
      title={view.type === "unit" ? leadLabel(view) : seat.name}
    />
  );
}

/** A row's problems: a seat's own as tags, a team's as how many of its agents have one, and the draft's. */
function RowProblems({
  api,
  view,
  s,
  agents,
}: {
  api: BuilderApi;
  view: NodeView;
  s: SeatLive | undefined;
  agents: readonly SeatLive[];
}) {
  const drafted = api.problemsFor(view.key).length;
  const draft =
    drafted > 0 ? (
      <Tag
        variant="danger"
        size="sm"
        title="Problems the configuration's check found in this draft"
      >
        {plural(drafted, "configuration problem")}
      </Tag>
    ) : null;
  const seats = view.type === "seat" ? 0 : agents.filter((a) => a.problems.length > 0).length;
  const own = s?.problems ?? [];
  if (own.length === 0 && seats === 0 && !draft) {
    return <span className="btable-value muted">None</span>;
  }
  return (
    <span className="btable-value">
      {own.map((p) => (
        <Tag key={p.key} variant={p.tone} size="sm" title={p.sentence}>
          {p.tag}
        </Tag>
      ))}
      {seats > 0 && (
        <Tag variant="danger" size="sm" title={`${plural(seats, "agent")} here with a problem`}>
          {plural(seats, "agent")}
        </Tag>
      )}
      {draft}
    </span>
  );
}

/**
 * The edits at the end of a row, drawn at rest: Edit, Delete and the row's
 * menu, which holds what has no button of its own. A row too narrow for its
 * controls also holds Pause and stop and Steer in its menu, and a phone's row
 * holds Edit and Delete there too.
 */
function RowEdits({
  api,
  grid,
  open,
  reorder,
  view,
  folded,
  seat,
  controls,
}: {
  api: BuilderApi;
  grid: TreeGridContext;
  open: OpenScreen;
  reorder: Reorder;
  view: NodeView;
  folded: boolean;
  /** The seat whose own controls the menu holds, where its row has no room for them. */
  seat: SeatLive | null;
  controls: SeatControls;
}) {
  const access = useWriteAccess("pause_seat");
  const name = view.name || "the company";
  const editHeld = api.editsHeld ?? undefined;
  const deleteHeld = api.readOnly ? (editHeld ?? REFUSED) : undefined;
  const hold = (reason: string | undefined) => (reason ? { disabled: true } : {});
  const strip: MenuEntry[] = folded
    ? [
        ...(editsInPlace(view)
          ? [
              {
                key: "edit",
                label: editLabel(name),
                icon: <PencilGlyph />,
                onSelect: () => api.openEditor(view.key),
                ...hold(editHeld),
              },
            ]
          : []),
        ...(isDeletable(view)
          ? [
              {
                key: "delete",
                label: `Delete ${view.name}`,
                icon: <TrashGlyph />,
                danger: true,
                onSelect: () => api.openDelete(view.key),
                ...hold(deleteHeld),
              },
            ]
          : []),
      ]
    : [];
  const menu: MenuEntry[] = [
    ...seatEntries(seat, seat ? controlHeld(access, seat) : undefined, controls),
    ...strip,
    ...(strip.length ? [{ kind: "separator" as const, key: "strip-end" }] : []),
    ...heldEdits(rowMenu(api, view, open, reorder), editHeld),
  ];
  const rowMenuButton = menu.length > 0 && (
    <Menu
      label={`Actions for ${name}`}
      tooltip="More actions"
      align="end"
      open={grid.menuOpen(view.key)}
      onOpenChange={(up) => grid.setMenuOpen(view.key, up)}
      items={menu}
      {...(editHeld ? { header: editHeld } : {})}
    />
  );
  if (folded) return <span className="btable-row-menu">{rowMenuButton}</span>;
  return (
    <OrgTableActions shown>
      {editsInPlace(view) && (
        <IconButton
          tooltip
          size="sm"
          label={editLabel(name)}
          icon={<PencilGlyph />}
          {...(editHeld ? { disabledReason: editHeld } : {})}
          onClick={() => api.openEditor(view.key)}
        />
      )}
      {isDeletable(view) && (
        <IconButton
          tooltip
          size="sm"
          variant="ghost-danger"
          label={`Delete ${view.name}`}
          icon={<TrashGlyph />}
          {...(deleteHeld ? { disabledReason: deleteHeld } : {})}
          onClick={() => api.openDelete(view.key)}
        />
      )}
      {rowMenuButton}
    </OrgTableActions>
  );
}

/** A narrow row's own controls, first in its menu, each held for the reason its button would be. */
function seatEntries(
  s: SeatLive | null,
  held: string | undefined,
  controls: SeatControls,
): MenuEntry[] {
  if (!s || s.seat.kind !== "agent") return [];
  const hold = held ? { disabled: true, description: held } : {};
  const out: MenuEntry[] = [];
  if (isRunning(s)) {
    out.push({
      key: "stop",
      label: "Pause and stop its turn",
      onSelect: () => controls.open("stop", [s]),
      danger: true,
      ...hold,
    });
  }
  if (s.row?.turn) {
    out.push({
      key: "steer",
      label: "Steer its turn",
      icon: <CompassGlyph />,
      onSelect: () => controls.steer(s),
      ...hold,
    });
  }
  if (out.length) out.push({ kind: "separator", key: "seat-end" });
  return out;
}
