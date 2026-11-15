/**
 * The dialog every screen opens: the kit's `Modal`, with a place for what its
 * write controls were refused, and the one sentence for why it cannot be
 * closed while its write is out ([useWaiting]).
 *
 * A DIALOG'S HEAD AND FOOT ARE ROWS OF CONTROLS. A write control there drew
 * its refusal beside itself (`WriteButton`), in the foot's own wrapping row,
 * so the primary action and the refusal dropped onto left-aligned lines of
 * their own under the foot and left Cancel alone at its end. Here the foot,
 * the foot's start and the head's actions send their controls' refusals to a
 * place at the end of the body, above the foot, and the foot keeps its row
 * ([useRefusalPlace]).
 *
 * EVERY DIALOG OPENS THROUGH HERE (`app/source.test.ts`): one opened on the
 * kit's `Modal` itself has no such place, and the first refusal of a control
 * in its foot would break the foot again.
 */

import type { ReactNode } from "react";
import {
  ConfirmModal as KitConfirmModal,
  Modal as KitModal,
  type ConfirmModalProps,
  type ModalProps,
} from "@crewlethq/ui";
import { useWaiting } from "~/lib/waiting.ts";
import { RefusalsTo, useRefusalPlace } from "./WriteButton.tsx";

export type { ConfirmModalProps, ModalProps };

export function Modal({
  children,
  footer,
  footerStart,
  headerActions,
  closeDisabledReason,
  ...rest
}: ModalProps) {
  const refusals = useRefusalPlace();
  const waiting = useWaiting();
  // A SLOT LEFT EMPTY STAYS EMPTY: the kit draws no band for a slot it was
  // not handed, and a wrapper around nothing would be handed one.
  const routed = (controls: ReactNode) =>
    controls === undefined || controls === null || controls === false || controls === "" ? (
      controls
    ) : (
      <RefusalsTo place={refusals.element}>{controls}</RefusalsTo>
    );
  return (
    <KitModal
      {...rest}
      // ONE SENTENCE FOR WHY IT CANNOT CLOSE, from where every dialog's
      // waiting words live; a dialog states its own only for a wait of its own.
      closeDisabledReason={closeDisabledReason ?? waiting.reason}
      footer={routed(footer)}
      footerStart={routed(footerStart)}
      headerActions={routed(headerActions)}
    >
      {children}
      {refusals.place}
    </KitModal>
  );
}

/** The kit's confirmation, saying why it cannot close in the same words. */
export function ConfirmModal({ busyReason, ...rest }: ConfirmModalProps) {
  const waiting = useWaiting();
  return <KitConfirmModal {...rest} busyReason={busyReason ?? waiting.reason} />;
}
