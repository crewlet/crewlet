/**
 * The Data tab: an artifact's datastore as its authors inspect it, the human
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
import { Button, SheetGrid, Select, Skeleton, downloadText, useToast } from "@crewlethq/ui";
import { ArrowDownToLineGlyph } from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { QueryState, Section } from "~/components/common.tsx";
import { Segmented } from "~/ui/primitives.tsx";
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
import { inferredColumns, sheetColumns, sheetRows } from "~/lib/artifacts/sheet.ts";
import { useStoreVersion } from "~/lib/artifacts/useDataStore.ts";
import { actorName, roleIncludes } from "~/lib/artifacts/words.ts";
import { SchemaEditor } from "./SchemaEditor.tsx";
import { sheetCollections } from "./SheetView.tsx";

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
    <div className="artifact-data">
      <div className="artifact-data-bar">
        <Segmented
          value={view}
          onChange={(next) => setView(next)}
          ariaLabel="What to show"
          options={[
            { value: "rows", label: "Rows" },
            { value: "schema", label: "Schema" },
            { value: "history", label: "History" },
          ]}
        />
        <span className="artifact-note">
          {plural(schema.store.docs, "row")}, {fmtBytes(schema.store.footprint)} held
        </span>
      </div>
      {view === "rows" && (
        <Rows detail={detail} store={store} collections={collections} role={role} />
      )}
      {view === "schema" && (
        <SchemaEditor
          detail={detail}
          collections={collections}
          role={role}
          onSaved={() => void store.loadSchema()}
        />
      )}
      {view === "history" && <StoreHistory detail={detail} role={role} />}
    </div>
  );
}

/** One collection whole, with its export. */
function Rows({
  detail,
  store,
  collections,
  role,
}: {
  detail: ArtifactDetail;
  store: ArtifactDataStore;
  collections: DataCollection[];
  role: ArtifactDetail["role"];
}) {
  const people = useArtifactPeople();
  const toast = useToast();
  const [picked, setPicked] = useState(collections[0]?.name ?? "");
  const collection = collections.find((c) => c.name === picked) ?? collections[0];
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
  const [exporting, setExporting] = useState(false);
  const canExport = roleIncludes(role, "editor");
  const exportCollection = async () => {
    if (!name) return;
    setExporting(true);
    try {
      const { started } = downloadText(await exportCSV(detail.id, name), `${name}.csv`, "text/csv");
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
  if (!collection)
    return <p className="artifact-note">No collections yet. Add one on the Schema view.</p>;
  return (
    <>
      <div className="artifact-sheet-bar">
        {collections.length > 1 && (
          <Select
            width="auto"
            value={name}
            onChange={(value) => setPicked(String(value))}
            options={collections.map((c) => ({ value: c.name, label: c.title || c.name }))}
            ariaLabel="Collection"
          />
        )}
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
      </div>
      <SheetGrid
        className="artifact-grid"
        label={`${collection.title || collection.name}, every field`}
        columns={columns}
        rows={rows}
        people={people.sheetPeople}
        mode={roleIncludes(role, "contributor") ? "edit" : "read"}
        onEdit={(edits) => void store.edit(name, edits)}
        sync={{ ...store.pending.sync(false), failed: store.unsettledCount }}
        onRetry={() => void store.retry()}
      />
    </>
  );
}

/** The store's own record: every write, newest first, with Undo where it can still be undone. */
function StoreHistory({ detail, role }: { detail: ArtifactDetail; role: ArtifactDetail["role"] }) {
  const people = useArtifactPeople();
  const now = useNow();
  const toast = useToast();
  const answer = useQuery(
    "artifact_data_history",
    { id: detail.id, limit: 100 },
    { pollMs: 30_000 },
  );
  const gesture = useKeyedGesture();
  const canUndo = roleIncludes(role, "contributor");
  const undo = async (entry: DataHistoryEntry) => {
    const result = await gesture.run(
      undoData(detail.id, entry.seq, `dash-${newActOpID()}`, { via: "dashboard" }),
    );
    if (result?.kind === "done") {
      toast.ok("Undone");
      answer.refetch();
    } else if (result) {
      toast.show({ variant: "danger", title: "Could not undo it", message: result.text });
    }
  };
  const entries = answer.data?.entries ?? [];
  return (
    <Section
      title="Every change"
      hint="Kept for seven days. Who made each one, and what it changed."
    >
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
            <li key={entry.seq} className="artifact-history-row">
              <span className="artifact-history-who">
                {actorName(entry.actor, people.seatName)}
              </span>
              <span className="artifact-history-what">
                {changeWords(entry)}
                {entry.verdict !== "applied" ? `, refused (${entry.reason || entry.verdict})` : ""}
              </span>
              <time className="artifact-history-when" dateTime={entry.at} title={entry.at}>
                {relTime(entry.at, now)}
              </time>
              {entry.undoable && canUndo && (
                <Button
                  size="small"
                  variant="ghost"
                  loading={gesture.busy}
                  onClick={() => void undo(entry)}
                >
                  Undo
                </Button>
              )}
            </li>
          ))}
        </ol>
      </QueryState>
    </Section>
  );
}

/** One write in words: how many rows and cells it changed, or the schema. */
function changeWords(entry: DataHistoryEntry): string {
  if (entry.op === "configure") return "changed the schema";
  if (entry.op === "purge") return "deleted the data";
  const docs = new Set(entry.changes.map((c) => `${c.c}/${c.d ?? ""}`)).size;
  const cells = entry.changes.reduce((n, c) => n + (c.f?.length ?? 0), 0);
  const removed = entry.changes.filter((c) => c.op === "delete").length;
  const verb = entry.op === "undo" ? "undid a change to" : "changed";
  if (removed > 0 && removed === entry.changes.length) return `deleted ${plural(removed, "row")}`;
  return `${verb} ${plural(cells, "cell")} in ${plural(docs, "row")}`;
}
