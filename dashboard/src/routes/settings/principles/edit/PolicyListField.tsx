/**
 * The standing policies, open for writing: the kit's ordered list, every
 * policy one line edited where it stands, added with Enter, moved by its grip,
 * with Alt and an arrow or with the move buttons, and removed with its own
 * control, which offers Undo where the policy was (`ListInput`). The list
 * holds at most [CHARTER_LIMITS.policies] and says so where the next one
 * would be written once it is full.
 *
 * THE NEW-ITEM BOX IS PART OF THE DRAFT (`draft`): a policy typed there and
 * not yet added with Enter is counted as a change and saved with the rest,
 * rather than dropped by a Save pressed before Enter.
 *
 * The list's own rules (one per policy, said under it) and the list's length
 * (said under the list) come from `charter.ts`; this only places them.
 */

import { ListInput } from "@crewlethq/ui";
import { CHARTER_LIMITS, PART_HELP, PART_TITLES, type PartProblems } from "./charter.ts";

export function PolicyListField({
  value,
  draft,
  problems,
  disabled,
  onChange,
  onDraftChange,
}: {
  value: readonly string[];
  /** The text in the new-item box. */
  draft: string;
  problems: PartProblems;
  disabled: boolean;
  onChange: (items: string[]) => void;
  onDraftChange: (text: string) => void;
}) {
  return (
    <ListInput
      // THE CARD'S TITLE IS THE VISIBLE NAME, right above the list; the
      // legend stays for a reader moving by form controls.
      label={PART_TITLES.policies}
      hideLabel
      itemName="policy"
      value={value}
      onChange={onChange}
      draft={draft}
      onDraftChange={onDraftChange}
      helper={PART_HELP.policies}
      error={problems.field}
      itemError={(index) => problems.items?.[index]}
      maxItems={CHARTER_LIMITS.policies}
      maxLength={CHARTER_LIMITS.policy}
      reorderByDrag
      undoRemove
      disabled={disabled}
      placeholder="Write a policy, then press Enter"
    />
  );
}
