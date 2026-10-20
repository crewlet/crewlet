/**
 * A sheet's gallery cover: its first sheet's first columns and rows, read as
 * the reader through the datastore's own questions and drawn natively at card
 * size, never a screenshot (D64). A reader sees on the cover exactly the rows
 * the sheet would show them.
 */

import { useMemo } from "react";
import {
  ArtifactPreviewGlyph,
  SHEET_COVER_COLUMNS,
  SHEET_COVER_ROWS,
  SHEET_LABELS,
  SheetCover as Cover,
  displayText,
  normalizeValue,
} from "@crewlethq/ui";
import type { ArtifactListItem } from "~/protocol/index.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { sheetColumns, sheetRows } from "~/lib/artifacts/sheet.ts";
import { sheetCollections } from "./SheetView.tsx";

/** How many documents a cover reads to find the first rows in the sheet's own order. */
const COVER_READ = 50;

export function SheetCover({ item }: { item: Pick<ArtifactListItem, "id"> }) {
  const schema = useQuery("artifact_data_schema", { id: item.id });
  const collection = schema.data ? sheetCollections(schema.data.collections)[0] : undefined;
  const docs = useQuery(
    "artifact_data",
    { id: item.id, collection: collection?.name ?? "", limit: COVER_READ },
    { enabled: collection !== undefined },
  );
  const drawn = useMemo(() => {
    if (!collection || !schema.data || !docs.data) return null;
    // A HIDDEN COLUMN IS LEFT OUT by [sheetColumns], as the sheet leaves it out.
    const columns = sheetColumns(collection, schema.data.role).slice(0, SHEET_COVER_COLUMNS);
    const rows = sheetRows(
      docs.data.docs
        .filter((d) => !d.deleted && d.fields)
        .map((d) => ({ doc: d, fields: d.fields ?? {} })),
    ).slice(0, SHEET_COVER_ROWS);
    const context = { labels: SHEET_LABELS };
    return {
      columns: columns.map((c) => c.label),
      rows: rows.map((r) =>
        columns.map((c) => displayText(c, normalizeValue(c, r.values[c.field]), context)),
      ),
    };
  }, [collection, schema.data, docs.data]);
  if (!drawn || drawn.columns.length === 0) return <ArtifactPreviewGlyph kind="sheet" />;
  return <Cover columns={drawn.columns} rows={drawn.rows} />;
}
