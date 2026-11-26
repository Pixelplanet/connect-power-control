import type { Operation } from '@prusa/connect';
import type { ModuleHost } from '@prusa/connect-ui/slots';
import { OWN_MODULE } from './own-module';

const BASE = `/api/v1/modules/${OWN_MODULE || 'com.github.aux.pixelplanet.connect-power-control'}`;

export type PlugDriver =
  | 'tasmota'
  | 'tasmota_http'
  | 'shelly_gen1'
  | 'shelly_gen2'
  | 'shelly_http'
  | 'zigbee2mqtt'
  | 'esphome'
  | 'openbeken'
  | 'generic_mqtt'
  | 'homeassistant';

export interface HAEntity {
  entity_id: string;
  friendly_name?: string | undefined;
  state?: string | undefined;
  domain?: string | undefined;
  device_class?: string | undefined;
  unit?: string | undefined;
}

export interface HATestResult {
  ok: boolean;
  url?: string | undefined;
  // The process answers null, not an empty list, when it found none.
  entities?: HAEntity[] | null | undefined;
  error?: string | undefined;
}

export interface PlugSettings {
  mqtt_builtin: boolean;
  mqtt_host: string;
  mqtt_port: number;
  mqtt_tls: boolean;
  mqtt_username: string;
  mqtt_password?: string | undefined;
  has_password: boolean;
  keep_password?: boolean | undefined;
  mqtt_client_id: string;
  ha_url?: string | undefined;
  ha_token?: string | undefined;
  has_ha_token?: boolean | undefined;
  keep_ha_token?: boolean | undefined;
  auto_off_cooldown_sec: number;
  confirm_turn_off: boolean;
  /** Minutes a printer may be on and idle before it is turned off. 0 = off. */
  idle_off_min: number;
}

export interface SettingsResponse {
  settings: PlugSettings;
  mqtt_configured: boolean;
  mqtt_connected: boolean;
  mqtt_broker?: string | undefined;
  mqtt_error?: string | undefined;
  mqtt_warning?: string | undefined;
  mqtt_builtin_running: boolean;
  mqtt_builtin_port?: number | undefined;
  mqtt_builtin_clients: number;
  /** The operator approved the built-in broker's port. */
  mqtt_builtin_granted?: boolean | undefined;
  ha_configured: boolean;
  /** Written in the host's own settings for this extension, so read-only here. */
  mqtt_host_pinned?: boolean | undefined;
  ha_url_pinned?: boolean | undefined;
  /** The address offered when a plug is set up by IP address. */
  suggested_http_host?: string | undefined;
}

export interface BrokerTestResult {
  ok: boolean;
  broker?: string | undefined;
  error?: string | undefined;
  warning?: string | undefined;
}

export interface DeviceConfig {
  driver?: PlugDriver | '' | undefined;
  mode?: 'mqtt' | 'http' | 'ha' | '' | undefined;
  tasmota_topic: string;
  http_host?: string | undefined;
  http_user?: string | undefined;
  http_password?: string | undefined;
  has_http_password?: boolean | undefined;
  keep_http_password?: boolean | undefined;
  mqtt_command_topic?: string | undefined;
  mqtt_state_topic?: string | undefined;
  mqtt_availability_topic?: string | undefined;
  mqtt_payload_on?: string | undefined;
  mqtt_payload_off?: string | undefined;
  mqtt_state_on?: string | undefined;
  mqtt_state_off?: string | undefined;
  mqtt_state_json_key?: string | undefined;
  mqtt_power_topic?: string | undefined;
  mqtt_power_json_key?: string | undefined;
  ha_entity_id?: string | undefined;
  ha_power_entity_id?: string | undefined;
  relay_index?: number | undefined;
  /** The plug turns off after the print that runs now, or the next one. */
  auto_off_after_print: boolean;
  /** The plug turns off after every print. */
  auto_off_default?: boolean | undefined;
  cooldown_sec?: number | undefined;
  /** Left out: the global idle time counts. 0: never. Else own minutes. */
  idle_off_min?: number | undefined;
}

export function effectiveDriver(
  cfg: DeviceConfig | undefined,
): PlugDriver | '' {
  if (!cfg) return '';
  if (cfg.driver) return cfg.driver;
  if (cfg.mode === 'http') return 'tasmota_http';
  if (cfg.ha_entity_id) return 'homeassistant';
  if (cfg.mqtt_command_topic) return 'generic_mqtt';
  if (cfg.http_host && !cfg.tasmota_topic) return 'tasmota_http';
  if (cfg.tasmota_topic) return 'tasmota';
  return '';
}

export function isDeviceConfigured(cfg: DeviceConfig | undefined): boolean {
  if (!cfg) return false;
  const d = effectiveDriver(cfg);
  if (!d) return false;
  switch (d) {
    case 'tasmota_http':
    case 'shelly_http':
      return Boolean(cfg.http_host?.trim());
    case 'homeassistant':
      return Boolean(cfg.ha_entity_id?.trim());
    case 'generic_mqtt':
      return Boolean(cfg.mqtt_command_topic?.trim());
    default:
      return Boolean(cfg.tasmota_topic.trim());
  }
}

/** The address a person knows the plug by: its topic, its host or its entity. */
export function driverDisplayTarget(cfg: DeviceConfig | undefined): string {
  if (!cfg) return '';
  const d = effectiveDriver(cfg);
  switch (d) {
    case 'tasmota_http':
    case 'shelly_http':
      return cfg.http_host ?? '';
    case 'homeassistant':
      return cfg.ha_entity_id ?? '';
    case 'generic_mqtt':
      return cfg.mqtt_command_topic ?? '';
    default:
      return cfg.tasmota_topic;
  }
}

/** The relay a config names. An absent relay, or a zero, is the first. */
export const relayIndexOf = (cfg: DeviceConfig | undefined): number => {
  const relay = cfg?.relay_index;

  return relay === undefined || relay < 1 ? 1 : relay;
};

/** A text that says something, or `fallback` for an absent or empty one. */
export const textOr = (value: string | undefined, fallback: string): string =>
  value === undefined || value === '' ? fallback : value;

export type PowerState = 'ON' | 'OFF' | 'UNKNOWN';

export interface PlugTelemetry {
  topic?: string;
  driver?: string;
  friendly_name?: string;
  model?: string;
  relay_count?: number;
  relay_names?: string[];
  relays?: Record<string, PowerState>;
  power: PowerState;
  // The process answers null, not an absent key, for what a plug never said.
  online?: boolean | null;
  power_watts?: number | null;
  voltage?: number | null;
  current_amps?: number | null;
  last_error?: string;
  updated_at?: string;
  /** The plug's Wi-Fi signal, when it said one. */
  wifi_dbm?: number;
  /** Why the plug turned itself off: overpower, overtemp, a voltage. */
  protection?: string;
  /**
   * What the relay does when the power comes back after a cut: off, on,
   * last, toggle, always_on, delayed_on or switch. The plug's own setting.
   */
  after_power_cut?: string;
}

/** A reading a plug gave, or undefined for one it never gave. Never a zero. */
export const readingOf = (
  value: number | null | undefined,
): number | undefined => value ?? undefined;

export interface PowerSample {
  ts: number;
  watts: number;
}

export interface PowerSession {
  active: boolean;
  started_at: string;
  ended_at?: string;
  duration_sec: number;
  energy_wh: number;
  avg_watts: number;
  peak_watts: number;
  /** Priced by the rule of the Energy tab. Null with no price. */
  cost?: number | null;
}

export interface PrintPowerRecord {
  job_id?: string;
  file_name: string;
  active: boolean;
  outcome?: string;
  started_at: string;
  ended_at?: string;
  duration_sec: number;
  energy_wh: number;
  avg_watts: number;
  peak_watts: number;
  /** The seconds of the print the plug was on with a reading. */
  metered_sec?: number;
  /** The print ended while nothing was sampled, so its end is the last sample. */
  end_estimated?: boolean;
  /** Set on a running print whose printer went away: when it did. */
  away_since?: string;
  /** Null with no price, and for a print with no reading. */
  cost?: number | null;
}

export interface PowerStatus {
  device_id: string;
  mqtt_configured: boolean;
  mqtt_connected: boolean;
  mqtt_broker?: string;
  mqtt_error?: string;
  ha_configured?: boolean;
  config: DeviceConfig;
  plug?: PlugTelemetry;
  discovered_plugs?: PlugTelemetry[] | null;
  can_turn_off: boolean;
  lock_reason?: string;
  printer_connected: boolean;
  printer_state?: string;
  printer_moving?: boolean;
  auto_off_pending: boolean;
  auto_off_remaining_sec: number;
  /** Why the plug could not be turned off after a print. It is tried again. */
  auto_off_error?: string;
  /** Why the printer stayed on after its last print: cancelled or failed. */
  auto_off_kept?: string;
  /** The idle time that counts for this printer in minutes, 0 for none. */
  idle_off_min?: number;
  /** The idle clock runs: the printer is on, reachable and not busy. */
  idle_off_pending?: boolean;
  idle_off_remaining_sec?: number;
  /** Why the plug could not be turned off after the idle time. */
  idle_off_error?: string;
  default_cooldown_sec: number;
  confirm_turn_off?: boolean;
  poll_interval_sec?: number;
  power_history?: PowerSample[];
  session_usage?: PowerSession;
  current_print_usage?: PrintPowerRecord;
  print_history?: PrintPowerRecord[];
  /** The label of the costs above, '' when none is set. */
  currency?: string;
  price_set?: boolean;
}

export interface AssignmentsResponse {
  assignments: Record<string, DeviceConfig>;
  // The process answers null, not an empty list, for a list with nothing in it.
  discovered_plugs: PlugTelemetry[] | null;
  mqtt_configured: boolean;
  mqtt_connected: boolean;
  mqtt_broker?: string;
  mqtt_error?: string;
  ha_configured?: boolean;
  ha_entities?: HAEntity[] | null;
  default_cooldown_sec: number;
}

/** One printer's plug in the fleet overview. */
export interface OverviewEntry {
  device_id: string;
  config: DeviceConfig;
  plug?: PlugTelemetry;
  /** The samples of the minutes asked for, only when they were asked for. */
  power_history?: PowerSample[];
  /** Whether the person may switch this plug. The process asks again. */
  can_operate?: boolean;
  session_usage?: PowerSession;
  current_print_usage?: PrintPowerRecord;
  /** Seconds until the idle turn-off, only while its clock runs. */
  idle_off_remaining_sec?: number;
}

/** Every printer with a plug the person may see, in one answer. */
export interface OverviewResponse {
  devices: OverviewEntry[];
  mqtt_configured: boolean;
  mqtt_connected: boolean;
  mqtt_error?: string;
  ha_configured: boolean;
  default_cooldown_sec: number;
  confirm_turn_off: boolean;
  poll_interval_sec: number;
  /** Whether the person may change plugs and connections. */
  can_configure?: boolean;
}

/** The length of the period the Energy view shows. */
export type EnergySpan = 'day' | 'week' | 'month' | 'year';

/**
 * The table a CSV file holds: one row per period and printer, per print, or
 * per file name.
 */
export type EnergyTable = 'periods' | 'prints' | 'files';

/** A price per kWh, with an optional second price for the night hours. */
export interface PriceChange {
  price: number;
  night: boolean;
  night_price: number;
  /** The site hour, 0 to 23, from which the night price counts. */
  night_start: number;
  /** The site hour, 0 to 23, from which the day price counts again. */
  night_end: number;
}

/** A price and the moment it counts from. */
export interface Tariff {
  /** Unix seconds, 0 for "from the first record". */
  from: number;
  price: number;
  night?: boolean;
  night_price?: number;
  night_start?: number;
  night_end?: number;
}

export interface EnergySettingsResponse {
  keep_days: number;
  /** An IANA name, or '' while days follow the server clock. */
  time_zone: string;
  zone_set: boolean;
  utc_offset_sec: number;
  currency: string;
  /** The price that counts now, null with no price. */
  price: PriceChange | null;
  price_since: string | null;
  tariffs: Tariff[];
  /** False when the extension was given no data directory. */
  recording: boolean;
  recording_since: string;
  /**
   * Nothing is deleted for its age, because the saved settings could not be
   * read. A change that carries `keep_days` ends it.
   */
  prune_paused?: boolean;
}

/** What a save changes. A field left out keeps its saved value. */
export interface EnergySettingsChange {
  keep_days?: number;
  /** Said when the new keep time deletes records. */
  shorten?: boolean;
  time_zone?: string;
  currency?: string;
  price?: PriceChange;
  /** 'start' replaces every earlier price. */
  price_from?: 'now' | 'start';
  clear_price?: boolean;
}

/**
 * Energy and the three times that say what it means: seen at all, plug on,
 * and on with a reading. `factOf` in `energy.ts` reads them.
 */
export interface EnergyFigures {
  energy_wh: number;
  night_wh: number;
  /** Null with no price, never a zero. */
  cost: number | null;
  observed_sec: number;
  on_sec: number;
  metered_sec: number;
  /** The energy that went into prints. The three parts add up to energy_wh. */
  print_wh?: number;
  idle_wh?: number;
  /** Null with no price. */
  idle_cost?: number | null;
  /** Energy of records that do not say whether a print ran. */
  split_unknown_wh?: number;
  /** Part of the split is worked out from the print records. */
  split_estimated?: boolean;
}

/** One period of the span: an hour, a day or a month. */
export interface EnergyPoint extends EnergyFigures {
  start: string;
  /** The site date the period starts on, as YYYY-MM-DD. */
  day: string;
  /** The site hour the period starts at, 0 unless the span is a day. */
  hour: number;
}

export interface EnergyPrinter extends EnergyFigures {
  device_id: string;
  name: string;
  prints: number;
}

export interface EnergyPrint {
  device_id: string;
  name: string;
  job_id?: string;
  file_name: string;
  outcome?: string;
  started_at: string;
  ended_at: string;
  /** The site wall clock of the end, with no offset. */
  ended_local: string;
  duration_sec: number;
  energy_wh: number;
  night_wh: number;
  cost: number | null;
  metered_sec: number;
  peak_watts: number;
  end_estimated?: boolean;
}

/** Every print of one file name in the span. */
export interface EnergyFile {
  /** '' for prints with no known name. */
  file_name: string;
  prints: number;
  /** Failed, cancelled or interrupted. */
  not_finished: number;
  /** Prints with a reading. Energy and cost are theirs. */
  measured: number;
  energy_wh: number;
  cost: number | null;
  /** Over the prints that finished with a reading, null with none. */
  avg_wh: number | null;
  avg_cost: number | null;
}

export interface EnergyReport {
  span: EnergySpan;
  /** The first site day of the span. */
  at: string;
  /** The first day of the span before and after, '' when there is none. */
  prev_at: string;
  next_at: string;
  from: string;
  to: string;
  /** The oldest site day still kept, '' with no record. */
  earliest: string;
  time_zone: string;
  zone_set: boolean;
  utc_offset_sec: number;
  keep_days: number;
  currency: string;
  price_set: boolean;
  /** A night price counts somewhere in the span. */
  night: boolean;
  price_changed: boolean;
  /** The printer the figures are narrowed to, '' for all. */
  device: string;
  recording: boolean;
  can_configure: boolean;
  /** Every period of the span, also the empty ones. */
  points: EnergyPoint[];
  total: EnergyFigures;
  /** The span before, null when nothing was seen in it. */
  previous: EnergyFigures | null;
  /** Never narrowed to one printer. Most energy first. */
  printers: EnergyPrinter[];
  printers_total: number;
  /** Printers with a record that Connect no longer lists. */
  removed: number;
  /** Newest first, at most 200. `prints_total` counts all. */
  prints: EnergyPrint[];
  prints_total: number;
  prints_wh: number;
  prints_cost: number | null;
  prints_dropped: number;
  /** Most energy first, at most 200. `files_total` counts all. */
  files?: EnergyFile[];
  files_total?: number;
}

/** One printer's part of the summary. */
export interface EnergySummaryPrinter {
  device_id: string;
  today: EnergyFigures;
  this_month: EnergyFigures;
}

/** Today and this month for every printer the person may see. */
export interface EnergySummary {
  /** The site day and month the figures are for. */
  day: string;
  month: string;
  currency: string;
  price_set: boolean;
  /** The price per kWh that counts now, null with no price. */
  price_now: number | null;
  night_now: boolean;
  /** More than one price counts in this month. */
  price_changed: boolean;
  recording: boolean;
  today: EnergyFigures;
  this_month: EnergyFigures;
  /** Every printer with a record this month. */
  printers: EnergySummaryPrinter[];
}

/** One part of a CSV file. The parts join to the whole file. */
export interface EnergyExport {
  file_name: string;
  csv: string;
  /** The row the next part starts at, 0 after the last part. */
  next_offset: number;
  /** The moment the rows are read for. Every later part sends it back. */
  as_of: number;
}

export interface EnergyQuery {
  span: EnergySpan;
  /** A site day inside the span, as YYYY-MM-DD. Left out for now. */
  at?: string | undefined;
  /** A printer, left out or '' for all. */
  device?: string | undefined;
}

const opGetSettings: Operation = {
  id: 'get-tasmota-settings',
  method: 'GET',
  path: `${BASE}/settings`,
  response: 'json',
  anonymous: false,
};

const opPutSettings: Operation = {
  id: 'put-tasmota-settings',
  method: 'PUT',
  path: `${BASE}/settings`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opTestBroker: Operation = {
  id: 'test-broker-connection',
  method: 'POST',
  path: `${BASE}/broker/test`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opTestHomeAssistant: Operation = {
  id: 'test-ha-connection',
  method: 'POST',
  path: `${BASE}/ha/test`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opDiscover: Operation = {
  id: 'discover-tasmota-plugs',
  method: 'POST',
  path: `${BASE}/discover`,
  response: 'json',
  anonymous: false,
};

const opGetAssignments: Operation = {
  id: 'get-plug-assignments',
  method: 'GET',
  path: `${BASE}/assignments`,
  response: 'json',
  anonymous: false,
};

const opGetOverview: Operation = {
  id: 'get-power-overview',
  method: 'GET',
  path: `${BASE}/overview`,
  response: 'json',
  anonymous: false,
};

const opGetStatus: Operation = {
  id: 'get-device-power',
  method: 'GET',
  path: `${BASE}/devices/{device_id}`,
  response: 'json',
  anonymous: false,
};

const opPutConfig: Operation = {
  id: 'put-device-power-config',
  method: 'PUT',
  path: `${BASE}/devices/{device_id}/config`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opPostPower: Operation = {
  id: 'post-device-power',
  method: 'POST',
  path: `${BASE}/devices/{device_id}/power`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opPostAutoOff: Operation = {
  id: 'post-device-auto-off',
  method: 'POST',
  path: `${BASE}/devices/{device_id}/auto-off`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opPostIdleOff: Operation = {
  id: 'post-device-idle-off',
  method: 'POST',
  path: `${BASE}/devices/{device_id}/idle-off`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opGetEnergySettings: Operation = {
  id: 'get-energy-settings',
  method: 'GET',
  path: `${BASE}/energy/settings`,
  response: 'json',
  anonymous: false,
};

const opPutEnergySettings: Operation = {
  id: 'put-energy-settings',
  method: 'PUT',
  path: `${BASE}/energy/settings`,
  body: 'application/json',
  response: 'json',
  anonymous: false,
};

const opGetEnergy: Operation = {
  id: 'get-energy-report',
  method: 'GET',
  path: `${BASE}/energy`,
  response: 'json',
  anonymous: false,
};

const opGetEnergySummary: Operation = {
  id: 'get-energy-summary',
  method: 'GET',
  path: `${BASE}/energy/summary`,
  response: 'json',
  anonymous: false,
};

const opGetEnergyExport: Operation = {
  id: 'get-energy-export',
  method: 'GET',
  path: `${BASE}/energy/export`,
  response: 'json',
  anonymous: false,
};

/** The query of both energy reads. An empty value is left out. */
const energyQueryOf = (query: EnergyQuery): Record<string, string> => ({
  span: query.span,
  ...(query.at ? { at: query.at } : {}),
  ...(query.device ? { device: query.device } : {}),
});

export function createApi(host: Pick<ModuleHost, 'client'>) {
  const t = host.client.transport;
  return {
    getSettings: (signal?: AbortSignal) =>
      t.call<SettingsResponse>(opGetSettings, {}, signal),

    saveSettings: (settings: Partial<PlugSettings>) =>
      t.call<SettingsResponse>(opPutSettings, { body: settings }),

    testBroker: (settings?: Partial<PlugSettings>) =>
      t.call<BrokerTestResult>(opTestBroker, { body: settings ?? {} }),

    testHomeAssistant: (settings?: Partial<PlugSettings>) =>
      t.call<HATestResult>(opTestHomeAssistant, { body: settings ?? {} }),

    discoverPlugs: () => t.call<{ plugs: PlugTelemetry[] }>(opDiscover, {}),

    getAssignments: (signal?: AbortSignal) =>
      t.call<AssignmentsResponse>(opGetAssignments, {}, signal),

    /** history is the minutes of samples to carry, 0 for none. */
    getOverview: (history: number, signal?: AbortSignal) =>
      t.call<OverviewResponse>(
        opGetOverview,
        history > 0 ? { query: { history } } : {},
        signal,
      ),

    getStatus: (deviceId: string, signal?: AbortSignal) =>
      t.call<PowerStatus>(
        opGetStatus,
        { path: { device_id: deviceId } },
        signal,
      ),

    saveConfig: (deviceId: string, config: DeviceConfig) =>
      t.call<PowerStatus>(opPutConfig, {
        path: { device_id: deviceId },
        body: config,
      }),

    /** `idleOnly` leaves a printer on that Connect cannot reach. */
    sendPower: (
      deviceId: string,
      action: 'on' | 'off' | 'toggle',
      idleOnly = false,
    ) =>
      t.call<PowerStatus>(opPostPower, {
        path: { device_id: deviceId },
        body: idleOnly ? { action, idle_only: true } : { action },
      }),

    /** `asDefault` sets what the printer does after every print. */
    setAutoOff: (deviceId: string, enabled: boolean, asDefault = false) =>
      t.call<PowerStatus>(opPostAutoOff, {
        path: { device_id: deviceId },
        body: asDefault ? { enabled, default: enabled } : { enabled },
      }),

    /** Starts the idle wait of a printer again. */
    keepOn: (deviceId: string) =>
      t.call<PowerStatus>(opPostIdleOff, {
        path: { device_id: deviceId },
        body: { action: 'keep' },
      }),

    getEnergySettings: (signal?: AbortSignal) =>
      t.call<EnergySettingsResponse>(opGetEnergySettings, {}, signal),

    saveEnergySettings: (change: EnergySettingsChange) =>
      t.call<EnergySettingsResponse>(opPutEnergySettings, { body: change }),

    getEnergy: (query: EnergyQuery, signal?: AbortSignal) =>
      t.call<EnergyReport>(
        opGetEnergy,
        { query: energyQueryOf(query) },
        signal,
      ),

    getEnergySummary: (signal?: AbortSignal) =>
      t.call<EnergySummary>(opGetEnergySummary, {}, signal),

    /**
     * One part of a CSV file. `asOf` is the `as_of` the first part answered,
     * so that a record made between two parts does not move the rows.
     */
    getEnergyExport: (
      query: EnergyQuery,
      table: EnergyTable,
      offset: number,
      asOf?: number,
      signal?: AbortSignal,
    ) =>
      t.call<EnergyExport>(
        opGetEnergyExport,
        {
          query: {
            ...energyQueryOf(query),
            table,
            offset,
            ...(asOf === undefined ? {} : { as_of: asOf }),
          },
        },
        signal,
      ),
  };
}

export type PowerApi = ReturnType<typeof createApi>;
