/**
 * The views and dialogs the Org chart screen hands its Builder lens.
 *
 * ONE PLACE WHERE THE LENS IS ASSEMBLED. The Builder hosts its canvas, its
 * outline, its node editor and its structural dialogs without importing any
 * of them (see `BuilderSurfaces` in `Builder.tsx`), so each is built and
 * tested against `BuilderContext` alone. This is where the screen binds them.
 * A member left `null` is a surface this build does not carry, and the lens
 * says so where it would have drawn it.
 */

import type { BuilderSurfaces } from "./Builder.tsx";

export const builderSurfaces: BuilderSurfaces = {
  canvas: null,
  outline: null,
  editor: null,
  add: null,
  move: null,
  remove: null,
  changeKind: null,
};
