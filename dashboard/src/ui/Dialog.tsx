/**
 * The modal shell every dialog in the dashboard uses.
 *
 * Generalized from the token dialog rather than copied beside it: that one is
 * the only modal the dashboard had, and a second hand-rolled `.veil` plus
 * `role="dialog"` is how two dialogs start disagreeing about which one closes
 * on Escape and which one traps focus.
 *
 * What the shell owns, because every dialog needs it and none should restate
 * it: the veil and its click-outside, Escape, focus moved into the dialog on
 * open and returned to whatever opened it on close, `aria-modal` with a label,
 * and the head/body/foot structure the stylesheet already carries.
 *
 * What it deliberately does NOT own: what the dialog says, what its buttons
 * do, and whether closing is allowed. A dialog mid-write passes
 * `dismissable={false}` so a stray click cannot abandon a request whose
 * outcome the operator has not seen.
 */

import { useEffect, useRef, type ReactNode } from "react";
import { Icon, type IconName } from "~/ui/Icon.tsx";

export function Dialog({
  title,
  icon,
  onClose,
  children,
  footer,
  width = 480,
  dismissable = true,
  onSubmit,
}: {
  title: string;
  icon?: IconName;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: number;
  /** False while a request is in flight: the veil and Escape stop closing. */
  dismissable?: boolean;
  /** When given, the shell is a form and Enter submits it. */
  onSubmit?: () => void;
}) {
  // HTMLElement, because the shell is a <form> when it submits and a <div>
  // when it does not, and one ref has to reach whichever was rendered.
  const ref = useRef<HTMLElement | null>(null);
  // WHERE FOCUS CAME FROM, so it can go back. A dialog that returns focus to
  // the document body leaves a keyboard reader at the top of the page, which
  // on this dashboard means the nav rather than the row they opened.
  const opener = useRef<Element | null>(null);

  useEffect(() => {
    opener.current = document.activeElement;
    // The first control, or the dialog itself when it has none, so a screen
    // reader announces the label rather than continuing behind the veil.
    const focusable = ref.current?.querySelector<HTMLElement>(
      "input, select, textarea, button, [href], [tabindex]:not([tabindex='-1'])",
    );
    (focusable ?? ref.current)?.focus();
    return () => {
      const back = opener.current;
      if (back instanceof HTMLElement && document.contains(back)) back.focus();
    };
  }, []);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape" && dismissable) {
        e.stopPropagation();
        onClose();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [dismissable, onClose]);

  const body = (
    <>
      <header className="dialog-head">
        {icon && <Icon name={icon} size="sm" />}
        <strong style={{ fontSize: "var(--fs-sm)" }}>{title}</strong>
      </header>
      <div className="dialog-body col gap-3">{children}</div>
      {footer && <footer className="dialog-foot">{footer}</footer>}
    </>
  );

  const shell = {
    className: "dialog",
    style: { width: `min(${width}px, 100%)` },
    role: "dialog" as const,
    "aria-modal": true,
    "aria-label": title,
    tabIndex: -1,
    ref: (el: HTMLElement | null) => {
      ref.current = el;
    },
    onMouseDown: (e: React.MouseEvent) => e.stopPropagation(),
  };

  return (
    <div className="veil" onMouseDown={() => dismissable && onClose()} role="presentation">
      {onSubmit ? (
        <form
          {...shell}
          onSubmit={(e) => {
            e.preventDefault();
            onSubmit();
          }}
        >
          {body}
        </form>
      ) : (
        <div {...shell}>{body}</div>
      )}
    </div>
  );
}
