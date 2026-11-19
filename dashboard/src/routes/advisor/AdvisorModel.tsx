/** The Advisor's model in its page's head: the picker for an administrator (`components/HelperModel.tsx`). */

import { HelperModel, useHelperModel } from "~/components/HelperModel.tsx";

export function AdvisorModel() {
  const model = useHelperModel("advisor");
  return (
    <HelperModel
      helper="advisor"
      current={model.current}
      choices={model.choices}
      canAssign={model.canAssign}
      onApplied={model.reload}
    />
  );
}
