// A saved password or token is kept only for the address it was saved for.
// The process refuses to send one anywhere else, so the forms ask for it
// again as soon as the address changes. These answer, as the process does,
// whether an address is still the saved one.
import type { PlugSettings } from './api';

type Broker = Pick<
  PlugSettings,
  'mqtt_builtin' | 'mqtt_host' | 'mqtt_port' | 'mqtt_tls'
>;

const same = (a: string | undefined, b: string | undefined): boolean =>
  (a ?? '').trim().toLowerCase() === (b ?? '').trim().toLowerCase();

const MQTT_PORT = 1883;
const MQTT_TLS_PORT = 8883;
const MAX_PORT = 65535;

/**
 * The broker a form names, read as the process reads it: a scheme says only
 * whether TLS is on, a port in the host counts when no port is set, and an
 * unset port is the default one.
 */
const brokerOf = (b: Partial<Broker>): string => {
  let host = (b.mqtt_host ?? '').trim();
  let tls = b.mqtt_tls === true;
  let port = b.mqtt_port ?? 0;
  if (host.includes('://') || host.includes(':')) {
    let rest = host;
    const cut = rest.indexOf('://');
    if (cut >= 0) {
      const scheme = rest.slice(0, cut).toLowerCase();
      if (scheme === 'ssl' || scheme === 'tls' || scheme === 'mqtts')
        tls = true;
      rest = rest.slice(cut + 3);
    }
    rest = rest.split('/')[0] ?? '';
    rest = rest.slice(rest.lastIndexOf('@') + 1);
    const withPort = /^(\[[^\]]*\]|[^:]*):(\d+)$/.exec(rest);
    let name = rest.replace(/^\[|\]$/g, '');
    if (withPort !== null) {
      name = (withPort[1] ?? '').replace(/^\[|\]$/g, '');
      const n = Number(withPort[2]);
      if (n > 0 && n <= MAX_PORT && !(port > 0)) port = n;
    }
    if (name !== '') host = name;
  }
  if (!(port > 0 && port <= MAX_PORT)) port = tls ? MQTT_TLS_PORT : MQTT_PORT;

  return `${tls ? 'ssl' : 'tcp'}://${host.toLowerCase()}:${String(port)}`;
};

/**
 * Whether `next` uses the MQTT password where `saved` uses it. A switch
 * between the built-in broker and an external one counts as a change either
 * way: the password of one is never given to the other.
 */
export const sameBroker = (
  saved: Partial<Broker> | undefined,
  next: Partial<Broker>,
): boolean => {
  if (saved === undefined) return false;
  if (saved.mqtt_builtin === true || next.mqtt_builtin === true)
    return saved.mqtt_builtin === true && next.mqtt_builtin === true;

  return brokerOf(saved) === brokerOf(next);
};

/** The scheme, host and port of a Home Assistant URL, or nothing. */
const originOf = (raw: string | undefined): string => {
  const text = (raw ?? '').trim();
  if (text === '') return '';
  try {
    const url = new URL(text.includes('://') ? text : `http://${text}`);

    return `${url.protocol}//${url.host}`.toLowerCase();
  } catch {
    return '';
  }
};

export const sameHaServer = (
  saved: string | undefined,
  next: string | undefined,
): boolean => originOf(saved) === originOf(next);

/** A plug address as the process stores it: no `http://`, no trailing `/`. */
const plugHostOf = (raw: string | undefined): string =>
  (raw ?? '')
    .trim()
    .replace(/^http:\/\//i, '')
    .replace(/\/+$/, '');

export const samePlugHost = (
  a: string | undefined,
  b: string | undefined,
): boolean => same(plugHostOf(a), plugHostOf(b));
