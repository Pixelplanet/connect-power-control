/** The handler an `Input` takes for what a person types into it. */
export const typed =
  (set: (value: string) => void) =>
  (event: Event): void => {
    const target = event.currentTarget;
    if (target instanceof HTMLInputElement) set(target.value);
  };

/** A whole number a person typed, or `fallback` for anything else. */
export const wholeOf = (text: string, fallback: number): number => {
  const parsed = Number.parseInt(text, 10);

  return Number.isFinite(parsed) && parsed >= 0 ? parsed : fallback;
};

/**
 * A number a person typed with a dot or a comma, or undefined for anything
 * else. An empty text is not a number.
 */
export const decimalOf = (text: string): number | undefined => {
  const trimmed = text.trim();
  if (!/^(?:\d+[.,]?\d*|[.,]\d+)$/u.test(trimmed)) return undefined;
  const parsed = Number(trimmed.replace(',', '.'));

  return Number.isFinite(parsed) ? parsed : undefined;
};
