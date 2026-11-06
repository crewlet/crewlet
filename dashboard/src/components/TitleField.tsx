/**
 * A title being written: one line of meaning, drawn so it can be read whole.
 *
 * A one-line `Input` scrolls a long value sideways, and titles here routinely
 * wrap to three lines, so nobody could review what they typed. This is the kit's
 * one-line textarea: it grows with its words, and Enter inserts no line break and
 * a pasted one folds into a space. A textarea has no implicit submit, so Enter is
 * handed to `onEnter`.
 */

import { forwardRef, type KeyboardEvent } from "react";
import { Textarea, isComposing, type TextareaProps } from "@crewlethq/ui";

export interface TitleFieldProps extends Omit<TextareaProps, "singleLine" | "autoResize" | "rows"> {
  /** A plain Enter: what the surrounding form does with it. */
  onEnter: () => void;
}

export const TitleField = forwardRef<HTMLTextAreaElement, TitleFieldProps>(function TitleField(
  { onEnter, onKeyDown, ...rest },
  ref,
) {
  const keys = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    onKeyDown?.(event);
    if (event.defaultPrevented || event.key !== "Enter") return;
    if (event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) return;
    if (isComposing(event)) return;
    event.preventDefault();
    onEnter();
  };
  return (
    <Textarea
      ref={ref}
      foldedMessage="Line breaks were replaced with spaces: a title holds one line."
      {...rest}
      singleLine
      autoResize
      rows={1}
      onKeyDown={keys}
    />
  );
});
