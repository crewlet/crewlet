/**
 * Where a render error stops.
 *
 * WITHOUT A BOUNDARY, A RENDER ERROR UNMOUNTS THE WHOLE APPLICATION — React's
 * contract, and what a seat whose `llm` was a per-phase mapping once did to
 * this dashboard: a blank page, no navigation, and no way to learn which
 * screen or which field. So every region that draws what the engine sent has
 * one, and each takes down only itself:
 *
 * - the ROUTED SCREEN ([ScreenBoundary], `App.tsx`), reset by the resolved
 *   path, so going anywhere else is a fresh chance to render;
 * - the PEEK (`frame/PeekHost.tsx`), reset by the object it shows, inside the
 *   rail so the rail's own close and `Open ↗` still work;
 * - each LAYER the frame raises (`Shell.tsx` — the command palette and the New
 *   task sheet, [LayerBoundary]), whose fallback is a dialog of its own that
 *   closes the way the layer does;
 * - each LIVE SECTION OF THE SIDEBAR ([RailBoundary], `sidebar/Sidebar.tsx`),
 *   so a malformed project row costs the Projects list and never the
 *   navigation above it.
 *
 * NOT THE SHELL. The frame around a screen is how a reader gets somewhere
 * else, and a boundary wrapping it would take the way out down with the
 * failure it reports.
 *
 * A screen's code arrives in a chunk (`lazyScreen.ts`), so [ScreenBoundary]
 * is also where a chunk that never came is drawn — as its own sentence, with a
 * Reload, because what fixes it is a reload and not a report.
 */

import { Suspense, type ReactNode } from "react";
import { Button, Callout, ErrorBoundary, Modal, Skeleton, SidebarNav } from "@crewlethq/ui";
import { ChunkLoadError } from "./lazyScreen.ts";

/** Reload the page — the one fix for a chunk from another engine version. */
function reload(): void {
  location.reload();
}

/**
 * A chunk that did not arrive: what happened, and the two ways out.
 *
 * RELOAD FIRST, because the usual cause is an upgrade and nothing else fixes
 * that. TRY AGAIN beside it, because the other cause is a dropped request,
 * and `lazyScreen.ts` forgets a failed load so a retry really does ask again.
 */
function ChunkFailed({ error, reset }: { error: ChunkLoadError; reset: () => void }) {
  return (
    <Callout
      variant="neutral"
      role="alert"
      title={error.title}
      action={
        <span className="boundary-actions">
          <Button variant="primary" size="small" onClick={reload}>
            Reload
          </Button>
          <Button variant="ghost" size="small" onClick={reset}>
            Try again
          </Button>
        </span>
      }
    >
      {error.advice}
    </Callout>
  );
}

/**
 * The inner half of a screen's or a peek's pair: it draws a chunk that never
 * came and RETHROWS everything else.
 *
 * A boundary whose own fallback throws hands the error to the boundary above
 * it — React's contract — so the outer one, the kit's plain block with the
 * message a report needs, draws every other failure exactly as the kit draws
 * it, and nothing here keeps a second copy of that block.
 */
function chunkOnly(error: Error, reset: () => void): ReactNode {
  if (!(error instanceof ChunkLoadError)) throw error;
  return <ChunkFailed error={error} reset={reset} />;
}

/**
 * The pair every routed region gets: the kit's block outside, the chunk's
 * sentence inside, and the suspense boundary innermost.
 *
 * THE ERROR BOUNDARIES ARE OUTSIDE THE SUSPENSE, so a chunk that rejects —
 * which `Suspense` does not catch — lands in them.
 */
function Guarded({
  what,
  loading,
  rows,
  resetKey,
  children,
}: {
  what: string;
  loading: string;
  rows: number;
  resetKey: string;
  children: ReactNode;
}) {
  return (
    <ErrorBoundary resetKey={resetKey} title={`${what} could not be drawn`}>
      <ErrorBoundary resetKey={resetKey} fallback={chunkOnly}>
        <Suspense fallback={<Skeleton variant="text" rows={rows} label={loading} />}>
          {children}
        </Suspense>
      </ErrorBoundary>
    </ErrorBoundary>
  );
}

/**
 * The routed screen. `resetKey` is the resolved path: a new path is a new
 * screen and gets a fresh render, while a change to the query string alone (a
 * filter, an open peek) is the same screen and keeps its failure on view
 * rather than flickering back into the same throw.
 */
export function ScreenBoundary({ resetKey, children }: { resetKey: string; children: ReactNode }) {
  return (
    <Guarded what="This screen" loading="Loading this screen" rows={6} resetKey={resetKey}>
      {children}
    </Guarded>
  );
}

/**
 * A column a workspace draws beside its screens from its own chunk — the
 * Knowledge tree. Reset by the WORKSPACE rather than the path: the column
 * outlives every navigation inside it (that is what keeps its expanded
 * folders), so a navigation is not a reason to redraw one that threw, and
 * leaving the workspace unmounts it anyway.
 */
export function ColumnBoundary({
  what,
  resetKey,
  children,
}: {
  what: string;
  resetKey: string;
  children: ReactNode;
}) {
  return (
    <Guarded what={what} loading={`Loading ${what.toLowerCase()}`} rows={8} resetKey={resetKey}>
      {children}
    </Guarded>
  );
}

/**
 * The peek's body, reset by the object it shows, so `[` and `]` step off a
 * peek that threw onto one that may not.
 */
export function PeekBoundary({ resetKey, children }: { resetKey: string; children: ReactNode }) {
  return (
    <Guarded what="This preview" loading="Loading this preview" rows={4} resetKey={resetKey}>
      {children}
    </Guarded>
  );
}

/**
 * A layer the frame raises over the screen: the command palette, the New task
 * sheet. Its fallback is a DIALOG, not a block: the layer is over the screen,
 * and a block drawn where it was mounted would land below the frame, out of
 * sight, with the layer's own focus trap gone. The dialog closes the way the
 * layer does, and the next press opens a fresh one.
 *
 * A LAYER'S CODE CAN ARRIVE IN A CHUNK (the sheet is the Work workspace's), so
 * a load in flight draws nothing — the layer is not on screen until it is —
 * and a chunk that never came is this dialog, carrying the chunk's own
 * sentence about reloading.
 */
export function LayerBoundary({
  title,
  onClose,
  children,
}: {
  /** What could not be drawn: "Search", "New task". */
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  return (
    <ErrorBoundary
      fallback={(error) => (
        <Modal
          open
          onClose={onClose}
          size="sm"
          title={`${title} could not be drawn`}
          footer={
            error instanceof ChunkLoadError ? (
              <>
                <Button variant="secondary" onClick={onClose}>
                  Close
                </Button>
                <Button variant="primary" onClick={reload}>
                  Reload
                </Button>
              </>
            ) : (
              <Button variant="secondary" onClick={onClose}>
                Close
              </Button>
            )
          }
        >
          <p className="boundary-message">
            {error instanceof ChunkLoadError ? error.advice : error.message || error.name}
          </p>
        </Modal>
      )}
    >
      <Suspense fallback={null}>{children}</Suspense>
    </ErrorBoundary>
  );
}

/**
 * One live section of the sidebar. Its fallback keeps the section's heading,
 * so the reader can see WHICH list is missing, and a compact Try again sized
 * for the rail rather than the kit's panel-sized block.
 *
 * `resetKey` is the path, as for the screen: the sidebar is re-rendered on
 * every navigation, and a section that threw on one screen's data gets a
 * fresh chance on the next.
 */
export function RailBoundary({
  label,
  resetKey,
  children,
}: {
  label: string;
  resetKey: string;
  children: ReactNode;
}) {
  return (
    <ErrorBoundary
      resetKey={resetKey}
      fallback={(_error, reset) => (
        <SidebarNav.Group label={label}>
          <p className="side-failed" role="alert">
            Could not be drawn.{" "}
            <Button variant="ghost" size="small" onClick={reset}>
              Try again
            </Button>
          </p>
        </SidebarNav.Group>
      )}
    >
      {children}
    </ErrorBoundary>
  );
}
