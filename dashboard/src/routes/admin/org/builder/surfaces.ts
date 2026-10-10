/**
 * The views and dialogs Agents › Edit org hands its builder.
 *
 * ONE PLACE WHERE THE BUILDER IS ASSEMBLED. The Builder hosts its visualization,
 * its table, its node editor and its structural dialogs without importing any
 * of them (see `BuilderSurfaces` in `Builder.tsx`), so each is built and
 * tested against `BuilderContext` alone, and a Builder suite can stand a view
 * in with a fake. This is where the screen binds the real ones, and
 * `surfaces.test.tsx` holds the builder to drawing them.
 */

import type { BuilderSurfaces } from "./Builder.tsx";
import { AddNodeDialog } from "./AddNodeDialog.tsx";
import { CanvasView } from "./CanvasView.tsx";
import { ChangeKindDialog } from "./ChangeKindDialog.tsx";
import { DeleteDialog } from "./DeleteDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";
import { NodeEditor } from "./NodeEditor.tsx";
import { TableView } from "./TableView.tsx";

export const builderSurfaces: BuilderSurfaces = {
  canvas: CanvasView,
  table: TableView,
  editor: NodeEditor,
  add: AddNodeDialog,
  move: MoveDialog,
  remove: DeleteDialog,
  changeKind: ChangeKindDialog,
};
