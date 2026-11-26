// Readings as a person reads them, in the reader's own number format.
import {
  formatDuration,
  formatNumber,
  formatUnit,
} from '@prusa/connect-ui/i18n';

export const formatWatts = (watts: number): string =>
  formatUnit(Math.round(watts), 'W', { maximumFractionDigits: 0 });

export const formatVolts = (volts: number): string =>
  formatUnit(Math.round(volts), 'V', { maximumFractionDigits: 0 });

export const formatAmps = (amps: number): string =>
  formatUnit(amps, 'A', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });

/** Watt-hours below one kilowatt-hour, kilowatt-hours from there. */
export const formatEnergy = (wattHours: number): string =>
  wattHours >= 1000
    ? formatUnit(wattHours / 1000, 'kWh', {
        minimumFractionDigits: 2,
        maximumFractionDigits: 2,
      })
    : formatUnit(wattHours, 'Wh', {
        minimumFractionDigits: 1,
        maximumFractionDigits: 1,
      });

/** A span of seconds. A span of nothing is zero seconds, never a blank. */
export const formatSpan = (seconds: number): string =>
  formatDuration(seconds) ||
  formatNumber(0, { style: 'unit', unit: 'second', unitDisplay: 'short' });

/**
 * A wait that counts down, read every few seconds. From two minutes up it
 * is said in whole minutes, so the text does not change with every read.
 */
export const formatCountdown = (seconds: number): string =>
  formatSpan(seconds >= 120 ? Math.ceil(seconds / 60) * 60 : seconds);

const CENTS = { minimumFractionDigits: 2, maximumFractionDigits: 2 } as const;

const labelled = (figure: string, currency: string): string =>
  currency === '' ? figure : `${figure} ${currency}`;

/**
 * An amount of money with its label. The label is free text a person set,
 * so the figure is a plain number and never a currency format. An amount
 * too small for two decimals reads "< 0.01": it is not nothing.
 */
export const formatCost = (value: number, currency: string): string =>
  labelled(
    value > 0 && value < 0.005
      ? `< ${formatNumber(0.01, CENTS)}`
      : formatNumber(value, CENTS),
    currency,
  );

/** An amount with up to four decimals, for the scale of the cost chart. */
export const formatCostFine = (value: number, currency: string): string =>
  labelled(
    formatNumber(value, { minimumFractionDigits: 2, maximumFractionDigits: 4 }),
    currency,
  );

/** A span of seconds in hours, for sums that run to days. */
export const formatHours = (seconds: number): string =>
  formatUnit(seconds / 3600, 'h', {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  });

/** A part of a whole, 0 to 1, as a whole percentage. */
export const formatShare = (part: number): string =>
  formatNumber(part, { style: 'percent', maximumFractionDigits: 0 });
