// What a plug says about itself beside its relay: how well it hears the Wi-Fi,
// why it cut the power by itself, and what it does after a power cut. Only
// Tasmota and Shelly plugs say any of it.
import type { Translate } from '@prusa/connect-ui/i18n';

import type { PlugTelemetry } from './api';
import type { PlugStateName } from './plug-state';

export type SignalName = 'good' | 'fair' | 'weak';

/** A signal in dBm as a word: good from -60, fair from -75. */
export const signalOf = (dbm: number): SignalName => {
  if (dbm >= -60) return 'good';

  return dbm >= -75 ? 'fair' : 'weak';
};

/** The signal as a person reads it: the word, and the number behind it. */
export const signalText = (t: Translate, dbm: number): string =>
  t('power.signal.value', {
    word: t(`power.signal.${signalOf(dbm)}`),
    dbm: String(Math.round(dbm)),
  });

/**
 * The signal of a plug that answers, or undefined. A plug that is offline
 * said its signal before it went, which is not what it has now.
 */
export const signalOfPlug = (
  t: Translate,
  plug: PlugTelemetry | undefined,
  state: PlugStateName,
): string | undefined =>
  plug?.wifi_dbm === undefined || state === 'offline' || state === 'none'
    ? undefined
    : signalText(t, plug.wifi_dbm);

const AFTER_CUT: Readonly<Record<string, string>> = {
  off: 'power.afterCut.off',
  on: 'power.afterCut.on',
  last: 'power.afterCut.last',
  toggle: 'power.afterCut.toggle',
  always_on: 'power.afterCut.alwaysOn',
  delayed_on: 'power.afterCut.delayedOn',
  switch: 'power.afterCut.switch',
};

/**
 * What a plug that answers does after a power cut, in words, or undefined
 * where the plug did not say or said a mode with no words.
 */
export const afterCutOfPlug = (
  t: Translate,
  plug: PlugTelemetry | undefined,
  state: PlugStateName,
): string | undefined => {
  const key = AFTER_CUT[plug?.after_power_cut ?? ''];

  return key === undefined || state === 'offline' || state === 'none'
    ? undefined
    : t(key);
};

/** Whether the plug is set to stay on and refuse switching. */
export const alwaysOn = (
  plug: PlugTelemetry | undefined,
  state: PlugStateName,
): boolean =>
  plug?.after_power_cut === 'always_on' &&
  state !== 'offline' &&
  state !== 'none';

const REASONS: Readonly<Record<string, string>> = {
  overpower: 'power.protection.overpower',
  overtemp: 'power.protection.overtemp',
  overvoltage: 'power.protection.overvoltage',
  undervoltage: 'power.protection.undervoltage',
};

/** Whether the plug's own protection cut the power. */
export const tripped = (plug: PlugTelemetry | undefined): boolean =>
  plug?.protection !== undefined && plug.protection !== '';

/** Why the protection tripped, or undefined for a reason with no words. */
export const protectionText = (
  t: Translate,
  plug: PlugTelemetry | undefined,
): string | undefined => {
  const key = REASONS[plug?.protection ?? ''];

  return key === undefined ? undefined : t(key);
};
