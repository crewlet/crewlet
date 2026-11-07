/** A connect form's sentences with their `{link}` drawn, shared by field help and Setup steps. */

import { Fragment, type ReactNode } from "react";
import { marked } from "~/ui/Problems.tsx";

/** Where a field's vendor link sits in its words (`setup.LinkMark`). */
export const LINK_MARK = "{link}";

/** A field's vendor link, resolved: its address ("" until it has one) and its words. */
export interface FieldLink {
  href: string;
  text: string;
}

/**
 * The engine's words with `{link}` drawn as its link and other literals marked
 * ([marked]); a link with no address yet keeps its words.
 */
export function withLink(text: string, link: FieldLink | undefined): ReactNode {
  const parts = text.split(LINK_MARK);
  return parts.map((part, i) => (
    <Fragment key={i}>
      {marked(part)}
      {i < parts.length - 1 &&
        (link?.href ? (
          <a className="prose-link" href={link.href} target="_blank" rel="noreferrer">
            {link.text}
          </a>
        ) : (
          (link?.text ?? "")
        ))}
    </Fragment>
  ));
}
