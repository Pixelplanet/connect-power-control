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

export const PLUG_DRIVER_OPTIONS: Array<{ value: PlugDriver; label: string }> = [
  { value: 'tasmota', label: 'Tasmota (MQTT)' },
  { value: 'tasmota_http', label: 'Tasmota (Direct HTTP)' },
  { value: 'shelly_gen2', label: 'Shelly Gen2 / Plus / Pro (MQTT)' },
  { value: 'shelly_gen1', label: 'Shelly Gen1 (MQTT)' },
  { value: 'shelly_http', label: 'Shelly Gen1 / Gen2+ (Direct HTTP)' },
  { value: 'zigbee2mqtt', label: 'Zigbee2MQTT' },
  { value: 'esphome', label: 'ESPHome (MQTT)' },
  { value: 'openbeken', label: 'OpenBeken (MQTT)' },
  { value: 'generic_mqtt', label: 'Generic MQTT (Custom Topics)' },
  { value: 'homeassistant', label: 'Home Assistant (REST)' },
];

export const DRIVER_BADGE_LABELS: Record<PlugDriver, string> = {
  tasmota: 'Tasmota',
  tasmota_http: 'Tasmota HTTP',
  shelly_gen1: 'Shelly Gen1',
  shelly_gen2: 'Shelly Gen2+',
  shelly_http: 'Shelly HTTP',
  zigbee2mqtt: 'Zigbee2MQTT',
  esphome: 'ESPHome',
  openbeken: 'OpenBeken',
  generic_mqtt: 'Generic MQTT',
  homeassistant: 'Home Assistant',
};

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
  entities?: HAEntity[] | undefined;
  error?: string | undefined;
}

export interface TasmotaSettings {
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
}

export interface SettingsResponse {
  settings: TasmotaSettings;
  mqtt_configured: boolean;
  mqtt_connected: boolean;
  mqtt_broker?: string | undefined;
  mqtt_error?: string | undefined;
  mqtt_warning?: string | undefined;
  mqtt_builtin_running: boolean;
  mqtt_builtin_port?: number | undefined;
  mqtt_builtin_clients: number;
  ha_configured: boolean;
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
  auto_off_after_print: boolean;
  cooldown_sec?: number | undefined;
}

export function effectiveDriver(cfg: DeviceConfig | undefined): PlugDriver | '' {
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
      return Boolean(cfg.tasmota_topic?.trim());
  }
}

export function driverUsesMQTT(driver: PlugDriver | ''): boolean {
  return (
    driver === 'tasmota' ||
    driver === 'shelly_gen1' ||
    driver === 'shelly_gen2' ||
    driver === 'zigbee2mqtt' ||
    driver === 'esphome' ||
    driver === 'openbeken' ||
    driver === 'generic_mqtt'
  );
}

export function driverUsesHTTP(driver: PlugDriver | ''): boolean {
  return driver === 'tasmota_http' || driver === 'shelly_http';
}

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
      return cfg.tasmota_topic ?? '';
  }
}

export interface PlugTelemetry {
  topic?: string;
  driver?: string;
  friendly_name?: string;
  model?: string;
  relay_count?: number;
  relay_names?: string[];
  relays?: Record<string, 'ON' | 'OFF' | 'UNKNOWN'>;
  power: 'ON' | 'OFF' | 'UNKNOWN';
  online?: boolean;
  power_watts?: number;
  voltage?: number;
  current_amps?: number;
  last_error?: string;
  updated_at?: string;
}

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
  discovered_plugs?: PlugTelemetry[];
  can_turn_off: boolean;
  lock_reason?: string;
  printer_connected: boolean;
  printer_state?: string;
  printer_moving?: boolean;
  auto_off_pending: boolean;
  auto_off_remaining_sec: number;
  default_cooldown_sec: number;
  confirm_turn_off?: boolean;
  poll_interval_sec?: number;
  power_history?: PowerSample[];
  session_usage?: PowerSession;
  current_print_usage?: PrintPowerRecord;
  print_history?: PrintPowerRecord[];
}

export interface AssignmentsResponse {
  assignments: Record<string, DeviceConfig>;
  discovered_plugs: PlugTelemetry[];
  mqtt_configured: boolean;
  mqtt_connected: boolean;
  mqtt_broker?: string;
  mqtt_error?: string;
  ha_configured?: boolean;
  ha_entities?: HAEntity[];
  default_cooldown_sec: number;
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

export function createApi(host: Pick<ModuleHost, 'client'>) {
  const t = host.client.transport;
  return {
    getSettings: (signal?: AbortSignal) =>
      t.call<SettingsResponse>(opGetSettings, {}, signal),

    saveSettings: (settings: Partial<TasmotaSettings>) =>
      t.call<SettingsResponse>(opPutSettings, { body: settings }),

    testBroker: (settings?: Partial<TasmotaSettings>) =>
      t.call<BrokerTestResult>(opTestBroker, { body: settings ?? {} }),

    testHomeAssistant: (settings?: Partial<TasmotaSettings>) =>
      t.call<HATestResult>(opTestHomeAssistant, { body: settings ?? {} }),

    discoverPlugs: () =>
      t.call<{ plugs: PlugTelemetry[] }>(opDiscover, {}),

    getAssignments: (signal?: AbortSignal) =>
      t.call<AssignmentsResponse>(opGetAssignments, {}, signal),

    getStatus: (deviceId: string, signal?: AbortSignal) =>
      t.call<PowerStatus>(opGetStatus, { path: { device_id: deviceId } }, signal),

    saveConfig: (deviceId: string, config: DeviceConfig) =>
      t.call<PowerStatus>(opPutConfig, {
        path: { device_id: deviceId },
        body: config,
      }),

    sendPower: (deviceId: string, action: 'on' | 'off' | 'toggle') =>
      t.call<PowerStatus>(opPostPower, {
        path: { device_id: deviceId },
        body: { action },
      }),

    setAutoOff: (deviceId: string, enabled: boolean) =>
      t.call<PowerStatus>(opPostAutoOff, {
        path: { device_id: deviceId },
        body: { enabled },
      }),
  };
}
