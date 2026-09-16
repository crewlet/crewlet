/**
 * The saved-view inventory.
 *
 * # Why a view deserves a page
 *
 * A saved view is FURNITURE a person arranged: a name, a shape and a filter,
 * put somewhere so the same question is one click away tomorrow. The engine
 * keeps who owns each one, which are protected, which is a container's default
 * and which this reader has pinned — and none of it was visible anywhere. The
 * only surface a view had was a tab in the tracker's strip, which shows the
 * name and nothing else, so a company accumulating forty of them had no way to
 * see what it had.
 *
 * # A builtin is not a row here
 *
 * The three every container has without anybody saving one (five on a
 * sprinting project) have no `id`: there is nothing to rename, protect, rank
 * or pin, and nothing to address. They are the list's own view strip. This
 * screen is what SOMEBODY SAVED.
 *
 * # Running one is the tracker, not a copy of it
 *
 * `#/work/views/{key}` hands the view's key to the board as `view=`, which is
 * the engine's own expansion: the saved params are merged UNDER whatever the
 * reader types, so opening a view and changing one filter gives them the view
 * with that one key changed rather than a locked query.
 */

import { useMemo } from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { usePageCoverage, usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { Badge, Button, Empty } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { WorkView } from "~/protocol/index.ts";

/**
 * How a view's container is written, everywhere it is written.
 *
 * ONE SPELLING. It was three — the grid's sort value, the grid's cell and the
 * facts block — and the first two already disagreed: the sort key always
 * carried the colon and the cell carried it only where there was an id, so a
 * workspace view sorted under `workspace:` and rendered as `workspace`.
 */
function containerRef(view: WorkView): string {
  return view.container.id ? `${view.container.kind}:${view.container.id}` : view.container.kind;
}

export function SavedViews({ id }: { id?: string }) {
  const nav = useNavigator();
  const viewer = useViewer();
  const org = useOrg();
  // AN OWNER IS A PERSON, and the chart is what turns their handle into the
  // name they are actually called. A handle that is not in it — an operator
  // writing through the MCP surface owns views too — falls through to the
  // handle itself rather than to nothing.
  const index = useMemo(() => indexOrg(org), [org]);
  // THE VIEWER IS SENT, which is what makes pins and personal views appear at
  // all: `work_views` answers the SHARED strip without one, so this screen
  // showed every reader the same list and no pin could ever render.
  const views = useQuery(
    "work_views",
    { container: "workspace", ...(viewer.handle ? { viewer: viewer.handle } : {}) },
    { pollMs: 120_000 },
  );
  usePageCoverage(views.data);

  const saved = useMemo(
    () => (views.data?.views ?? []).filter((v) => !v.builtin && v.id),
    [views.data],
  );
  const one = id ? saved.find((v) => v.id === id) : undefined;
  usePageLabels(one ? { [one.id as string]: one.name } : {});

  // ONE VIEW IS THE BOARD RUNNING IT. There is no second renderer: a view is
  // a set of parameters for the tracker, so the page for one is the tracker
  // with `view=` set, and a copy here would be a second board to keep correct.
  if (id) {
    if (views.loading && !views.data) return <PageNote>Loading the saved view…</PageNote>;
    // A FAILED READ IS NOT A MISSING VIEW. The inventory is the ONLY answer
    // this screen has, so a refusal rendered as "no saved view with that id"
    // tells a reader their view was deleted when the truth is that nothing
    // was read at all — and they act on it, by saving a second copy.
    if (views.error) return <QueryState error={views.error} loading={false} />;
    if (!one) {
      return (
        <Empty
          icon="columns"
          title="No saved view with that id"
          hint="A view that was deleted, or one saved against a different container. The inventory lists what this company has."
        />
      );
    }
    // ONE PERSON, ONE NAME ON THIS SCREEN. The note printed the raw handle
    // while the Owner fact two lines below resolved the same value through the
    // chart — so `ada-okonkwo` and "Ada Okonkwo" appeared on one page as if
    // they were two people. Resolved once, here, and handed to both.
    //
    // FALLING BACK TO THE HANDLE rather than to nothing: an operator writing
    // through the MCP surface owns views too and holds no seat, and their
    // handle is still the thing to say.
    const ownerName = one.owner ? (index.byHandle.get(one.owner)?.name ?? one.owner) : undefined;
    // A REDIRECT WOULD BE WRONG HERE, because the reader asked for the view
    // rather than for the board: the button says what running it means.
    return (
      <>
        <PageNote>
          {ownerName
            ? `A personal view owned by ${ownerName}.`
            : "A shared view — everybody in this company sees it."}{" "}
          Running it hands its saved parameters to the board, where any one of them can be changed.
        </PageNote>
        {/* A SAVED VIEW IS AN OBJECT, and this screen never printed its name:
            the only place it appeared was the breadcrumb `usePageLabels`
            feeds, so the page for one view was a paragraph, some badges and a
            button about something it never named. */}
        <ObjectHeader
          kind="Saved view"
          icon="columns"
          identifier={one.key}
          title={one.name}
          facts={viewFacts(one, ownerName)}
        />
        <ViewFacts view={one} />
        <Button
          variant="primary"
          icon="arrowRight"
          onClick={() => nav.to(["work"], { view: one.key })}
        >
          Run this view on the board
        </Button>
      </>
    );
  }

  return (
    <>
      <PageNote>
        Every view somebody saved, and what each one is. The builtins a container has without
        anybody saving one are its own view strip and have no row here: a builtin carries no id, so
        there is nothing to rename, protect, rank or pin.
      </PageNote>

      <QueryState
        error={views.error}
        loading={views.loading}
        empty={
          saved.length
            ? undefined
            : {
                title: "Nobody has saved a view yet",
                hint: "A seat or an operator saves one with save_work_view, and it appears in the container's strip and here.",
              }
        }
      >
        <DataGrid<WorkView>
          rows={saved}
          rowKey={(v) => v.id as string}
          rowHref={(v) => href(["work", "views", v.id as string])}
          defaultSort="name"
          columns={[
            {
              key: "name",
              header: "View",
              sortValue: (v) => v.name,
              cell: (v) => <TextCell icon="columns">{v.name}</TextCell>,
            },
            {
              key: "key",
              header: "Key",
              shrink: true,
              sortValue: (v) => v.key,
              cell: (v) => <KeyCell value={v.key} />,
            },
            {
              key: "type",
              header: "Shape",
              shrink: true,
              sortValue: (v) => v.type,
              cell: (v) => <Badge outline>{v.type}</Badge>,
            },
            {
              key: "owner",
              header: "Owner",
              sortValue: (v) => v.owner ?? "",
              // A SEAT IS AN AVATAR AND A NAME, not a handle in a chip: this
              // column named a person and rendered them differently from
              // every other column in the product that does.
              //
              // AND AN ABSENT OWNER IS NOT AN ABSENT PERSON, which is why
              // `SeatCell`'s own dash is not what draws here: empty means the
              // view is SHARED, a setting somebody chose.
              cell: (v) =>
                v.owner ? (
                  <SeatCell handle={v.owner} name={index.byHandle.get(v.owner)?.name} />
                ) : (
                  <span className="t-caption">shared</span>
                ),
            },
            {
              key: "container",
              header: "Container",
              sortValue: containerRef,
              cell: (v) => <KeyCell value={containerRef(v)} />,
            },
            {
              key: "marks",
              header: "Marks",
              cell: (v) => (
                <span className="row gap-1">
                  {v.default && (
                    <Badge outline title="the container's landing tab">
                      default
                    </Badge>
                  )}
                  {v.protected && (
                    <Badge outline title="only its owner may change it">
                      protected
                    </Badge>
                  )}
                  {v.pinned && (
                    <Badge tone="accent" title="pinned by you — pins are per reader">
                      pinned
                    </Badge>
                  )}
                </span>
              ),
            },
          ]}
          loadedNote={`${saved.length} saved`}
        />
      </QueryState>
    </>
  );
}

/**
 * The four facts a saved view is read by.
 *
 * THE BADGE ROW THIS REPLACES said the same things in the same order and said
 * them nowhere else on the screen — so it is re-homed here rather than
 * dropped, and [ViewFacts] below is now what it was always named for: the
 * parameters the view actually carries.
 */
function viewFacts(view: WorkView, ownerName?: string): Fact[] {
  return [
    { label: "Shape", value: view.type },
    {
      // A SHARED VIEW IS NOT AN UNOWNED ONE. Empty means everybody in the
      // company sees it, which is a setting somebody chose rather than a
      // field nobody filled in — so it is a word and never a dash.
      label: "Owner",
      value: view.owner ? (
        <SeatCell handle={view.owner} name={ownerName} />
      ) : (
        <span className="t-caption">shared</span>
      ),
    },
    { label: "Container", value: <KeyCell value={containerRef(view)} /> },
    {
      label: "Marks",
      value:
        view.default || view.protected || view.pinned ? (
          <span className="row gap-1">
            {view.default && <Badge outline>default</Badge>}
            {view.protected && <Badge outline>protected</Badge>}
            {/* THE ACCENT SAYS "YOURS" — a pin is this reader's own, where
                the other two are facts about the view itself. */}
            {view.pinned && <Badge tone="accent">pinned</Badge>}
          </span>
        ) : (
          ""
        ),
    },
  ];
}

/** What one view actually asks for, as the parameters it carries. */
function ViewFacts({ view }: { view: WorkView }) {
  const params = Object.entries(view.params ?? {});
  return (
    <div className="col gap-2">
      {params.length === 0 ? (
        <p className="t-caption">
          This view saves no filters — it is the container's whole list in this shape.
        </p>
      ) : (
        <div className="list">
          {params.map(([key, value]) => (
            <div key={key} className="thread-entry">
              <code className="inline">{key}</code>
              <span className="truncate t-cell" style={{ flex: 1 }}>
                {String(value)}
              </span>
            </div>
          ))}
        </div>
      )}
      <p className="t-caption">
        These are the board's own parameter names, not the tool's — a caller hands them straight
        back, or passes the key as <code className="inline">view=</code> and lets the engine expand
        them.
      </p>
    </div>
  );
}
