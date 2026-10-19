/**
 * The Data panel: an artifact's datastore as its authors inspect it, the human
 * mirror of what an agent reads through its tools.
 *
 * Rows draws a collection whole, every column the schema declares (hidden ones
 * included) and every field a schemaless collection's documents carry; Schema
 * is the schema editor; History is the store's own record of every write,
 * who made it, what it changed, and an Undo for each one that can still be
 * undone. Every read is the engine's, under the collection's read rule, so a
 * reader sees here exactly the rows they could see anywhere else.
 */

import { useEffect, useMemo, useState } from "react";
import {
  Button,
  EmptyState,
  SegmentedControl,
  SheetGrid,
  Select,
  Skeleton,
  Tag,
  downloadText,
  useToast,
} from "@crewlethq/ui";
import { ArrowDownToLineGlyph } from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { QueryState, Section } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes, plural, relTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import {
  newActOpID,
  type ArtifactDetail,
  type DataCollection,
  type DataHistoryEntry,
} from "~/protocol/index.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { exportCSV, undoData } from "~/lib/artifacts/api.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { csvFileName, inferredColumns, sheetColumns, sheetRows } from "~/lib/artifacts/sheet.ts";
import { useStoreVersion } from "~/lib/artifacts/useDataStore.ts";
import {
  actorName,
  dataChangeWords,
  dataRefusalReason,
  roleIncludes,
} from "~/lib/artifacts/words.ts";
import { SchemaEditor } from "./SchemaEditor.tsx";
import { sheetCollections, useSheetParam } from "./SheetView.tsx";
import { useRowDelete } from "./RowDelete.tsx";

const VIEWS = ["rows", "schema", "history"] as const;
type View = (typeof VIEWS)[number];

export function DataTab({ detail, store }: { detail: ArtifactDetail; store: ArtifactDataStore }) {
  useStoreVersion(store);
  useEffect(() => {
    if (!store.schema) void store.loadSchema();
  }, [store]);
  const [viewParam, setView] = useParam("data", "rows");
  const view: View = (VIEWS as readonly string[]).includes(viewParam)
    ? (viewParam as View)
    : "rows";
  const schema = store.schema;
  const collections = useMemo(() => sheetCollections(schema?.collections ?? []), [schema]);
  // THE COLLECTION IS IN THE ADDRESS (`sheet=`), the same one the artifact's view
  // draws, so a link or a reload keeps it.
  const [collection, setCollection] = useSheetParam(collections);
  const role = schema?.role ?? detail.role;
  if (!schema) {
    return store.schemaFailure ? (
      <QueryState
        error={store.schemaFailure.message}
        refusal={store.schemaFailure.refusal}
        loading={false}
      />
    ) : (
      <Skeleton variant="text" rows={4} label="Loading the data" />
    );
  }
  return (
    <div className={view === "rows" ? "artifact-data artifact-data--rows" : "artifact-data"}>
      <div className="artifact-data-bar">
        <SegmentedControl
          semantics="radio"
          activate="manual"
          label="What to show"
          value={view}
          onValueChange={(next) => setView(next)}
          options={[
            { value: "rows", label: "Rows" },
            { value: "schema", label: "Schema" },
            { value: "history", label: "History" },
          ]}
        />
        <span className="artifact-note">
          {plural(schema.store.docs, "row")} · {fmtBytes(schema.store.footprint)}
        </span>
        {view === "rows" && collection && (
          <ExportButton detail={detail} collection={collection} role={role} />
        )}
      </div>
      {view === "rows" && (
        <Rows
          store={store}
          collections={collections}
          collection={collection}
          onCollection={setCollection}
          onSchema={() => setView("schema")}
          role={role}
        />
      )}
      {view === "schema" && (
        <SchemaEditor
          detail={detail}
          collections={collections}
          role={role}
          onSaved={() => void store.loadSchema()}
        />
      )}
      {view === "history" && <StoreHistory detail={detail} role={role} collections={collections} />}
    </div>
  );
}

/** A collection's every row as CSV, named by the artifact's and the collection's titles. */
function ExportButton({
  detail,
  collection,
  role,
}: {
  detail: ArtifactDetail;
  collection: DataCollection;
  role: ArtifactDetail["role"];
}) {
  const toast = useToast();
  const [exporting, setExporting] = useState(false);
  const canExport = roleIncludes(role, "editor");
  const exportCollection = async () => {
    setExporting(true);
    try {
      const { started } = downloadText(
        await exportCSV(detail.id, collection.name),
        csvFileName(detail.title, collection.title || collection.name),
        "text/csv",
      );
      if (!started)
        toast.show({ variant: "warning", title: "The browser did not take the download" });
    } catch (err) {
      toast.show({
        variant: "danger",
        title: "Could not export it",
        message: err instanceof Error ? err.message : "The engine did not answer.",
      });
    } finally {
      setExporting(false);
    }
  };
  return (
    <span className="artifact-data-bar-end">
      <Button
        size="small"
        variant="secondary"
        leadingIcon={<ArrowDownToLineGlyph size="sm" />}
        loading={exporting}
        disabledReason={canExport ? undefined : "Exporting every row needs the “Can edit” role."}
        onClick={() => void exportCollection()}
      >
        Export CSV
      </Button>
    </span>
  );
}

/** One collection whole, every field. */
function Rows({
  store,
  collections,
  collection,
  onCollection,
  onSchema,
  role,
}: {
  store: ArtifactDataStore;
  collections: DataCollection[];
  collection: DataCollection | undefined;
  onCollection: (name: string) => void;
  /** Opens the Schema view, where a first collection is added. */
  onSchema: () => void;
  role: ArtifactDetail["role"];
}) {
  const people = useArtifactPeople();
  const name = collection?.name ?? "";
  useEffect(() => {
    if (name) void store.load(name);
  }, [store, name]);
  const docs = name ? store.docsOf(name) : [];
  const columns = useMemo(() => {
    if (!collection) return [];
    // EVERY COLUMN, HIDDEN ONES INCLUDED, and every field the documents carry
    // that no column declares: what an author inspects is what is stored.
    const declared = sheetColumns(
      { ...collection, columns: (collection.columns ?? []).map((c) => ({ ...c, hidden: false })) },
      role,
    );
    const extra = inferredColumns(docs).filter((c) => !declared.some((d) => d.field === c.field));
    return [...declared, ...extra];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [collection, role, docs]);
  const rows = useMemo(() => store.pending.decorate(sheetRows(docs)), [docs, store.pending]);
  const remove = useRowDelete(
    store,
    name,
    collection ? roleIncludes(role, collection.delete) : false,
  );
  if (!collection)
    return (
      <EmptyState
        size="compact"
        title="No collections yet"
        description="A collection is one table of rows with its own columns. Add one on the Schema view."
        action={
          <Button size="small" variant="secondary" onClick={onSchema}>
            Open Schema
          </Button>
        }
      />
    );
  return (
    <>
      {collections.length > 1 && (
        <div className="artifact-sheet-bar">
          <Select
            width="auto"
            value={name}
            onChange={(value) => onCollection(String(value))}
            options={collections.map((c) => ({ value: c.name, label: c.title || c.name }))}
            ariaLabel="Collection"
          />
        </div>
      )}
      <SheetGrid
        className="artifact-grid"
        fit="content"
        label={`${collection.title || collection.name}, every field`}
        columns={columns}
        rows={rows}
        people={people.sheetPeople}
        mode={roleIncludes(role, "contributor") ? "edit" : "read"}
        onEdit={(edits) => void store.edit(name, edits)}
        onDeleteRow={
          remove.onDeleteRow
            ? (rowId) =>
                remove.onDeleteRow?.({
                  id: rowId,
                  name: `row ${rows.findIndex((r) => r.id === rowId) + 1}`,
                })
            : undefined
        }
        sync={{ ...store.pending.sync(false), failed: store.unsettledCount }}
        onRetry={() => void store.retry()}
        onDismissRefused={() => store.pending.dismissAll()}
      />
      {remove.dialog}
    </>
  );
}

/** The store's own record: every write, newest first, with Undo where it can still be undone. */
function StoreHistory({
  detail,
  role,
  collections,
}: {
  detail: ArtifactDetail;
  role: ArtifactDetail["role"];
  collections: DataCollection[];
}) {
  const people = useArtifactPeople();
  const now = useNow();
  const answer = useQuery(
    "artifact_data_history",
    { id: detail.id, limit: 100 },
    { pollMs: 30_000 },
  );
  const canUndo = roleIncludes(role, "contributor");
  const labelOf = useMemo(() => {
    const labels = new Map(
      collections.flatMap((c) =>
        (c.columns ?? []).map((col) => [`${c.name}/${col.key}`, col.label]),
      ),
    );
    return (collection: string, field: string) => labels.get(`${collection}/${field}`) || field;
  }, [collections]);
  const entries = answer.data?.entries ?? [];
  return (
    <Section title="Every change" hint="Who made each one, kept for seven days">
      <QueryState
        error={answer.error}
        refusal={answer.refusal}
        loading={answer.loading}
        empty={
          entries.length === 0
            ? { title: "No changes kept", hint: "Changes appear here as they are made." }
            : undefined
        }
      >
        <ol className="artifact-history" role="list">
          {entries.map((entry) => (
            <li
              key={entry.seq}
              className={
                canUndo
                  ? "artifact-history-row artifact-history-row--action"
                  : "artifact-history-row"
              }
            >
              <span className="artifact-history-who">
                {actorName(entry.actor, people.seatName)}
              </span>
              <span className="artifact-history-what">
                {dataChangeWords(entry, labelOf)}
                {entry.verdict !== "applied" && (
                  <>
                    {" "}
                    <Tag variant="neutral" appearance="outline">
                      Not saved
                    </Tag>{" "}
                    <span className="artifact-history-reason">
                      {dataRefusalReason(entry.verdict)}
                    </span>
                  </>
                )}
              </span>
              <time className="artifact-history-when" dateTime={entry.at} title={entry.at}>
                {relTime(entry.at, now)}
              </time>
              {canUndo && (
                <span className="artifact-history-action">
                  {entry.undoable && (
                    <UndoButton detail={detail} entry={entry} onDone={answer.refetch} />
                  )}
                </span>
              )}
            </li>
          ))}
        </ol>
      </QueryState>
    </Section>
  );
}

/** One row's Undo, with its own pending state: pressing one never spins the others. */
function UndoButton({
  detail,
  entry,
  onDone,
}: {
  detail: ArtifactDetail;
  entry: DataHistoryEntry;
  onDone: () => void;
}) {
  const toast = useToast();
  const gesture = useKeyedGesture();
  const undo = async () => {
    const result = await gesture.run(
      undoData(detail.id, entry.seq, `dash-${newActOpID()}`, { via: "dashboard" }),
    );
    if (result?.kind === "done") {
      toast.ok("Undone");
      onDone();
    } else if (result) {
      toast.show({ variant: "danger", title: "Could not undo it", message: result.text });
    }
  };
  return (
    <Button size="small" variant="ghost" loading={gesture.busy} onClick={() => void undo()}>
      Undo
    </Button>
  );
}
