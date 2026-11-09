/**
 * The panel every chat pane draws in: the kit's `SidePanel`, set the one way
 * the chat screen sets it, beside the conversation or as a list's detail
 * ([ChatDetailPlacement]).
 *
 * ONE SETTING FOR FOUR PANES, so a thread, a room's details, a profile and a
 * catch up cannot come to differ in what their close control is called, which
 * class the frame lays them out by, or whether a press of Close and a press of
 * Escape do the same thing. Focus, Escape and the hand-back are the kit's.
 */

import { createContext, useContext, type ComponentProps, type ReactNode } from "react";

import { ListDetailBack, SidePanel } from "@crewlethq/ui";

/** The class every pane's panel carries, which the frame lays out by. */
export const PANE_CLASS = "chat-pane";

/**
 * The class a pane carries where it is a list's detail (Activity): a handle
 * the suites find it by. The kit's `fill` sizes it; no rule here does.
 */
export const DETAIL_PANE_CLASS = "chat-detail-pane";

/**
 * A pane drawn as a list's detail rather than beside the conversation: it
 * fills the detail rather than the frame's pane column, and carries the way
 * back to the list (`back` names it) for a phone, where the list is folded
 * out of sight.
 */
const DetailPlacement = createContext<{ back: string } | null>(null);

export function ChatDetailPlacement({ back, children }: { back: string; children: ReactNode }) {
  return <DetailPlacement.Provider value={{ back }}>{children}</DetailPlacement.Provider>;
}

type PanelProps = ComponentProps<typeof SidePanel>;

export function ChatSidePanel({
  title,
  subtitle,
  icon,
  actions,
  footer,
  flush,
  flow = false,
  initialFocus,
  onClose,
  children,
}: {
  title: string;
  /** A second line on the title's baseline: where a thread was said, a room's kind. */
  subtitle?: ReactNode;
  icon?: ReactNode;
  actions?: ReactNode;
  footer?: ReactNode;
  /** For content that scrolls and insets itself, a transcript. */
  flush?: boolean;
  /**
   * A conversation whose box follows it: the body takes its content's height,
   * so in a short thread the reply box sits under the last reply, and shrinks
   * to scroll once the thread is taller than the pane.
   */
  flow?: boolean;
  initialFocus?: PanelProps["initialFocus"];
  onClose: () => void;
  children: ReactNode;
}) {
  const detail = useContext(DetailPlacement);
  return (
    <SidePanel
      className={detail ? DETAIL_PANE_CLASS : PANE_CLASS}
      // AS A LIST'S DETAIL IT FILLS ITS COLUMN by the kit's own rule: a rule
      // here would lose to the kit's sheet, which a built page links last.
      fill={detail !== null}
      leading={detail ? <ListDetailBack label={detail.back} onClick={onClose} /> : undefined}
      title={title}
      subtitle={subtitle}
      icon={icon}
      actions={actions}
      footer={footer}
      flush={flush}
      flow={flow}
      density="chat"
      initialFocus={initialFocus}
      onClose={onClose}
      labels={{ close: () => "Close" }}
    >
      {children}
    </SidePanel>
  );
}
