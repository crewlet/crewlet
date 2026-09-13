/**
 * A side sheet: a modal surface that slides in from the right edge.
 *
 * WHY IT EXISTS. A dialog is sized for a question; an editor with a dozen
 * fields, a list or two and the refusals beside them needs the full height of
 * the window, and it needs the thing being edited to stay in view at its side
 * so the operator keeps their place. A centred dialog grown to hold that
 * covers the very card it is editing.
 *
 * THE RULES IT KEEPS.
 *
 * - It is a MODAL on the same stack as the dialog (`useModal`), so a prompt
 *   raised over it ("discard these edits?") closes on its own Escape and
 *   leaves the drawer open, Tab stays inside, and focus returns to whatever
 *   opened it.
 * - Focus starts on the first control in the BODY, not on the Close button
 *   that comes first in the header.
 * - It shares the dialog's panel element (`ModalPanel`), so the two announce
 *   the same way and submit on Enter the same way.
 * - It sits on `--z-modal`, never on `--z-drawer`: that layer belongs to the
 *   navigation rail, and a modal surface below the popover layer could be
 *   covered by a menu it did not open.
 * - FULL WIDTH below the shell's one breakpoint, where a side sheet no longer
 *   has content beside it.
 * - Its Close button honours `dismissable` exactly as Escape and the veil do:
 *   a drawer mid-write cannot be abandoned by any of the three.
 */

import { useRef, type ReactNode } from "react";
import { ModalPanel } from "./Dialog.tsx";
import { Icon, type IconName } from "./Icon.tsx";
import { Button } from "./primitives.tsx";
import { focusables, useModal } from "./useModal.ts";

export function Drawer({
  title,
  icon,
  onClose,
  children,
  footer,
  dismissable = true,
  onSubmit,
}: {
  title: string;
  icon?: IconName;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  /** False while a request is in flight: the veil, Escape and Close stop closing. */
  dismissable?: boolean;
  /** When given, the sheet is a form and Enter submits it. */
  onSubmit?: () => void;
}) {
  const body = useRef<HTMLDivElement>(null);
  const modal = useModal({
    onClose,
    dismissable,
    initialFocus: () => (body.current ? (focusables(body.current)[0] ?? null) : null),
  });
  return (
    <div className="veil sheet-veil" ref={modal.veilRef} role="presentation">
      <ModalPanel className="sheet" title={title} panelRef={modal.panelRef} onSubmit={onSubmit}>
        <header className="sheet-head">
          {icon && <Icon name={icon} size="sm" />}
          <strong className="sheet-title truncate">{title}</strong>
          <span className="spacer" />
          <Button
            variant="ghost"
            size="sm"
            icon="x"
            title="Close"
            onClick={onClose}
            disabled={!dismissable}
          />
        </header>
        <div className="sheet-body col gap-3" ref={body}>
          {children}
        </div>
        {footer && <footer className="sheet-foot">{footer}</footer>}
      </ModalPanel>
    </div>
  );
}
