/**
 * The dialog every screen opens: the kit's `Modal`, with a place for what its
 * write controls were refused.
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
import { Modal as KitModal, type ModalProps } from "@crewlethq/ui";
import { RefusalsTo, useRefusalPlace } from "./WriteButton.tsx";

export type { ModalProps };

export function Modal({ children, footer, footerStart, headerActions, ...rest }: ModalProps) {
  const refusals = useRefusalPlace();
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
      footer={routed(footer)}
      footerStart={routed(footerStart)}
      headerActions={routed(headerActions)}
    >
      {children}
      {refusals.place}
    </KitModal>
  );
}
