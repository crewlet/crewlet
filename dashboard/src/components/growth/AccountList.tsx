/**
 * Vendor accounts as a list that reads at any width: the account's name,
 * which is somebody's words and wraps between words; its ID, which is how two
 * accounts are told apart and is never broken; and a line of facts (product,
 * currency, zone, state, caps), each kept whole.
 */

import type { ReactNode } from "react";
import "~/styles/growth.css";

export interface AccountListRow {
  key: string;
  /** A mark before the name: the product's, where several products share the list. */
  mark?: ReactNode | undefined;
  name: string;
  id: string;
  /** The facts under the name, already in words. */
  facts: ReactNode[];
}

export function AccountList({ rows, label }: { rows: readonly AccountListRow[]; label: string }) {
  return (
    <ul className="growth-accounts" aria-label={label}>
      {rows.map((row) => (
        <li key={row.key} className="growth-account">
          {row.mark ? <span className="growth-account__mark">{row.mark}</span> : null}
          <span className="growth-account__body">
            <span className="growth-account__head">
              <span className="growth-account__name">{row.name || row.id}</span>
              {row.name && row.name !== row.id ? (
                <span className="growth-account__id">{row.id}</span>
              ) : null}
            </span>
            {row.facts.length > 0 ? (
              <span className="growth-account__facts">
                {row.facts.map((fact, index) => (
                  <span key={index} className="growth-account__fact">
                    {fact}
                  </span>
                ))}
              </span>
            ) : null}
          </span>
        </li>
      ))}
    </ul>
  );
}
