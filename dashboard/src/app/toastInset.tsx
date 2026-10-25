/**
 * The room the toast stack keeps clear of the frame's own panels.
 *
 * The one toast host wraps the whole app (`App.tsx`), above the frame, so a
 * toast survives the navigation the write that raised it causes; but the
 * frame is what knows whether a panel stands at the window's edge, where the
 * stack would otherwise land on it. While the assistant is a column or over
 * the page, the stack stands beside it rather than over it; while it is the
 * whole window on a phone, the stack stands above its foot (the composer).
 *
 * A REQUEST PER OWNER, merged: the frame knows the width a panel takes and
 * the panel knows its own foot's height, and two writers of one value would
 * each undo the other's. Each says only its own part and withdraws it when it
 * goes; the host keeps the largest of each.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { ToastProvider, type ToasterInset } from "@crewlethq/ui";

/** Who keeps room: the frame (a panel's width) or a panel itself (its foot). */
export type InsetOwner = "frame" | "panel";

type Request = (owner: InsetOwner, inset: ToasterInset | null) => void;

const InsetRequest = createContext<Request>(() => {});

/** The app's toast host, keeping clear whatever its owners asked for. */
export function InsetToastProvider({ children }: { children: ReactNode }) {
  const [asked, setAsked] = useState<Partial<Record<InsetOwner, ToasterInset>>>({});
  const request = useCallback<Request>((owner, inset) => {
    setAsked((now) => {
      const same = now[owner]?.inline === inset?.inline && now[owner]?.block === inset?.block;
      if (same && (inset === null) === (now[owner] === undefined)) return now;
      const next = { ...now };
      if (inset === null) delete next[owner];
      else next[owner] = inset;
      return next;
    });
  }, []);
  const inset = useMemo<ToasterInset | undefined>(() => {
    const all = Object.values(asked);
    if (all.length === 0) return undefined;
    return {
      inline: Math.max(0, ...all.map((i) => i.inline ?? 0)),
      block: Math.max(0, ...all.map((i) => i.block ?? 0)),
    };
  }, [asked]);
  return (
    <InsetRequest.Provider value={request}>
      <ToastProvider inset={inset}>{children}</ToastProvider>
    </InsetRequest.Provider>
  );
}

/**
 * Keep `inset` clear of toasts while this component is mounted, as `owner`;
 * null asks for nothing. Withdrawn when the component goes.
 */
export function useToastInset(owner: InsetOwner, inset: ToasterInset | null): void {
  const request = useContext(InsetRequest);
  const inline = inset?.inline;
  const block = inset?.block;
  const none = inset === null;
  useEffect(() => {
    request(owner, none ? null : { inline, block });
  }, [request, owner, none, inline, block]);
  useEffect(() => () => request(owner, null), [request, owner]);
}
