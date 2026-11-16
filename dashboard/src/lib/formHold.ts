/**
 * Why a form cannot be sent yet. A refused value names its field, which draws
 * the sentence as its error; a hold about the form (nothing typed or changed) holds only the submit.
 */
export interface FormHold<F extends string = string> {
  /** The sentence the submit is held with, and the field's error where it names one. */
  readonly reason: string;
  /** The field whose value is refused; absent for a hold about the form itself. */
  readonly field?: F;
}

/** A hold about the form: nothing is wrong with any one field. */
export function formHeld(reason: string): FormHold<never> {
  return { reason };
}

/** A hold on `field`'s value, said under that field. */
export function fieldHeld<F extends string>(field: F, reason: string): FormHold<F> {
  return { reason, field };
}

/** The sentence a submit is held with, or undefined for a form that can be sent. */
export function holdReason(hold: FormHold | undefined): string | undefined {
  return hold?.reason;
}

/** The error `field` draws: the hold's sentence where its value is the one refused. */
export function fieldError<F extends string>(
  hold: FormHold<F> | undefined,
  field: F,
): string | undefined {
  return hold?.field === field ? hold.reason : undefined;
}
