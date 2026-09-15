/**
 * The modal shell every dialog in the dashboard uses.
 *
 * Generalized from the token dialog rather than copied beside it: that one is
 * the only modal the dashboard had, and a second hand-rolled `.veil` plus
 * `role="dialog"` is how two dialogs start disagreeing about which one closes
 * on Escape and which one traps focus.
 *
 * What the shell owns, because every dialog needs it and none should restate
 * it: the veil, `aria-modal` with a label, and the head/body/foot structure
 * the stylesheet already carries. Escape, the veil press, the Tab trap and
 * where focus goes on open and on close belong to `useModal`, which the
 * drawer shares, so a prompt raised over a drawer closes on its own Escape and
 * leaves the drawer open.
 *
 * What it deliberately does NOT own: what the dialog says, what its buttons
 * do, and whether closing is allowed. A dialog mid-write passes
 * `dismissable={false}` so a stray click cannot abandon a request whose
 * outcome the operator has not seen.
 */

import type { GlyphProps } from "@crewlethq/icons/glyphs";
import type { CSSProperties, ComponentType, ReactNode } from "react";
import { useModalLayer } from "@crewlethq/ui";

export function Dialog({
  title,
  icon: Glyph,
  onClose,
  children,
  footer,
  width = 480,
  dismissable = true,
  onSubmit,
}: {
  title: string;
  icon?: ComponentType<GlyphProps>;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: number;
  /** False while a request is in flight: the veil and Escape stop closing. */
  dismissable?: boolean;
  /** When given, the shell is a form and Enter submits it. */
  onSubmit?: () => void;
}) {
  const modal = useModalLayer({ onClose, dismissable });
  return (
    <div className="veil" ref={modal.veilRef} role="presentation">
      <ModalPanel
        className="dialog"
        style={{ width: `min(${width}px, 100%)` }}
        title={title}
        panelRef={modal.panelRef}
        onSubmit={onSubmit}
      >
        <header className="dialog-head">
          {Glyph && <Glyph size="sm" />}
          <strong style={{ fontSize: "var(--font-size-sm)" }}>{title}</strong>
        </header>
        <div className="dialog-body col gap-3">{children}</div>
        {footer && <footer className="dialog-foot">{footer}</footer>}
      </ModalPanel>
    </div>
  );
}

/**
 * The element a modal surface is: a labelled `role="dialog"`, and a form when
 * the surface submits.
 *
 * Shared by the dialog and the drawer so the two cannot drift on the ARIA a
 * screen reader announces or on how Enter submits.
 */
export function ModalPanel({
  className,
  style,
  title,
  panelRef,
  onSubmit,
  children,
}: {
  className: string;
  style?: CSSProperties;
  title: string;
  panelRef: (el: HTMLElement | null) => void;
  onSubmit?: () => void;
  children: ReactNode;
}) {
  const shell = {
    className,
    style,
    role: "dialog" as const,
    "aria-modal": true,
    "aria-label": title,
    tabIndex: -1,
    ref: panelRef,
  };
  return onSubmit ? (
    <form
      {...shell}
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit();
      }}
    >
      {children}
    </form>
  ) : (
    <div {...shell}>{children}</div>
  );
}
