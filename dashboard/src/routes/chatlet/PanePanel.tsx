/**
 * The panel every chat pane draws in: the kit's `SidePanel`, set the one way
 * the chat screen sets it.
 *
 * ONE SETTING FOR FOUR PANES, so a thread, a room's details, a profile and a
 * catch up cannot come to differ in what their close control is called, which
 * class the frame lays them out by, or whether a press of Close and a press of
 * Escape do the same thing. Focus, Escape and the hand-back are the kit's.
 */

import type { ComponentProps, ReactNode } from "react";

import { SidePanel } from "@crewlethq/ui";

/** The class every pane's panel carries, which the frame lays out by. */
export const PANE_CLASS = "chat-pane";

type PanelProps = ComponentProps<typeof SidePanel>;

export function ChatSidePanel({
  title,
  subtitle,
  icon,
  actions,
  footer,
  flush,
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
  initialFocus?: PanelProps["initialFocus"];
  onClose: () => void;
  children: ReactNode;
}) {
  return (
    <SidePanel
      className={PANE_CLASS}
      title={title}
      subtitle={subtitle}
      icon={icon}
      actions={actions}
      footer={footer}
      flush={flush}
      initialFocus={initialFocus}
      onClose={onClose}
      labels={{ close: () => "Close" }}
    >
      {children}
    </SidePanel>
  );
}
