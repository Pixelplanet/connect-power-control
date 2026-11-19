<script lang="ts">
  import { onMount } from 'svelte';
  import {
    Badge,
    Button,
    Input,
    Label,
    SettingsPane,
    Switch,
  } from '@prusa/connect-ui/primitives';

  import {
    createApi,
    DRIVER_BADGE_LABELS,
    driverDisplayTarget,
    driverUsesHTTP,
    effectiveDriver,
    isDeviceConfigured,
    PLUG_DRIVER_OPTIONS,
    type AssignmentsResponse,
    type BrokerTestResult,
    type DeviceConfig,
    type HAEntity,
    type PlugDriver,
    type PlugTelemetry,
    type SettingsResponse,
  } from './api';
  import { heldHost } from './host';

  const host = heldHost();
  const api = createApi(host);
  const fleet = host.fleet;
  const t = host.t;

  // Broker & General form state
  let mqttBuiltin = $state(false);
  let mqttHost = $state('');
  let mqttPort = $state('1883');
  let mqttTls = $state(false);
  let mqttUsername = $state('');
  let mqttPassword = $state('');
  let hasSavedPassword = $state(false);
  let mqttClientId = $state('prusa-connect-tasmota');
  let cooldownSec = $state('120');
  let confirmTurnOff = $state(true);

  // Home Assistant form state
  let haUrl = $state('');
  let haToken = $state('');
  let hasSavedHaToken = $state(false);
  let haEntities = $state<HAEntity[]>([]);
  let testingHa = $state(false);
  let haMessage = $state('');
  let haMessageIsError = $state(false);

  let settingsMeta = $state<SettingsResponse | undefined>(undefined);
  let assignmentsData = $state<AssignmentsResponse | undefined>(undefined);

  interface DeviceDraft {
    driver: PlugDriver;
    topic: string;
    manual: boolean;
    relay: number;
    httpHost: string;
    httpPassword: string;
    hasHttpPassword: boolean;
    mqttCmdTopic: string;
    mqttStateTopic: string;
    mqttAvailTopic: string;
    mqttPayloadOn: string;
    mqttPayloadOff: string;
    mqttStateJsonKey: string;
    mqttPowerTopic: string;
    mqttPowerJsonKey: string;
    haEntityId: string;
    haPowerEntityId: string;
  }

  let drafts = $state<Record<string, DeviceDraft>>({});
  let justSaved = $state<Record<string, boolean>>({});

  let savingSettings = $state(false);
  let testing = $state(false);
  let scanning = $state(false);
  let savingDeviceId = $state('');

  let settingsMessage = $state('');
  let settingsMessageIsError = $state(false);
  let testResult = $state<BrokerTestResult | undefined>(undefined);
  let assignmentError = $state('');

  const devices = $derived(fleet.devices);
  const haSwitchEntities = $derived(
    haEntities.filter((e) => e.domain === 'switch' || e.domain === 'input_boolean'),
  );
  const haPowerEntities = $derived(
    haEntities.filter((e) => e.domain === 'sensor'),
  );

  function defaultLanHost(): string {
    if (typeof window !== 'undefined' && window.location.hostname) {
      return window.location.hostname;
    }
    return '127.0.0.1';
  }

  function buildSettingsPayload() {
    const parsedPort = Number.parseInt(mqttPort, 10);
    const parsedCooldown = Number.parseInt(cooldownSec, 10);
    const effectiveTls = mqttBuiltin ? false : mqttTls;
    return {
      mqtt_builtin: mqttBuiltin,
      mqtt_host: mqttBuiltin
        ? mqttHost.trim() || defaultLanHost()
        : mqttHost.trim(),
      mqtt_port: mqttBuiltin
        ? 1883
        : Number.isFinite(parsedPort) && parsedPort > 0
          ? parsedPort
          : effectiveTls
            ? 8883
            : 1883,
      mqtt_tls: effectiveTls,
      mqtt_username: mqttUsername.trim(),
      mqtt_password: mqttPassword,
      keep_password: mqttPassword === '' && hasSavedPassword,
      mqtt_client_id: mqttClientId.trim() || 'prusa-connect-tasmota',
      ha_url: haUrl.trim(),
      ha_token: haToken.trim(),
      keep_ha_token: haToken.trim() === '' && hasSavedHaToken,
      auto_off_cooldown_sec:
        Number.isFinite(parsedCooldown) && parsedCooldown >= 0
          ? parsedCooldown
          : 120,
      confirm_turn_off: confirmTurnOff,
    };
  }

  function applySettingsResponse(res: SettingsResponse) {
    settingsMeta = res;
    mqttBuiltin = Boolean(res.settings.mqtt_builtin);
    mqttHost = res.settings.mqtt_host;
    mqttPort = String(
      res.settings.mqtt_port || (res.settings.mqtt_tls ? 8883 : 1883),
    );
    mqttTls = res.settings.mqtt_tls;
    mqttUsername = res.settings.mqtt_username;
    mqttPassword = '';
    hasSavedPassword = res.settings.has_password;
    mqttClientId = res.settings.mqtt_client_id || 'prusa-connect-tasmota';
    haUrl = res.settings.ha_url ?? '';
    haToken = '';
    hasSavedHaToken = Boolean(res.settings.has_ha_token);
    cooldownSec = String(res.settings.auto_off_cooldown_sec ?? 120);
    confirmTurnOff = res.settings.confirm_turn_off ?? true;
  }

  function makeDraftFromConfig(cfg: DeviceConfig | undefined): DeviceDraft {
    const drv = effectiveDriver(cfg) || 'tasmota';
    return {
      driver: drv,
      topic: cfg?.tasmota_topic ?? '',
      manual: false,
      relay: cfg?.relay_index || 1,
      httpHost: cfg?.http_host ?? '',
      httpPassword: '',
      hasHttpPassword: Boolean(cfg?.has_http_password),
      mqttCmdTopic: cfg?.mqtt_command_topic ?? '',
      mqttStateTopic: cfg?.mqtt_state_topic ?? '',
      mqttAvailTopic: cfg?.mqtt_availability_topic ?? '',
      mqttPayloadOn: cfg?.mqtt_payload_on ?? '',
      mqttPayloadOff: cfg?.mqtt_payload_off ?? '',
      mqttStateJsonKey: cfg?.mqtt_state_json_key ?? '',
      mqttPowerTopic: cfg?.mqtt_power_topic ?? '',
      mqttPowerJsonKey: cfg?.mqtt_power_json_key ?? '',
      haEntityId: cfg?.ha_entity_id ?? '',
      haPowerEntityId: cfg?.ha_power_entity_id ?? '',
    };
  }

  function syncAssignmentDrafts(
    aRes: AssignmentsResponse,
    preserveDrafts = false,
  ) {
    assignmentsData = aRes;
    if (aRes.ha_entities && aRes.ha_entities.length > 0) {
      haEntities = aRes.ha_entities;
    }
    const nextDrafts: Record<string, DeviceDraft> = {};
    for (const dev of devices) {
      const cfg = aRes.assignments[dev.id];
      const existing = drafts[dev.id];
      if (preserveDrafts && existing !== undefined) {
        nextDrafts[dev.id] = {
          ...existing,
          hasHttpPassword: Boolean(cfg?.has_http_password),
        };
      } else {
        nextDrafts[dev.id] = makeDraftFromConfig(cfg);
      }
    }
    drafts = nextDrafts;
  }

  function getDraft(deviceId: string): DeviceDraft {
    const existing = drafts[deviceId];
    if (existing) return existing;
    return makeDraftFromConfig(assignmentsData?.assignments[deviceId]);
  }

  function updateDraft(deviceId: string, patch: Partial<DeviceDraft>) {
    drafts[deviceId] = {
      ...getDraft(deviceId),
      ...patch,
    };
  }

  async function refreshAll() {
    try {
      const [sRes, aRes] = await Promise.all([
        api.getSettings(),
        api.getAssignments(),
      ]);
      applySettingsResponse(sRes);
      syncAssignmentDrafts(aRes);
    } catch (err) {
      settingsMessage = err instanceof Error ? err.message : String(err);
      settingsMessageIsError = true;
    }
  }

  onMount(() => {
    void refreshAll();
  });

  function handleTlsToggle(next: boolean) {
    mqttTls = next;
    if (next && mqttPort === '1883') mqttPort = '8883';
    if (!next && mqttPort === '8883') mqttPort = '1883';
  }

  function handleBuiltinToggle(next: boolean) {
    mqttBuiltin = next;
    testResult = undefined;
    if (next) {
      mqttPort = '1883';
    }
  }

  async function handleTest() {
    testing = true;
    testResult = undefined;
    settingsMessage = '';
    try {
      const res = await api.testBroker(buildSettingsPayload());
      testResult = res;
      if (res.ok) {
        settingsMessage = t('tasmota-power.settings.testOk', {
          broker: res.broker ?? '',
        });
        settingsMessageIsError = false;
      } else {
        settingsMessage = t('tasmota-power.settings.testFail', {
          error: res.error ?? 'unknown error',
        });
        settingsMessageIsError = true;
      }
    } catch (err) {
      settingsMessage = err instanceof Error ? err.message : String(err);
      settingsMessageIsError = true;
    } finally {
      testing = false;
    }
  }

  async function handleTestHa() {
    testingHa = true;
    haMessage = '';
    try {
      const res = await api.testHomeAssistant(buildSettingsPayload());
      if (res.ok) {
        if (res.entities) {
          haEntities = res.entities;
        }
        haMessage = t('tasmota-power.settings.haTestOk', {
          url: res.url ?? haUrl.trim(),
          count: String(res.entities?.length ?? 0),
        });
        haMessageIsError = false;
      } else {
        haMessage = t('tasmota-power.settings.testFail', {
          error: res.error ?? 'unknown error',
        });
        haMessageIsError = true;
      }
    } catch (err) {
      haMessage = err instanceof Error ? err.message : String(err);
      haMessageIsError = true;
    } finally {
      testingHa = false;
    }
  }

  async function handleSaveSettings(event?: SubmitEvent) {
    event?.preventDefault();
    savingSettings = true;
    settingsMessage = '';
    haMessage = '';
    testResult = undefined;
    try {
      const res = await api.saveSettings(buildSettingsPayload());
      applySettingsResponse(res);
      syncAssignmentDrafts(await api.getAssignments(), true);
      if (res.mqtt_error) {
        settingsMessage = res.mqtt_error;
        settingsMessageIsError = true;
      } else {
        settingsMessage = t('tasmota-power.settings.savedMessage');
        settingsMessageIsError = false;
      }
    } catch (err) {
      settingsMessage = err instanceof Error ? err.message : String(err);
      settingsMessageIsError = true;
    } finally {
      savingSettings = false;
    }
  }

  async function handleScan() {
    scanning = true;
    assignmentError = '';
    try {
      await api.discoverPlugs();
      syncAssignmentDrafts(await api.getAssignments(), true);
    } catch (err) {
      assignmentError = err instanceof Error ? err.message : String(err);
    } finally {
      scanning = false;
    }
  }

  function discoveredForDriver(driver: PlugDriver): PlugTelemetry[] {
    const all = assignmentsData?.discovered_plugs ?? [];
    if (driver === 'shelly_gen1') {
      return all.filter((p) => p.driver === 'shelly_gen1');
    }
    if (driver === 'tasmota') {
      return all.filter((p) => !p.driver || p.driver === 'tasmota');
    }
    return [];
  }

  function plugByTopic(driver: PlugDriver, topic: string): PlugTelemetry | undefined {
    return discoveredForDriver(driver).find((p) => p.topic === topic);
  }

  function ownerOfTopic(
    driver: PlugDriver,
    topic: string,
    relayIndex: number,
    excludeDeviceId: string,
  ): string | undefined {
    if (!topic || !assignmentsData) return undefined;
    const targetRelay = relayIndex || 1;
    for (const [devId, cfg] of Object.entries(assignmentsData.assignments)) {
      if (
        devId !== excludeDeviceId &&
        effectiveDriver(cfg) === driver &&
        cfg.tasmota_topic === topic &&
        (cfg.relay_index || 1) === targetRelay
      ) {
        const dev = fleet.device(devId);
        return dev?.name || devId;
      }
    }
    return undefined;
  }

  function plugHasFreeRelay(
    driver: PlugDriver,
    plug: PlugTelemetry,
    excludeDeviceId: string,
  ): boolean {
    if (!plug.topic) return true;
    const count = Math.max(1, plug.relay_count ?? 1);
    for (let r = 1; r <= count; r++) {
      if (!ownerOfTopic(driver, plug.topic, r, excludeDeviceId)) {
        return true;
      }
    }
    return false;
  }

  function firstFreeRelay(
    driver: PlugDriver,
    plug: PlugTelemetry | undefined,
    excludeDeviceId: string,
  ): number {
    if (!plug?.topic) return 1;
    const count = Math.max(1, plug.relay_count ?? 1);
    for (let r = 1; r <= count; r++) {
      if (!ownerOfTopic(driver, plug.topic, r, excludeDeviceId)) {
        return r;
      }
    }
    return 1;
  }

  function plugLabel(
    driver: PlugDriver,
    plug: PlugTelemetry,
    currentDeviceId: string,
  ): string {
    const topic = plug.topic ?? '';
    const base =
      plug.friendly_name && plug.friendly_name !== topic
        ? `${plug.friendly_name} (${topic})`
        : topic;
    const relaysCount = Math.max(1, plug.relay_count ?? 1);
    const stateText =
      plug.online === false
        ? 'Offline'
        : relaysCount > 1
          ? `${relaysCount} relays`
          : plug.power;
    const watts =
      plug.online !== false && plug.power === 'ON' && plug.power_watts !== undefined
        ? ` · ${Math.round(plug.power_watts)} W`
        : '';
    let inUse = '';
    if (!plugHasFreeRelay(driver, plug, currentDeviceId)) {
      const owner = ownerOfTopic(driver, topic, 1, currentDeviceId);
      if (owner) {
        inUse = ` ${t('tasmota-power.settings.inUseBy', { printer: owner })}`;
      }
    }
    return `${base} · ${stateText}${watts}${inUse}`;
  }

  function draftCanSave(draft: DeviceDraft): boolean {
    switch (draft.driver) {
      case 'tasmota_http':
      case 'shelly_http':
        return draft.httpHost.trim() !== '';
      case 'homeassistant':
        return draft.haEntityId.trim() !== '';
      case 'generic_mqtt':
        return draft.mqttCmdTopic.trim() !== '';
      default:
        return draft.topic.trim() !== '';
    }
  }

  function driverSupportsMultiRelay(driver: PlugDriver): boolean {
    return (
      driver === 'tasmota' ||
      driver === 'tasmota_http' ||
      driver === 'shelly_gen1' ||
      driver === 'shelly_gen2' ||
      driver === 'shelly_http' ||
      driver === 'zigbee2mqtt' ||
      driver === 'openbeken'
    );
  }

  function topicPlaceholderForDriver(driver: PlugDriver): string {
    switch (driver) {
      case 'shelly_gen1':
        return t('tasmota-power.settings.shellyGen1Placeholder');
      case 'shelly_gen2':
        return t('tasmota-power.settings.shellyGen2Placeholder');
      case 'zigbee2mqtt':
        return t('tasmota-power.settings.z2mPlaceholder');
      case 'esphome':
        return t('tasmota-power.settings.esphomePlaceholder');
      case 'openbeken':
        return t('tasmota-power.settings.openbekenPlaceholder');
      default:
        return t('tasmota-power.settings.manualPlaceholder');
    }
  }

  async function saveDeviceDraft(deviceId: string) {
    const existing = assignmentsData?.assignments[deviceId];
    const d = getDraft(deviceId);
    const body: DeviceConfig = {
      driver: d.driver,
      tasmota_topic: '',
      auto_off_after_print: existing?.auto_off_after_print ?? false,
      cooldown_sec: existing?.cooldown_sec,
    };

    if (driverUsesHTTP(d.driver)) {
      body.mode = 'http';
      body.http_host = d.httpHost.trim();
      body.http_password = d.httpPassword;
      body.keep_http_password = d.httpPassword === '' && d.hasHttpPassword;
      body.relay_index = d.relay;
    } else if (d.driver === 'homeassistant') {
      body.mode = 'ha';
      body.ha_entity_id = d.haEntityId.trim();
      body.ha_power_entity_id = d.haPowerEntityId.trim();
    } else if (d.driver === 'generic_mqtt') {
      body.mode = 'mqtt';
      body.mqtt_command_topic = d.mqttCmdTopic.trim();
      body.mqtt_state_topic = d.mqttStateTopic.trim();
      body.mqtt_availability_topic = d.mqttAvailTopic.trim();
      body.mqtt_payload_on = d.mqttPayloadOn.trim();
      body.mqtt_payload_off = d.mqttPayloadOff.trim();
      body.mqtt_state_json_key = d.mqttStateJsonKey.trim();
      body.mqtt_power_topic = d.mqttPowerTopic.trim();
      body.mqtt_power_json_key = d.mqttPowerJsonKey.trim();
    } else {
      body.mode = 'mqtt';
      body.tasmota_topic = d.topic.trim();
      body.relay_index = d.relay;
    }

    savingDeviceId = deviceId;
    assignmentError = '';
    justSaved[deviceId] = false;
    try {
      await api.saveConfig(deviceId, body);
      syncAssignmentDrafts(await api.getAssignments());
      justSaved[deviceId] = true;
      setTimeout(() => {
        justSaved[deviceId] = false;
      }, 3000);
    } catch (err) {
      assignmentError = err instanceof Error ? err.message : String(err);
    } finally {
      savingDeviceId = '';
    }
  }

  async function unassignDevice(deviceId: string) {
    const existing = assignmentsData?.assignments[deviceId];
    savingDeviceId = deviceId;
    assignmentError = '';
    justSaved[deviceId] = false;
    try {
      await api.saveConfig(deviceId, {
        driver: '',
        mode: '',
        tasmota_topic: '',
        http_host: '',
        http_password: '',
        keep_http_password: false,
        ha_entity_id: '',
        ha_power_entity_id: '',
        mqtt_command_topic: '',
        auto_off_after_print: existing?.auto_off_after_print ?? false,
        cooldown_sec: existing?.cooldown_sec,
      });
      syncAssignmentDrafts(await api.getAssignments());
      justSaved[deviceId] = true;
      setTimeout(() => {
        justSaved[deviceId] = false;
      }, 3000);
    } catch (err) {
      assignmentError = err instanceof Error ? err.message : String(err);
    } finally {
      savingDeviceId = '';
    }
  }
</script>

<div class="flex flex-col gap-6">
  <!-- Pane 1: Unified MQTT Broker & General Configuration -->
  <form onsubmit={(e) => void handleSaveSettings(e)}>
    <SettingsPane
      title={t('tasmota-power.settings.mqttTitle')}
      description={t('tasmota-power.settings.mqttDescription')}
      edged
    >
      <div class="flex flex-col gap-4">
        <!-- Live Status Row -->
        <div class="flex flex-wrap items-center justify-between gap-2 rounded-sm border border-border bg-background/50 px-3 py-2 text-xs">
          <div class="flex flex-wrap items-center gap-2">
            {#if settingsMeta?.mqtt_connected}
              <Badge variant="success">
                {t('tasmota-power.settings.connected')}
              </Badge>
            {:else}
              <Badge variant="destructive">
                {t('tasmota-power.settings.disconnected')}
              </Badge>
            {/if}
            {#if settingsMeta?.mqtt_builtin_running}
              <Badge variant="outline">
                {t('tasmota-power.settings.builtinStatus', {
                  port: String(settingsMeta.mqtt_builtin_port ?? 1883),
                  clients: String(settingsMeta.mqtt_builtin_clients ?? 0),
                })}
              </Badge>
            {/if}
            <span class="text-muted-foreground font-mono">
              {settingsMeta?.mqtt_broker ||
                t('tasmota-power.settings.notConfigured')}
            </span>
          </div>
          {#if settingsMeta?.mqtt_error && !settingsMeta.mqtt_connected}
            <span class="text-red-600 dark:text-red-400 text-xs">
              {settingsMeta.mqtt_error}
            </span>
          {/if}
        </div>

        <!-- Optional Built-in MQTT Broker Toggle -->
        <div class="flex items-start justify-between gap-3 rounded-sm border border-border bg-background/40 px-3 py-2.5">
          <div class="flex flex-col gap-0.5">
            <Label for="mqtt-builtin" variant="option">
              {t('tasmota-power.settings.builtinBrokerLabel')}
            </Label>
            <p class="text-xs text-muted-foreground">
              {t('tasmota-power.settings.builtinBrokerHint')}
            </p>
            {#if mqttBuiltin}
              <p class="text-xs font-mono text-muted-foreground mt-1">
                {t('tasmota-power.settings.builtinTargetHint', {
                  host: mqttHost.trim() || defaultLanHost(),
                })}
              </p>
            {/if}
          </div>
          <Switch
            id="mqtt-builtin"
            checked={mqttBuiltin}
            onCheckedChange={handleBuiltinToggle}
          />
        </div>

        <!-- Row 1: Host | Port | TLS -->
        {#if !mqttBuiltin}
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-12 sm:items-end">
            <div class="sm:col-span-7 flex flex-col gap-1.5">
              <Label for="mqtt-host">
                {t('tasmota-power.settings.hostLabel')}
              </Label>
              <Input
                id="mqtt-host"
                type="text"
                placeholder={t('tasmota-power.settings.hostPlaceholder')}
                value={mqttHost}
                oninput={(e: Event) =>
                  (mqttHost = (e.currentTarget as HTMLInputElement).value)}
              />
            </div>

            <div class="sm:col-span-3 flex flex-col gap-1.5">
              <Label for="mqtt-port">
                {t('tasmota-power.settings.portLabel')}
              </Label>
              <Input
                id="mqtt-port"
                type="number"
                min="1"
                max="65535"
                value={mqttPort}
                oninput={(e: Event) =>
                  (mqttPort = (e.currentTarget as HTMLInputElement).value)}
              />
            </div>

            <div class="sm:col-span-2 flex items-center justify-between sm:justify-start gap-2 h-9">
              <Switch
                id="mqtt-tls"
                checked={mqttTls}
                onCheckedChange={handleTlsToggle}
              />
              <Label for="mqtt-tls" variant="option">
                {t('tasmota-power.settings.tlsLabel')}
              </Label>
            </div>
          </div>
        {/if}

        <!-- Row 2: Username & Password inline -->
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div class="flex flex-col gap-1.5">
            <Label for="mqtt-user">
              {t('tasmota-power.settings.usernameLabel')}
            </Label>
            <Input
              id="mqtt-user"
              type="text"
              autocomplete="username"
              placeholder={t('tasmota-power.settings.usernamePlaceholder')}
              value={mqttUsername}
              oninput={(e: Event) =>
                (mqttUsername = (e.currentTarget as HTMLInputElement).value)}
            />
          </div>

          <div class="flex flex-col gap-1.5">
            <Label for="mqtt-pass">
              {t('tasmota-power.settings.passwordLabel')}
            </Label>
            <Input
              id="mqtt-pass"
              type="password"
              autocomplete="current-password"
              placeholder={hasSavedPassword
                ? t('tasmota-power.settings.passwordSaved')
                : t('tasmota-power.settings.passwordPlaceholder')}
              value={mqttPassword}
              oninput={(e: Event) =>
                (mqttPassword = (e.currentTarget as HTMLInputElement).value)}
            />
          </div>
        </div>

        <!-- Row 3: Cooldown & Client ID -->
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div class="flex flex-col gap-1.5">
            <Label for="mqtt-cooldown">
              {t('tasmota-power.settings.cooldownLabel')}
            </Label>
            <Input
              id="mqtt-cooldown"
              type="number"
              min="0"
              value={cooldownSec}
              oninput={(e: Event) =>
                (cooldownSec = (e.currentTarget as HTMLInputElement).value)}
            />
            <p class="text-xs text-muted-foreground leading-relaxed">
              {t('tasmota-power.settings.cooldownHint')}
            </p>
          </div>

          <div class="flex flex-col gap-1.5">
            <Label for="mqtt-client-id">
              {t('tasmota-power.settings.clientIdLabel')}
            </Label>
            <Input
              id="mqtt-client-id"
              type="text"
              value={mqttClientId}
              oninput={(e: Event) =>
                (mqttClientId = (e.currentTarget as HTMLInputElement).value)}
            />
            <p class="text-xs text-muted-foreground leading-relaxed">
              {t('tasmota-power.settings.clientIdHint')}
            </p>
          </div>
        </div>

        <!-- Row 4: Turn-off confirmation toggle -->
        <div class="flex items-start justify-between gap-4 rounded-sm border border-border bg-background/40 px-3 py-2.5">
          <div class="flex flex-col gap-0.5">
            <Label for="confirm-turn-off" variant="option">
              {t('tasmota-power.settings.confirmTurnOffLabel')}
            </Label>
            <p class="text-xs text-muted-foreground leading-relaxed">
              {t('tasmota-power.settings.confirmTurnOffHint')}
            </p>
          </div>
          <Switch
            id="confirm-turn-off"
            checked={confirmTurnOff}
            onCheckedChange={(v: boolean) => (confirmTurnOff = v)}
          />
        </div>

        {#if settingsMessage}
          <div
            role={settingsMessageIsError ? 'alert' : 'status'}
            class={[
              'rounded-sm border px-3 py-2 text-xs font-medium',
              settingsMessageIsError
                ? 'border-destructive bg-destructive/15 text-red-600 dark:text-red-400'
                : 'border-success bg-success/15 text-emerald-700 dark:text-emerald-400',
            ]}
          >
            {settingsMessageIsError ? '✕ ' : '✓ '}
            {settingsMessage}
          </div>
        {/if}

        {#if testResult?.warning || settingsMeta?.mqtt_warning}
          <div
            role="alert"
            class="rounded-sm border border-amber-500/60 bg-amber-500/15 px-3 py-2 text-xs font-medium text-amber-800 dark:text-amber-300"
          >
            ⚠ {testResult?.warning || settingsMeta?.mqtt_warning}
          </div>
        {/if}
      </div>

      {#snippet actions()}
        <Button
          type="button"
          variant="secondary"
          disabled={testing || savingSettings || (!mqttBuiltin && !mqttHost.trim())}
          onclick={() => void handleTest()}
        >
          {testing
            ? t('tasmota-power.settings.testing')
            : t('tasmota-power.settings.testButton')}
        </Button>
        <Button
          type="submit"
          variant="default"
          disabled={savingSettings || testing}
        >
          {savingSettings
            ? t('tasmota-power.settings.saving')
            : t('tasmota-power.settings.saveButton')}
        </Button>
      {/snippet}
    </SettingsPane>
  </form>

  <!-- Pane 2: Optional Home Assistant REST Integration -->
  <SettingsPane
    title={t('tasmota-power.settings.haTitle')}
    description={t('tasmota-power.settings.haDescription')}
    edged
  >
    <div class="flex flex-col gap-4">
      <div class="flex flex-wrap items-center gap-2 rounded-sm border border-border bg-background/50 px-3 py-2 text-xs">
        {#if settingsMeta?.ha_configured}
          <Badge variant="success">
            {t('tasmota-power.settings.haConfigured')}
          </Badge>
          <span class="text-muted-foreground font-mono">
            {settingsMeta.settings.ha_url}
          </span>
        {:else}
          <Badge variant="outline">
            {t('tasmota-power.settings.haNotConfigured')}
          </Badge>
        {/if}
      </div>

      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div class="flex flex-col gap-1.5">
          <Label for="ha-url">
            {t('tasmota-power.settings.haUrlLabel')}
          </Label>
          <Input
            id="ha-url"
            type="text"
            placeholder={t('tasmota-power.settings.haUrlPlaceholder')}
            value={haUrl}
            oninput={(e: Event) =>
              (haUrl = (e.currentTarget as HTMLInputElement).value)}
          />
        </div>

        <div class="flex flex-col gap-1.5">
          <Label for="ha-token">
            {t('tasmota-power.settings.haTokenLabel')}
          </Label>
          <Input
            id="ha-token"
            type="password"
            placeholder={hasSavedHaToken
              ? t('tasmota-power.settings.haTokenSaved')
              : t('tasmota-power.settings.haTokenPlaceholder')}
            value={haToken}
            oninput={(e: Event) =>
              (haToken = (e.currentTarget as HTMLInputElement).value)}
          />
        </div>
      </div>

      {#if haMessage}
        <div
          role={haMessageIsError ? 'alert' : 'status'}
          class={[
            'rounded-sm border px-3 py-2 text-xs font-medium',
            haMessageIsError
              ? 'border-destructive bg-destructive/15 text-red-600 dark:text-red-400'
              : 'border-success bg-success/15 text-emerald-700 dark:text-emerald-400',
          ]}
        >
          {haMessageIsError ? '✕ ' : '✓ '}
          {haMessage}
        </div>
      {/if}
    </div>

    {#snippet actions()}
      <Button
        type="button"
        variant="secondary"
        disabled={testingHa || savingSettings || !haUrl.trim() || (!haToken.trim() && !hasSavedHaToken)}
        onclick={() => void handleTestHa()}
      >
        {testingHa
          ? t('tasmota-power.settings.testing')
          : t('tasmota-power.settings.haTestButton')}
      </Button>
      <Button
        type="button"
        variant="default"
        disabled={savingSettings || testingHa}
        onclick={() => void handleSaveSettings()}
      >
        {savingSettings
          ? t('tasmota-power.settings.saving')
          : t('tasmota-power.settings.saveButton')}
      </Button>
    {/snippet}
  </SettingsPane>

  <!-- Pane 3: Printer <-> Smart Plug Assignments -->
  <SettingsPane
    title={t('tasmota-power.settings.assignmentsTitle')}
    description={t('tasmota-power.settings.assignmentsDescription')}
    message={assignmentError || undefined}
    messageIsFailure={assignmentError !== ''}
    edged
  >
    <div class="flex flex-col gap-3">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <span class="text-xs text-muted-foreground">
          {t('tasmota-power.settings.discoveredCount', {
            count: String(assignmentsData?.discovered_plugs?.length ?? 0),
          })}
        </span>
        <Button
          type="button"
          size="sm"
          variant="secondary"
          disabled={scanning || !settingsMeta?.mqtt_connected}
          onclick={() => void handleScan()}
        >
          {scanning
            ? t('tasmota-power.settings.scanning')
            : t('tasmota-power.settings.scanButton')}
        </Button>
      </div>

      {#if devices.length === 0}
        <p class="text-sm text-muted-foreground py-2">
          {t('tasmota-power.settings.noPrinters')}
        </p>
      {:else}
        <div class="flex flex-col gap-3">
          {#each devices as dev (dev.id)}
            {@const currentCfg = assignmentsData?.assignments[dev.id]}
            {@const currentDriver = effectiveDriver(currentCfg)}
            {@const isCurrentAssigned = isDeviceConfigured(currentCfg)}
            {@const currentTarget = driverDisplayTarget(currentCfg)}
            {@const currentRelay = currentCfg?.relay_index || 1}
            {@const draft = getDraft(dev.id)}
            {@const discList = discoveredForDriver(draft.driver)}
            {@const hasDisc = discList.length > 0}
            {@const useDiscSelect =
              (draft.driver === 'tasmota' || draft.driver === 'shelly_gen1') &&
              hasDisc &&
              !draft.manual}
            {@const draftPlug = plugByTopic(draft.driver, draft.topic.trim())}
            {@const showRelay =
              driverSupportsMultiRelay(draft.driver) && draftCanSave(draft)}
            {@const relayCount = Math.max(
              4,
              draftPlug?.relay_count ?? 1,
              draft.relay,
            )}
            {@const busy = savingDeviceId === dev.id}

            <div class="flex flex-col gap-3 rounded-sm border border-border bg-background/40 p-3">
              <!-- Printer Header + Current Assignment Badge -->
              <div class="flex flex-wrap items-center justify-between gap-2">
                <div class="flex flex-wrap items-center gap-2 min-w-0">
                  <span class="text-sm font-semibold truncate">
                    {dev.name || dev.id}
                  </span>
                  <Badge variant="outline">{dev.model ?? 'Printer'}</Badge>
                  {#if isCurrentAssigned && currentDriver}
                    <Badge variant="secondary">
                      {DRIVER_BADGE_LABELS[currentDriver]} · {currentTarget}
                      {#if currentRelay > 1}
                        · {t('tasmota-power.section.relayBadge', {
                          relay: String(currentRelay),
                        })}
                      {/if}
                    </Badge>
                  {:else}
                    <Badge variant="outline">
                      {t('tasmota-power.settings.noPlugAssigned')}
                    </Badge>
                  {/if}
                  {#if justSaved[dev.id]}
                    <Badge variant="success">
                      ✓ {t('tasmota-power.settings.assignedSaved')}
                    </Badge>
                  {/if}
                </div>

                <div class="flex items-center gap-2">
                  <Button
                    type="button"
                    size="sm"
                    variant="default"
                    disabled={busy || !draftCanSave(draft)}
                    onclick={() => void saveDeviceDraft(dev.id)}
                  >
                    {t('tasmota-power.settings.saveTopic')}
                  </Button>
                  {#if isCurrentAssigned}
                    <Button
                      type="button"
                      size="sm"
                      variant="secondary"
                      disabled={busy}
                      onclick={() => void unassignDevice(dev.id)}
                    >
                      {t('tasmota-power.settings.unassign')}
                    </Button>
                  {/if}
                </div>
              </div>

              <!-- Driver Selector + Driver-Specific Inputs -->
              <div class="flex flex-wrap items-center gap-2">
                <select
                  aria-label={t('tasmota-power.settings.driverLabel')}
                  class="h-8 rounded-sm border border-input bg-background px-2.5 text-xs min-w-[210px]"
                  value={draft.driver}
                  disabled={busy}
                  onchange={(e) => {
                    const nextDriver = (e.currentTarget as HTMLSelectElement)
                      .value as PlugDriver;
                    updateDraft(dev.id, {
                      driver: nextDriver,
                      manual: false,
                    });
                  }}
                >
                  {#each PLUG_DRIVER_OPTIONS as opt (opt.value)}
                    <option value={opt.value}>{opt.label}</option>
                  {/each}
                </select>

                {#if driverUsesHTTP(draft.driver)}
                  <Input
                    type="text"
                    size="sm"
                    class="w-48"
                    placeholder={t('tasmota-power.settings.httpHostPlaceholder')}
                    value={draft.httpHost}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        httpHost: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                  <Input
                    type="password"
                    size="sm"
                    class="w-44"
                    placeholder={draft.hasHttpPassword
                      ? t('tasmota-power.settings.httpPasswordSaved')
                      : t('tasmota-power.settings.httpPasswordPlaceholder')}
                    value={draft.httpPassword}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        httpPassword: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                {:else if draft.driver === 'homeassistant'}
                  {#if haSwitchEntities.length > 0}
                    <select
                      class="h-8 rounded-sm border border-input bg-background px-2.5 text-xs min-w-[220px]"
                      value={draft.haEntityId}
                      disabled={busy}
                      onchange={(e) =>
                        updateDraft(dev.id, {
                          haEntityId: (e.currentTarget as HTMLSelectElement).value,
                        })}
                    >
                      <option value="">
                        {t('tasmota-power.settings.unassigned')}
                      </option>
                      {#each haSwitchEntities as ent (ent.entity_id)}
                        <option value={ent.entity_id}>
                          {ent.friendly_name
                            ? `${ent.friendly_name} (${ent.entity_id})`
                            : ent.entity_id}
                        </option>
                      {/each}
                    </select>
                  {:else}
                    <Input
                      type="text"
                      size="sm"
                      class="w-56"
                      placeholder={t('tasmota-power.settings.haEntityPlaceholder')}
                      value={draft.haEntityId}
                      oninput={(e: Event) =>
                        updateDraft(dev.id, {
                          haEntityId: (e.currentTarget as HTMLInputElement).value,
                        })}
                    />
                  {/if}

                  {#if haPowerEntities.length > 0}
                    <select
                      class="h-8 rounded-sm border border-input bg-background px-2.5 text-xs min-w-[200px]"
                      value={draft.haPowerEntityId}
                      disabled={busy}
                      onchange={(e) =>
                        updateDraft(dev.id, {
                          haPowerEntityId: (e.currentTarget as HTMLSelectElement)
                            .value,
                        })}
                    >
                      <option value="">
                        {t('tasmota-power.settings.haPowerNone')}
                      </option>
                      {#each haPowerEntities as ent (ent.entity_id)}
                        <option value={ent.entity_id}>
                          {ent.friendly_name
                            ? `${ent.friendly_name} (${ent.entity_id})`
                            : ent.entity_id}
                        </option>
                      {/each}
                    </select>
                  {:else}
                    <Input
                      type="text"
                      size="sm"
                      class="w-56"
                      placeholder={t(
                        'tasmota-power.settings.haPowerEntityPlaceholder',
                      )}
                      value={draft.haPowerEntityId}
                      oninput={(e: Event) =>
                        updateDraft(dev.id, {
                          haPowerEntityId: (e.currentTarget as HTMLInputElement)
                            .value,
                        })}
                    />
                  {/if}
                {:else if draft.driver === 'generic_mqtt'}
                  <Input
                    type="text"
                    size="sm"
                    class="w-52"
                    placeholder={t('tasmota-power.settings.genericCmdPlaceholder')}
                    value={draft.mqttCmdTopic}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttCmdTopic: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                  <Input
                    type="text"
                    size="sm"
                    class="w-52"
                    placeholder={t('tasmota-power.settings.genericStatePlaceholder')}
                    value={draft.mqttStateTopic}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttStateTopic: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                {:else if useDiscSelect}
                  <select
                    class="h-8 rounded-sm border border-input bg-background px-2.5 text-xs min-w-[230px]"
                    value={draft.topic}
                    disabled={busy}
                    onchange={(e) => {
                      const nextTopic = (e.currentTarget as HTMLSelectElement)
                        .value;
                      const nextRelay =
                        nextTopic === currentTarget
                          ? currentRelay
                          : firstFreeRelay(
                              draft.driver,
                              plugByTopic(draft.driver, nextTopic),
                              dev.id,
                            );
                      updateDraft(dev.id, {
                        topic: nextTopic,
                        relay: nextRelay,
                      });
                    }}
                  >
                    <option value="">
                      {t('tasmota-power.settings.unassigned')}
                    </option>
                    {#each discList as plug (plug.topic)}
                      {@const topic = plug.topic ?? ''}
                      {@const fullyUsed = !plugHasFreeRelay(
                        draft.driver,
                        plug,
                        dev.id,
                      )}
                      <option value={topic} disabled={fullyUsed}>
                        {plugLabel(draft.driver, plug, dev.id)}
                      </option>
                    {/each}
                  </select>
                {:else}
                  <Input
                    type="text"
                    size="sm"
                    class="w-60"
                    placeholder={topicPlaceholderForDriver(draft.driver)}
                    value={draft.topic}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        topic: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                {/if}

                {#if showRelay}
                  <select
                    class="h-8 rounded-sm border border-input bg-background px-2 text-xs"
                    value={String(draft.relay)}
                    disabled={busy}
                    onchange={(e) =>
                      updateDraft(dev.id, {
                        relay: Number.parseInt(
                          (e.currentTarget as HTMLSelectElement).value,
                          10,
                        ),
                      })}
                  >
                    {#each Array.from({ length: relayCount }, (_, i) => i + 1) as rNum (rNum)}
                      <option value={String(rNum)}>
                        {t('tasmota-power.settings.relayOption', {
                          relay: String(rNum),
                        })}
                      </option>
                    {/each}
                  </select>
                {/if}

                {#if (draft.driver === 'tasmota' || draft.driver === 'shelly_gen1') && hasDisc}
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    onclick={() =>
                      updateDraft(dev.id, { manual: !draft.manual })}
                  >
                    {t('tasmota-power.settings.manualToggle')}
                  </Button>
                {/if}
              </div>

              {#if draft.driver === 'generic_mqtt'}
                <div class="grid grid-cols-1 gap-2 sm:grid-cols-3">
                  <Input
                    type="text"
                    size="sm"
                    placeholder={t('tasmota-power.settings.genericAvailPlaceholder')}
                    value={draft.mqttAvailTopic}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttAvailTopic: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                  <Input
                    type="text"
                    size="sm"
                    placeholder={t(
                      'tasmota-power.settings.genericPayloadOnPlaceholder',
                    )}
                    value={draft.mqttPayloadOn}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttPayloadOn: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                  <Input
                    type="text"
                    size="sm"
                    placeholder={t(
                      'tasmota-power.settings.genericPayloadOffPlaceholder',
                    )}
                    value={draft.mqttPayloadOff}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttPayloadOff: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                  <Input
                    type="text"
                    size="sm"
                    placeholder={t(
                      'tasmota-power.settings.genericStateJsonPlaceholder',
                    )}
                    value={draft.mqttStateJsonKey}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttStateJsonKey: (e.currentTarget as HTMLInputElement)
                          .value,
                      })}
                  />
                  <Input
                    type="text"
                    size="sm"
                    placeholder={t(
                      'tasmota-power.settings.genericPowerTopicPlaceholder',
                    )}
                    value={draft.mqttPowerTopic}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttPowerTopic: (e.currentTarget as HTMLInputElement).value,
                      })}
                  />
                  <Input
                    type="text"
                    size="sm"
                    placeholder={t(
                      'tasmota-power.settings.genericPowerJsonPlaceholder',
                    )}
                    value={draft.mqttPowerJsonKey}
                    oninput={(e: Event) =>
                      updateDraft(dev.id, {
                        mqttPowerJsonKey: (e.currentTarget as HTMLInputElement)
                          .value,
                      })}
                  />
                </div>
              {/if}

              {#if draft.driver === 'homeassistant' && !settingsMeta?.ha_configured && haEntities.length === 0}
                <p class="text-xs text-muted-foreground">
                  {t('tasmota-power.settings.haNeedConfig')}
                </p>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </SettingsPane>
</div>
