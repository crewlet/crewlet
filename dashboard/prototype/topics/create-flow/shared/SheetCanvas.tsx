/**
 * A new sheet, open as the grid it will be: the chosen starting point's
 * columns in the sheet's own `SheetGrid`, editable before anything is made,
 * so a person fills the first rows while they decide. Choosing another
 * starting point swaps the columns and keeps nothing of the last one's rows,
 * which never matched them.
 */

import { useMemo, useState } from "react";
import { SheetGrid, type SheetEdit, type SheetRow } from "@crewlethq/ui";
import type { DataCollection } from "~/protocol/index.ts";
import { sheetColumns } from "~/lib/artifacts/sheet.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { SHEET_TEMPLATE_DEFS, type SheetTemplateDef } from "./templates.ts";

export interface SheetDraftState {
  title: string;
  template: string;
  rows: SheetRow[];
}

let next = 0;
const rowId = () => `draft-${++next}`;

/** A starting point's first rows, as the grid holds them. */
export function rowsOf(t: SheetTemplateDef): SheetRow[] {
  return t.rows.map((values) => ({ id: rowId(), values }));
}

export function sheetDraftOf(templateId = "blank", title = ""): SheetDraftState {
  const t = templateOf(templateId);
  return { title: title || t.suggested, template: t.id, rows: rowsOf(t) };
}

export function templateOf(id: string): SheetTemplateDef {
  return SHEET_TEMPLATE_DEFS.find((t) => t.id === id) ?? SHEET_TEMPLATE_DEFS[0]!;
}

/** The draft's schema and its rows' values, for the create. */
export function sheetCommitOf(draft: SheetDraftState): {
  schema: DataCollection[];
  rows: Record<string, unknown>[];
} {
  return {
    schema: templateOf(draft.template).schema(),
    rows: draft.rows.map((r) => ({ ...r.values })),
  };
}

export function SheetCanvas({
  draft,
  onChange,
  height,
  className,
}: {
  draft: SheetDraftState;
  onChange: (next: SheetDraftState) => void;
  height?: number;
  className?: string;
}) {
  const people = useArtifactPeople();
  const t = templateOf(draft.template);
  const collection = useMemo(() => t.schema()[0]!, [t]);
  const columns = useMemo(() => sheetColumns(collection, "owner"), [collection]);
  // THE GRID'S OWN UNDO is the draft's history: one step per edit.
  const [history, setHistory] = useState<SheetRow[][]>([]);
  const onEdit = (edits: readonly SheetEdit[]) => {
    setHistory((h) => [...h, draft.rows]);
    const rows = draft.rows.map((r) => {
      const mine = edits.filter((e) => e.rowId === r.id);
      if (mine.length === 0) return r;
      const values = { ...r.values };
      for (const e of mine) values[e.field] = e.value;
      return { ...r, values };
    });
    onChange({ ...draft, rows });
  };
  return (
    <div className={["cf-sheet", className].filter(Boolean).join(" ")}>
      <SheetGrid
        label={draft.title || "New sheet"}
        mode="edit"
        fit={height ? "container" : "content"}
        {...(height ? { height } : {})}
        columns={columns}
        rows={draft.rows}
        people={people.sheetPeople}
        onEdit={onEdit}
        onAddRow={() => onChange({ ...draft, rows: [...draft.rows, { id: rowId(), values: {} }] })}
        onDeleteRow={(id) => onChange({ ...draft, rows: draft.rows.filter((r) => r.id !== id) })}
        onUndo={
          history.length > 0
            ? () => {
                const last = history[history.length - 1]!;
                setHistory((h) => h.slice(0, -1));
                onChange({ ...draft, rows: last });
              }
            : undefined
        }
      />
    </div>
  );
}
