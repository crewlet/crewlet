/**
 * The Advisor's model in its page's head: the picker for an administrator
 * (`components/HelperModel.tsx`), captioned with what it picks. Beside Run
 * now, a bare "None" read as nothing at all, and on a phone it sat alone on
 * its own line. A reader who may not choose is told the model in a sentence,
 * which needs no caption.
 */

import { Text } from "@crewlethq/ui";
import { HelperModel, useHelperModel } from "~/components/HelperModel.tsx";

export function AdvisorModel() {
  const model = useHelperModel("advisor");
  const picker = (
    <HelperModel
      helper="advisor"
      current={model.current}
      choices={model.choices}
      canAssign={model.canAssign}
      onApplied={model.reload}
    />
  );
  if (!model.canAssign) return picker;
  return (
    <span className="advisor-model">
      <Text as="span" variant="caption" tone="secondary">
        Explains with
      </Text>
      {picker}
    </span>
  );
}
