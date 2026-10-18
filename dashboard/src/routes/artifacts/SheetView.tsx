/**
 * A native sheet: an artifact whose content is its datastore, drawn by the
 * design system's `SheetGrid` from the schema alone, so there is nothing for a
 * model to get wrong and no page script at all (D03).
 *
 * # Honest instant edits (D23)
 *
 * The one place the dashboard draws a value before the engine holds it: a
 * cell the reader edits shows their value at once, marked Saving, and turns
 * solid when the engine confirms it; a refusal reverts the cell and says why
 * on it. What is drawn solid is what the engine holds. Every other write the
 * dashboard makes stays confirmed, not optimistic.
 *
 * # Everyone in the cell they are in
 *
 * Collaborators are outlined on the cell they are on, in their presence hue,
 * with their name flag beside it (D22), and the reader's own cell is told to
 * the room. Others' edits arrive as the room's pushes; one that lands on the
 * cell being edited keeps the editor open and names who changed it.
 */

import { useEffect, useMemo, useState } from "react";
import {
  Button,
  LinkInterstitial,
  Modal,
  Select,
  SheetGrid,
  Skeleton,
  externalUrl,
  useAnnouncer,
  type PresenceMember,
  type SheetCellRef,
  type SheetSort,
} from "@crewlethq/ui";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { STATE_COLLECTION } from "~/contract/artifacts.ts";
import type { ArtifactDetail, DataCollection } from "~/protocol/index.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import { cellPresence, useArtifactPeople } from "~/lib/artifacts/people.ts";
import { inferredColumns, sheetColumns, sheetRows } from "~/lib/artifacts/sheet.ts";
import { useStoreVersion } from "~/lib/artifacts/useDataStore.ts";
import { actorName, roleIncludes } from "~/lib/artifacts/words.ts";
import type { PresenceReport } from "./presence.ts";

/** The collections a reader is offered as sheets: every one but the engine's per-viewer state. */
export function sheetCollections(collections: readonly DataCollection[]): DataCollection[] {
  return collections.filter((c) => c.name !== STATE_COLLECTION);
}

export function SheetView({
  detail,
  store,
  room,
  presence,
  drawn,
}: {
  detail: ArtifactDetail;
  store: ArtifactDataStore;
  room: ArtifactRoom;
  presence: PresenceReport;
  drawn: PresenceMember[];
}) {
  useStoreVersion(store);
  const people = useArtifactPeople();
  const connection = useConnection();
  const announce = useAnnouncer();
  useEffect(() => {
    if (!store.schema) void store.loadSchema();
  }, [store]);
  const collections = useMemo(
    () => sheetCollections(store.schema?.collections ?? []),
    [store.schema],
  );
  const [sheetParam, setSheet] = useParam("sheet", "");
  const collection = collections.find((c) => c.name === sheetParam) ?? collections[0];
  const name = collection?.name ?? "";
  useEffect(() => {
    if (name) void store.load(name);
  }, [store, name]);
  const role = store.schema?.role ?? detail.role;
  const docs = name ? store.docsOf(name) : [];
  const view = name ? store.view(name) : null;
  const columns = useMemo(() => {
    if (!collection) return [];
    return collection.columns ? sheetColumns(collection, role) : inferredColumns(docs);
    // The inferred columns follow the documents; the declared ones follow the schema.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [collection, role, collection?.columns ? null : docs]);
  const rows = useMemo(() => store.pending.decorate(sheetRows(docs)), [docs, store.pending]);
  const [sort, setSort] = useState<SheetSort | null>(null);
  const [history, setHistory] = useState<SheetCellRef | null>(null);
  const [link, setLink] = useState<URL | null>(null);
  const cells = useMemo(() => cellPresence(room.others, drawn, name), [room.others, drawn, name]);
  const canAdd = collection
    ? roleIncludes(role, collection.create) && roleIncludes(role, "contributor")
    : false;

  if (store.isGone) {
    return <p className="artifact-note">This sheet's data was deleted with the artifact.</p>;
  }
  if (!store.schema) {
    return store.schemaFailure ? (
      <QueryState
        error={store.schemaFailure.message}
        refusal={store.schemaFailure.refusal}
        loading={false}
      />
    ) : (
      <Skeleton variant="table" rows={6} label={`Loading ${detail.title}`} />
    );
  }
  if (!collection) {
    return (
      <p className="artifact-note">
        This sheet has no columns yet.{" "}
        {roleIncludes(role, "editor")
          ? "Add a collection on the Data tab."
          : "Its editors add them on the Data tab."}
      </p>
    );
  }
  return (
    <div className="artifact-sheet">
      {collections.length > 1 && (
        <div className="artifact-sheet-bar">
          <Select
            width="auto"
            value={name}
            onChange={(value) =>
              setSheet(String(value) === collections[0]?.name ? "" : String(value))
            }
            options={collections.map((c) => ({ value: c.name, label: c.title || c.name }))}
            ariaLabel="Sheet"
          />
        </div>
      )}
      <QueryState
        error={view?.status === "failed" ? (view.error?.message ?? "query_failed") : null}
        refusal={view?.error?.refusal ?? null}
        loading={false}
      >
        {view?.status === "loading" && rows.length === 0 ? (
          <Skeleton
            variant="table"
            rows={6}
            label={`Loading ${collection.title || collection.name}`}
          />
        ) : (
          <SheetGrid
            className="artifact-grid"
            label={collection.title || detail.title}
            columns={columns}
            rows={rows}
            people={people.sheetPeople}
            mode={roleIncludes(role, "contributor") ? "edit" : "read"}
            onEdit={(edits) => void store.edit(name, edits)}
            onAddRow={canAdd ? () => void store.addRow(name) : undefined}
            onShowHistory={(cell) => setHistory(cell)}
            onActiveCellChange={(cell) =>
              presence.at(
                cell ? { collection: name, doc: cell.rowId, field: cell.field } : null,
                false,
              )
            }
            onEditingChange={(cell) =>
              presence.at(
                cell ? { collection: name, doc: cell.rowId, field: cell.field } : null,
                cell !== null,
              )
            }
            onUndo={() =>
              void store.undo().then((result) => {
                if (result === null) announce("Nothing of yours to undo.");
                else if (result.verdict !== "applied" && result.verdict !== "pending") {
                  announce("That change was overtaken by another and cannot be undone.");
                }
              })
            }
            onRedo={() =>
              void store.redo().then((result) => {
                if (result === null) announce("Nothing to redo.");
              })
            }
            onOpenLink={(raw) => {
              const url = externalUrl(raw);
              if (url) setLink(url);
            }}
            presence={cells}
            sort={sort}
            onSortChange={setSort}
            sync={{ ...store.pending.sync(!connection.connected), failed: store.unsettledCount }}
            onRetry={() => void store.retry()}
          />
        )}
      </QueryState>
      <CellHistory
        artifact={detail.id}
        collection={name}
        cell={history}
        label={(field) => collection.columns?.find((c) => c.key === field)?.label || field}
        canDelete={roleIncludes(role, collection.delete)}
        onDelete={(rowId) => {
          setHistory(null);
          void store.deleteRows(name, [rowId]);
        }}
        onClose={() => setHistory(null)}
      />
      <LinkInterstitial url={link} source={detail.title} onClose={() => setLink(null)} />
    </div>
  );
}

/**
 * One cell's history: who changed it, when, and from what (D50 keeps it). Read
 * from the store's own history, narrowed to the document and the field, under
 * the collection's read rule like every other read.
 */
function CellHistory({
  artifact,
  collection,
  cell,
  label,
  canDelete,
  onDelete,
  onClose,
}: {
  artifact: string;
  collection: string;
  cell: SheetCellRef | null;
  label: (field: string) => string;
  canDelete: boolean;
  onDelete: (rowId: string) => void;
  onClose: () => void;
}) {
  const people = useArtifactPeople();
  const now = useNow();
  const answer = useQuery(
    "artifact_data_history",
    cell ? { id: artifact, collection, doc: cell.rowId, field: cell.field, limit: 50 } : undefined,
    { enabled: cell !== null },
  );
  const field = cell?.field ?? "";
  const entries = (answer.data?.entries ?? []).flatMap((e) =>
    e.changes
      .filter((c) => c.c === collection && c.d === cell?.rowId)
      .flatMap((c) =>
        (c.f ?? []).filter((f) => f.f === field).map((f) => ({ entry: e, change: c, field: f })),
      ),
  );
  const shown = (v: unknown) =>
    v === undefined || v === null ? "empty" : typeof v === "string" ? v : JSON.stringify(v);
  return (
    <Modal
      open={cell !== null}
      onClose={onClose}
      title={cell ? `History of ${label(field)}` : ""}
      placement="top"
    >
      <QueryState
        error={answer.error}
        refusal={answer.refusal}
        loading={answer.loading}
        empty={
          entries.length === 0
            ? { title: "No changes kept", hint: "Changes are kept for seven days." }
            : undefined
        }
      >
        <ol className="artifact-history" role="list">
          {entries.map(({ entry, field: f }) => (
            <li key={`${entry.seq}:${f.f}`} className="artifact-history-row">
              <span className="artifact-history-who">
                {actorName(entry.actor, people.seatName)}
              </span>
              <span className="artifact-history-what">
                {f.bh || f.ah ? "changed a long value" : `${shown(f.b)} to ${shown(f.a)}`}
              </span>
              <time className="artifact-history-when" dateTime={entry.at} title={entry.at}>
                {relTime(entry.at, now)}
              </time>
            </li>
          ))}
        </ol>
      </QueryState>
      {canDelete && cell && (
        <div className="artifact-history-foot">
          <Button size="small" variant="danger" onClick={() => onDelete(cell.rowId)}>
            Delete this row
          </Button>
        </div>
      )}
    </Modal>
  );
}
