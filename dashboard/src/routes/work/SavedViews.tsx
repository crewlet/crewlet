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
import { usePageCoverage, usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { Badge, Button, Empty } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { WorkView } from "~/protocol/index.ts";

export function SavedViews({ id }: { id?: string }) {
  const nav = useNavigator();
  const viewer = useViewer();
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
    if (!one) {
      return (
        <Empty
          icon="columns"
          title="No saved view with that id"
          hint="A view that was deleted, or one saved against a different container. The inventory lists what this company has."
        />
      );
    }
    // A REDIRECT WOULD BE WRONG HERE, because the reader asked for the view
    // rather than for the board: the button says what running it means.
    return (
      <>
        <PageNote>
          {one.owner
            ? `A personal view owned by ${one.owner}.`
            : "A shared view — everybody in this company sees it."}{" "}
          Running it hands its saved parameters to the board, where any one of them can be changed.
        </PageNote>
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
              cell: (v) =>
                v.owner ? (
                  <Badge outline>{v.owner}</Badge>
                ) : (
                  <span className="t-caption">shared</span>
                ),
            },
            {
              key: "container",
              header: "Container",
              sortValue: (v) => `${v.container.kind}:${v.container.id}`,
              cell: (v) => (
                <span className="mono t-caption">
                  {v.container.kind}
                  {v.container.id ? `:${v.container.id}` : ""}
                </span>
              ),
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

/** What one view actually asks for, as the parameters it carries. */
function ViewFacts({ view }: { view: WorkView }) {
  const params = Object.entries(view.params ?? {});
  return (
    <div className="col gap-2">
      <div className="row gap-1 wrap">
        <Badge outline>{view.type}</Badge>
        <Badge outline>
          {view.container.kind}
          {view.container.id ? `:${view.container.id}` : ""}
        </Badge>
        {view.protected && <Badge outline>protected</Badge>}
        {view.default && <Badge outline>default</Badge>}
        {view.pinned && <Badge tone="accent">pinned</Badge>}
      </div>
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
