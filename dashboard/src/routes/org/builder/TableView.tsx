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
 * the remedy its state asks for (Review call, Raise budget) leading; answering a
 * question stays in the seat panel and the inbox. A team's or the company's Status
 * is the summary of the agents in it.
 *
 * A LIVE CHANGE MOVES NOTHING. Every track a push can change is a length, its cells
 * keep one line (the whole of it in a tooltip), and a remedy has room in its track
 * whether or not any seat has one.
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
 * first, then Status, whose words move under the name; a narrow row's remedy
 * moves to its menu, and on a phone a row keeps its name, its one control, and
 * its menu, which holds the rest.
 *
 * MOVING A ROW AMONG ITS SIBLINGS is Alt with an arrow, and the same two
 * entries in the row's menu. The engine's primary manager of a seat is the
 * FIRST seat that lists it, so passing a sibling can change who a seat reports
 * to, which `reorder` announces when it happens.
 */

import { useCallback, useMemo, useRef, useState, type KeyboardEvent } from "react";
import {
  ArrowUpRightGlyph,
  CircleStopGlyph,
  CompassGlyph,
  PencilGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
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
import { DialogPresenter } from "~/components/dialogPresenter.tsx";
import { usePeekControls } from "~/app/frame/peekAddress.ts";
import {
  SeatControlButtons,
  SeatRemedy,
  SeatSelectionBar,
  controlHeld,
  isRunning,
  rowRemedy,
  useSeatControls,
  type SeatControls,
} from "~/components/seatControls.tsx";
import { useAdoptedMark } from "~/lib/brand/useBrand.ts";
import { plural } from "~/lib/format.ts";
import { waitingCount, waitingWords } from "~/lib/inboxWaiting.ts";
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
const WIDTHS = { lead: 1100, problems: 960, state: 600, phone: 560 } as const;

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
  // BELOW THE PROBLEMS COLUMN'S WIDTH THE NAME NEEDS THE ROOM: a seat's controls
  // fold to Pause or Resume, and its remedy, Pause and stop and Steer move into
  // the row's menu. Left at its full width, the column cut names at 768 px and
  // beside the seat panel.
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

  const columns = tableColumns({ phone, narrow });

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
            problems={foldedProblems(s, context.shown("problems"))}
          />
        );
      case "state":
        if (s) return <SeatStatus s={s} problems={foldedProblems(s, context.shown("problems"))} />;
        if (view.type === "seat" || !ready) return null;
        return (
          <RowSummary
            words={summaryOf(agentsUnder(id), view.type === "unit" ? "No agents" : "No agents yet")}
          />
        );
      case "lead":
        return <ReportsTo view={view} index={live?.index ?? null} />;
      case "problems":
        return <RowProblems api={api} view={view} s={s} agents={agentsUnder(id)} />;
      case "controls": {
        if (!ready || !s || s.seat.kind !== "agent") return null;
        // THE STATE'S REMEDY LEADS, beside Pause; Pause and stop and Steer move to the row's menu.
        // EVERY READER SEES IT: a reader who may not take it is shown it held, with its reason.
        const remedy = !narrow && rowRemedy(s) !== null;
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
            seat={s && ready && (narrow || rowRemedy(s) !== null) ? s : null}
            remedyInMenu={narrow}
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
        {/* KEPT WHILE AGENTS ARE PICKED, so the first pick moves no row under the thumb. */}
        {ready && pickable && phone && (
          <Button
            size="small"
            variant={selecting ? "ghost" : "secondary"}
            onClick={() => {
              setPicking(!selecting);
              setSelected(new Set());
            }}
          >
            {selecting ? "Done selecting" : "Select agents"}
          </Button>
        )}
      </div>
      {/* BEFORE THE ROWS, so the kit's sticky bar stays in view over them. */}
      {chosen.length > 0 && (
        <SeatSelectionBar
          selected={chosen}
          onClear={() => setSelected(new Set())}
          controls={controls}
          folded={phone}
        />
      )}
      {rows.length === 0 ? (
        <FilterEmpty />
      ) : (
        // A ROW'S DIALOG IS DRAWN OVER THE WINDOW, not inside the grid's layer host.
        <DialogPresenter value={controls.present}>
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
        </DialogPresenter>
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
  return (
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
}

/** A team's or the company's summary of its agents, on one line, whole in its tooltip. */
function RowSummary({ words }: { words: string }) {
  return (
    <span className="btable-value btable-clip btable-summary" title={words}>
      {words}
    </span>
  );
}

/**
 * A seat's problems where its row must say them beside its state: the
 * Problems column is left out (below 960 px), and a seat that failed its
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
          waitingCount(s.waiting)?.text ?? "",
        ]}
        detail={detail}
        overflow="clip"
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
  const seats = view.type === "seat" ? 0 : agents.filter((a) => a.problems.length > 0).length;
  const tags = [
    ...(s?.problems ?? []).map((p) => ({ tone: p.tone, tag: p.tag, says: p.sentence })),
    ...(seats > 0
      ? [
          {
            tone: "danger" as const,
            tag: plural(seats, "agent"),
            says: `${plural(seats, "agent")} here with a problem`,
          },
        ]
      : []),
    ...(drafted > 0
      ? [
          {
            tone: "danger" as const,
            tag: plural(drafted, "configuration problem"),
            says: "Problems the configuration's check found in this draft",
          },
        ]
      : []),
  ];
  const [worst, ...rest] = tags;
  if (!worst) return <span className="btable-value muted">None</span>;
  // ONE LINE, WORST FIRST, THE REST COUNTED, so a row keeps its height and its
  // cell its width however many problems a push brings.
  return (
    <span className="btable-value">
      <Tag variant={worst.tone} size="sm" title={worst.says}>
        {worst.tag}
      </Tag>
      {rest.length > 0 && (
        <Tag variant="neutral" size="sm" title={rest.map((p) => p.says).join(" · ")}>
          +{rest.length}
        </Tag>
      )}
    </span>
  );
}

/**
 * The edits at the end of a row, drawn at rest: Edit, Delete and the row's
 * menu, which holds what has no button of its own. A row too narrow for its
 * controls also holds its remedy, Pause and stop and Steer in its menu, and a
 * phone's row holds Edit and Delete there too.
 */
function RowEdits({
  api,
  grid,
  open,
  reorder,
  view,
  folded,
  seat,
  remedyInMenu,
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
  /** Whether its remedy is in the menu too: a narrow row, which has no room for the button. */
  remedyInMenu: boolean;
  controls: SeatControls;
}) {
  const access = useWriteAccess("pause_seat");
  const peek = usePeekControls();
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
    ...seatEntries(
      seat,
      seat ? controlHeld(access, seat) : undefined,
      controls,
      // THE SEAT PANEL HOLDS THE ANSWERS A HELD CALL TAKES AND THE RAISE A BUDGET DOES.
      remedyInMenu && seat ? () => peek.open({ kind: "seat", id: seat.seat.handle }) : null,
    ),
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
  openPanel: (() => void) | null,
): MenuEntry[] {
  if (!s || s.seat.kind !== "agent") return [];
  const hold = held ? { disabled: true, description: held } : {};
  const out: MenuEntry[] = [];
  const remedy = rowRemedy(s);
  if (remedy && openPanel) {
    out.push({ key: "remedy", label: remedy, icon: <ArrowUpRightGlyph />, onSelect: openPanel });
  }
  if (isRunning(s)) {
    out.push({
      key: "stop",
      label: "Pause and stop its turn",
      icon: <CircleStopGlyph />,
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

/**
 * The Table's columns at its width: `phone` under 560 px and `narrow` under 960.
 * Name takes the free room, Reports to fits its content, which only a save changes,
 * and the rest are lengths, because a push changes what they hold.
 */
export function tableColumns({
  phone,
  narrow,
}: {
  phone: boolean;
  narrow: boolean;
}): TreeGridColumn[] {
  return [
    // THE NAME HOLDS THE MARK, THE INDENT AND THE BOX, so it takes the free room.
    { key: "name", header: "Name", floor: "14rem" },
    // ROOM FOR A STATE AND ITS COUNT, or a team's usual summary, on one line. VALUES
    // READ AFTER THE NAME are a step smaller (`size: "sm"`), so a row reads name first.
    {
      key: "state",
      header: "Status",
      width: narrow ? "10rem" : "11.5rem",
      hideBelow: WIDTHS.state,
      size: "sm",
    },
    {
      key: "lead",
      header: "Reports to",
      width: "fit-content(11rem)",
      hideBelow: WIDTHS.lead,
      size: "sm",
    },
    {
      key: "problems",
      header: "Problems",
      width: "8.5rem",
      hideBelow: WIDTHS.problems,
      size: "sm",
    },
    {
      key: "controls",
      header: "Controls",
      headerHidden: phone,
      // ROOM FOR A REMEDY BESIDE PAUSE, held whether or not any seat has one, which
      // also holds the three icons. Narrow, Pause or Resume alone, in room for its header.
      width: phone
        ? "calc(var(--size-control-sm) + var(--spacing-1) * 2)"
        : narrow
          ? "calc(var(--size-control-sm) * 2 + var(--spacing-2) + var(--spacing-3) * 2)"
          : "calc(var(--size-control-sm) + 6.5rem + var(--spacing-1) + var(--spacing-3) * 2)",
    },
    {
      key: "actions",
      header: "Edit",
      headerHidden: phone,
      width: phone
        ? "calc(var(--size-control-sm) + var(--spacing-1) * 2)"
        : "calc(var(--crewlet-org-table-strip) + var(--spacing-3) * 2)",
    },
  ];
}
