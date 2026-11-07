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
 *
 * A BAR DOCKED AT THE WINDOW'S FOOT ASKS TOO (`useToastClearOf`): on a phone a
 * selection's bar stands where the stack does, and a toast raised by a press
 * on it lay over the controls the next press needs.
 *
 * THE PAGE KEEPS ROOM FOR THE STACK AT ITS FOOT (`useToastRoom`, `ToastRoom`).
 * A toast stands over the page, so after a press on a table's last row its
 * toast covered that row's controls for as long as it showed. THE ROOM IS
 * ALWAYS THERE, one toast's height over whatever a bar holds at the foot: made
 * as a toast came, it moved the page up under the pointer and back as it went.
 * Only a stack taller than that grows the page's end, keeping a reader at the
 * end there. Each stack reads its height off itself, the kit's toaster in its
 * own layer: it is always mounted, and is as tall as the toasts it holds. The
 * page keeps the largest.
 *
 * A SURFACE WITH A LAYER OF ITS OWN HOSTS ITS STACK THROUGH THE APP'S
 * (`SurfaceToastProvider`). A fullscreen surface such as the org builder
 * mounts a provider inside its `LayerHost`, since a toast drawn outside a
 * fullscreen element is not painted; but a bare provider there kept clear of
 * nothing the app's host is asked for, so on Structure a toast lay over the
 * assistant's panel, and its stack was one the page made no room for. It
 * takes the app's edge and inset, and says its room to the page.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useId,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { ToastProvider, useLayerContainer, type ToasterInset } from "@crewlethq/ui";
import { screenScroller } from "~/lib/scroller.ts";

/** Who keeps room: the frame (a panel's width), a panel itself (its foot), or a bar docked there. */
export type InsetOwner = "frame" | "panel" | "bar";

type Request = (owner: InsetOwner, inset: ToasterInset | null) => void;

/** A screen's claim on the top edge: where its bar ends, in px, or null to let go. */
type TopRequest = (bottom: number | null) => void;

const InsetRequest = createContext<Request>(() => {});
const TopEdge = createContext<TopRequest>(() => {});

/** A stack's room at the window's foot, said under its id; 0 withdraws it. */
type RoomReport = (id: string, room: number) => void;

/** What the app's host hands every stack: where it stands, what it keeps clear, and where its room goes. */
interface Host {
  readonly position: "bottom-right" | "top-right";
  readonly inset: ToasterInset | undefined;
  readonly report: RoomReport;
}

const HostContext = createContext<Host>({
  position: "bottom-right",
  inset: undefined,
  report: () => {},
});
const Room = createContext(0);

/**
 * The height a stack at the window's foot covers, from its top to the
 * window's bottom, in px: none while it holds no toast.
 */
export function roomOf(stack: DOMRect, viewport: number): number {
  return stack.height > 0 ? Math.max(0, Math.ceil(viewport - stack.top)) : 0;
}

/** The kit's toaster drawn into `layer`: the one directly in it, never a nested surface's. */
function stackIn(layer: HTMLElement): HTMLElement | null {
  return layer.querySelector<HTMLElement>(":scope > .crewlet-toaster");
}

/** The app's toast host, keeping clear whatever its owners asked for. */
export function InsetToastProvider({ children }: { children: ReactNode }) {
  const [asked, setAsked] = useState<Partial<Record<InsetOwner, ToasterInset>>>({});
  const [top, setTop] = useState<number | null>(null);
  const [rooms, setRooms] = useState<Readonly<Record<string, number>>>({});
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
  const report = useCallback<RoomReport>((id, room) => {
    setRooms((now) => {
      if ((now[id] ?? 0) === room) return now;
      const next = { ...now };
      if (room === 0) delete next[id];
      else next[id] = room;
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
  const host = useMemo<Host>(
    () => ({ position: top === null ? "bottom-right" : "top-right", inset, report }),
    [top, inset, report],
  );
  return (
    <InsetRequest.Provider value={request}>
      <TopEdge.Provider value={setTop}>
        <HostContext.Provider value={host}>
          <Room.Provider value={Math.max(0, ...Object.values(rooms))}>
            <HostedStack>{children}</HostedStack>
          </Room.Provider>
        </HostContext.Provider>
      </TopEdge.Provider>
    </InsetRequest.Provider>
  );
}

/**
 * The toast host for a surface that draws its own layer (`LayerHost`), such
 * as the org builder, whose fullscreen paints nothing outside itself: its
 * stack stands where the app's does, keeps clear of what the app's keeps
 * clear of, and its room is the page's too.
 */
export function SurfaceToastProvider({ children }: { children: ReactNode }) {
  return <HostedStack>{children}</HostedStack>;
}

/** A provider drawing its stack at the app host's edge and inset, its room said to the page. */
function HostedStack({ children }: { children: ReactNode }) {
  const { position, inset } = useContext(HostContext);
  return (
    <ToastProvider position={position} inset={inset}>
      <StackRoom atFoot={position === "bottom-right"} />
      {children}
    </ToastProvider>
  );
}

/** Says the room its layer's stack covers at the foot as it changes, and withdraws it when it goes. */
function StackRoom({ atFoot }: { atFoot: boolean }) {
  const id = useId();
  const { report } = useContext(HostContext);
  const layer = useLayerContainer();
  useLayoutEffect(() => {
    const stack = layer && atFoot ? stackIn(layer) : null;
    if (!stack) return;
    const measure = () => report(id, roomOf(stack.getBoundingClientRect(), window.innerHeight));
    measure();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(stack);
    window.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
      report(id, 0);
    };
  }, [layer, atFoot, id, report]);
  return null;
}

/** The room the largest stack at the window's foot covers, now ([roomOf]). */
export function useToastRoom(): number {
  return useContext(Room);
}

/**
 * Whether a reader whose scroller now ends `distance` px below its view was
 * at its end before the page's end grew by `grew` px.
 */
export function wasAtEnd(distance: number, grew: number): boolean {
  return grew > 0 && distance <= grew + 1;
}

/**
 * The room the page's end always keeps: a toast naming a long seat, which wraps
 * to three lines (87px measured, at 1440 and at a phone's width), over the kit's
 * 24px window inset, so one toast at the foot lies over the room and moves nothing.
 */
export const TOAST_REST_ROOM = 112;

/**
 * The room at the page's end: drawn last in a scrolling screen, so the last
 * row can stand above the stack, and above a bar docked at the foot. A stack
 * standing at the top keeps none.
 */
export function ToastRoom() {
  const room = useToastRoom();
  const { position, inset } = useContext(HostContext);
  if (position !== "bottom-right") return null;
  return <RoomAtEnd room={Math.max(room, TOAST_REST_ROOM + (inset?.block ?? 0))} />;
}

function RoomAtEnd({ room }: { room: number }) {
  const ref = useRef<HTMLDivElement>(null);
  // WHAT THIS ROOM ADDED TO THE PAGE, its gap in the column included. Null
  // until it is drawn: the room a page opens with is part of the page, and a
  // reader on a short page is not at an end it took them to.
  const added = useRef<number | null>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    const scroller = screenScroller();
    if (!el || !scroller) return;
    const column = el.parentElement;
    const gap = column ? parseFloat(getComputedStyle(column).rowGap) || 0 : 0;
    const now = el.offsetHeight + gap;
    const grew = added.current === null ? 0 : now - added.current;
    added.current = now;
    if (wasAtEnd(scroller.scrollHeight - scroller.clientHeight - scroller.scrollTop, grew)) {
      scroller.scrollTop = scroller.scrollHeight;
    }
  }, [room]);
  return <div ref={ref} aria-hidden="true" style={{ height: room, flex: "none" }} />;
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
 * Keep the toast stack above a bar docked at the window's foot while `on`, as
 * high as the bar's top is off the foot: measured now, whenever the bar changes
 * size, and whenever the window does. Withdrawn when it goes.
 */
export function useToastClearOf(bar: () => HTMLElement | null, on: boolean): void {
  const [block, setBlock] = useState<number | null>(null);
  const find = useRef(bar);
  find.current = bar;
  useLayoutEffect(() => {
    const el = on ? find.current() : null;
    if (!el) {
      setBlock(null);
      return;
    }
    const measure = () =>
      setBlock(Math.max(0, Math.ceil(window.innerHeight - el.getBoundingClientRect().top)));
    measure();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(el);
    window.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, [on]);
  useToastInset("bar", block === null ? null : { block });
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
