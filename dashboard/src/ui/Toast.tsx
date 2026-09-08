/**
 * What happened after a write.
 *
 * Every screen in this dashboard was a read until now, so there was no place
 * to say "saved" or "the engine refused that" — the stylesheet has carried
 * `.toast-host` and `.toast` for a while with nothing rendering them. A write
 * without an outcome surface is a button an operator presses twice.
 *
 * A toast is for the outcome of something the operator just did, and nothing
 * else. It is NOT for state: a degraded integration, a lost socket and an
 * unresolved secret all belong on the screen that owns them, where they stay
 * visible after the four seconds are up. Anything a reader would want to find
 * again must not be a toast.
 *
 * A FAILURE DOES NOT AUTO-DISMISS. A success is a confirmation of something
 * the operator already knows they asked for; a failure is news, and news that
 * removes itself is news somebody misses while reading the form they were
 * about to fix.
 */

import { createContext, useCallback, useContext, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Icon } from "~/ui/Icon.tsx";

export type ToastTone = "positive" | "critical";

interface Toast {
  id: number;
  tone: ToastTone;
  text: string;
}

interface ToastAPI {
  /** Announce a completed write. Dismisses itself. */
  ok: (text: string) => void;
  /** Announce a refusal. Stays until dismissed. */
  failed: (text: string) => void;
}

const noop: ToastAPI = { ok: () => {}, failed: () => {} };
const ToastContext = createContext<ToastAPI>(noop);

/** The hook every writing screen uses. Safe with no provider mounted. */
export function useToast(): ToastAPI {
  return useContext(ToastContext);
}

/** How long a success stays. Long enough to read one line, short enough not
 *  to sit over the row the operator is looking at next. */
const SUCCESS_MS = 4000;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const next = useRef(1);

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((t) => t.id !== id));
  }, []);

  const push = useCallback(
    (tone: ToastTone, text: string) => {
      const id = next.current++;
      setToasts((current) => [...current, { id, tone, text }]);
      if (tone === "positive") {
        window.setTimeout(() => dismiss(id), SUCCESS_MS);
      }
    },
    [dismiss],
  );

  const api = useMemo<ToastAPI>(
    () => ({
      ok: (text: string) => push("positive", text),
      failed: (text: string) => push("critical", text),
    }),
    [push],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      {/* POLITE, not assertive: a save confirmation must not interrupt a
          screen reader mid-sentence. A refusal is still announced, and it
          stays on screen, which is the part that matters. */}
      <div className="toast-host" role="status" aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className={t.tone === "critical" ? "toast critical" : "toast positive"}>
            <Icon name={t.tone === "critical" ? "alert" : "check"} size="sm" />
            <span className="truncate" style={{ flex: 1 }}>
              {t.text}
            </span>
            <button
              type="button"
              className="toast-close"
              aria-label="Dismiss"
              onClick={() => dismiss(t.id)}
            >
              <Icon name="x" size="sm" />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
