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
 *
 * A SCREEN WITH A BAR OF ITS OWN MAY STAND THE STACK UNDER IT (`useToastTop`):
 * Chatlet's composers sit at the window's foot and grow as they are typed in,
 * so no fixed inset from the foot clears them, while under its top bar's
 * right end is only the transcript or a pane's content. While a screen holds
 * the top, the stack stands top right, held off the window's top by where the
 * bar ends; a panel's width still keeps it beside the panel, and a foot's
 * height, which keeps clear of the other edge, no longer applies.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { ToastProvider, type ToasterInset } from "@crewlethq/ui";

/** Who keeps room: the frame (a panel's width) or a panel itself (its foot). */
export type InsetOwner = "frame" | "panel";

type Request = (owner: InsetOwner, inset: ToasterInset | null) => void;

/** A screen's claim on the top edge: where its bar ends, in px, or null to let go. */
type TopRequest = (bottom: number | null) => void;

const InsetRequest = createContext<Request>(() => {});
const TopEdge = createContext<TopRequest>(() => {});

/** The app's toast host, keeping clear whatever its owners asked for. */
export function InsetToastProvider({ children }: { children: ReactNode }) {
  const [asked, setAsked] = useState<Partial<Record<InsetOwner, ToasterInset>>>({});
  const [top, setTop] = useState<number | null>(null);
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
    if (all.length === 0 && top === null) return undefined;
    return {
      inline: Math.max(0, ...all.map((i) => i.inline ?? 0)),
      // THE EDGE THE STACK STANDS ON decides whose block applies: the bar's
      // bottom at the top, a foot's height at the bottom.
      block: top ?? Math.max(0, ...all.map((i) => i.block ?? 0)),
    };
  }, [asked, top]);
  return (
    <InsetRequest.Provider value={request}>
      <TopEdge.Provider value={setTop}>
        <ToastProvider position={top === null ? "bottom-right" : "top-right"} inset={inset}>
          {children}
        </ToastProvider>
      </TopEdge.Provider>
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

/**
 * Stand the toast stack under the bar `bar` holds while this component is
 * mounted, held off the window's top by where the bar ends: measured now,
 * whenever the bar changes size, and whenever the window does. Let go when
 * the component goes. One screen holds the top at a time, the one mounted.
 */
export function useToastTop(bar: RefObject<HTMLElement | null>): void {
  const claim = useContext(TopEdge);
  useLayoutEffect(() => {
    const el = bar.current;
    if (!el) return;
    const measure = () => claim(Math.max(0, Math.round(el.getBoundingClientRect().bottom)));
    measure();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(el);
    window.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
      claim(null);
    };
  }, [bar, claim]);
}
