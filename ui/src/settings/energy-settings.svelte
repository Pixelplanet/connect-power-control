<!--
  The "Energy and cost" pane of the Automation tab: how long energy records
  are kept, the time zone that days and night hours follow, the currency
  label and the price per kWh, with an optional second price for the night.

  These values are the process's own object, apart from the other settings,
  and a save sends only what changed. A field shows what is saved until a
  person types into it.

  With a price saved, the pane says what this month costs at it and links to
  the Energy tab, so a person sees at once what the price did.
-->
<script lang="ts">
  import {
    Alert,
    AlertDescription,
    Button,
    Combobox,
    Input,
    ListingSkeleton,
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
    SettingsPane,
    Switch,
  } from '@prusa/connect-ui/primitives';
  import { onMount } from 'svelte';

  import type {
    EnergySettingsChange,
    EnergySettingsResponse,
    PriceChange,
  } from '../api';
  import {
    browserZone,
    factOf,
    hasFigure,
    hourLabel,
    zoneItems,
  } from '../energy';
  import FaultNote from '../fault-note.svelte';
  import { formatCost } from '../format';
  import { heldHost } from '../host';
  import { decimalOf, typed, wholeOf } from '../input';
  import LinkButton from '../link-button.svelte';
  import { energyHref } from '../links';
  import { powerFor } from '../power.svelte';
  import { PANE_BODY } from './pane-body';
  import SettingRow from './setting-row.svelte';

  const MIN_KEEP = 7;
  const MAX_KEEP = 3650;
  /** The night hours offered when a night price is switched on. */
  const NIGHT_START = 22;
  const NIGHT_END = 6;
  const MAX_CURRENCY = 8;

  const host = heldHost();
  const t = host.t;
  const power = powerFor(host);
  const uid = $props.id();
  const here = browserZone();

  let saved = $state.raw<EnergySettingsResponse | undefined>(undefined);
  let loadFault = $state.raw<unknown>(undefined);

  /** What is typed, undefined while a field shows what is saved. */
  let keepText = $state<string | undefined>(undefined);
  let zoneText = $state<string | undefined>(undefined);
  let currencyText = $state<string | undefined>(undefined);
  let priceText = $state<string | undefined>(undefined);
  let nightChoice = $state<boolean | undefined>(undefined);
  let nightPriceText = $state<string | undefined>(undefined);
  let nightStartChoice = $state<number | undefined>(undefined);
  let nightEndChoice = $state<number | undefined>(undefined);
  /** The new price replaces every earlier price. */
  let past = $state(false);

  let saving = $state(false);
  let fault = $state.raw<unknown>(undefined);
  let justSaved = $state(false);

  /**
   * A change of a field, which also takes away what the last save said: a
   * "Saved." or a refusal is about what was in the fields then.
   */
  const edit =
    <T,>(set: (value: T) => void) =>
    (value: T): void => {
      fault = undefined;
      justSaved = false;
      set(value);
    };

  onMount(() => {
    power.api.getEnergySettings().then(
      (read) => {
        saved = read;
        void power.refreshSummary();
      },
      (thrown: unknown) => {
        loadFault = thrown;
      },
    );
  });

  const savedPrice = $derived(saved?.price ?? null);
  const savedKeep = $derived(saved?.keep_days ?? 0);
  /** Nothing is deleted for its age until a person saves the keep time. */
  const paused = $derived(saved?.prune_paused === true);

  const keep = $derived(keepText ?? String(savedKeep));
  const keepDays = $derived(wholeOf(keep, savedKeep));
  const keepChanged = $derived(keepDays !== savedKeep);
  const shorter = $derived(keepDays < savedKeep);

  const zone = $derived(zoneText ?? saved?.time_zone ?? '');
  const zoneChanged = $derived(zone.trim() !== (saved?.time_zone ?? ''));
  /**
   * The zones to pick from. The saved zone and this browser's are always
   * among them: the process may know a name this browser does not list.
   */
  const zones = $derived(zoneItems([saved?.time_zone ?? '', here]));

  const currency = $derived(currencyText ?? saved?.currency ?? '');
  const currencyChanged = $derived(currency.trim() !== (saved?.currency ?? ''));

  const price = $derived(
    priceText ?? (savedPrice === null ? '' : String(savedPrice.price)),
  );
  const night = $derived(nightChoice ?? savedPrice?.night ?? false);
  const nightPrice = $derived(
    nightPriceText ??
      (savedPrice?.night === true ? String(savedPrice.night_price) : ''),
  );
  const nightStart = $derived(
    nightStartChoice ??
      (savedPrice?.night === true ? savedPrice.night_start : NIGHT_START),
  );
  const nightEnd = $derived(
    nightEndChoice ??
      (savedPrice?.night === true ? savedPrice.night_end : NIGHT_END),
  );

  /** An empty price field means "no price", which is not a wrong price. */
  const priceEmpty = $derived(price.trim() === '');
  // Saving an empty price deletes every saved price, past ones included.
  const clearing = $derived(priceEmpty && savedPrice !== null);
  const priceInvalid = $derived(!priceEmpty && decimalOf(price) === undefined);
  const nightPriceInvalid = $derived(
    !priceEmpty && night && decimalOf(nightPrice) === undefined,
  );
  const sameHours = $derived(!priceEmpty && night && nightStart === nightEnd);

  /** The price the fields describe, undefined for none or for one not valid. */
  const draftPrice = $derived.by((): PriceChange | undefined => {
    const day = decimalOf(price);
    if (day === undefined) return undefined;
    if (!night) {
      return {
        price: day,
        night: false,
        night_price: 0,
        night_start: 0,
        night_end: 0,
      };
    }
    const byNight = decimalOf(nightPrice);
    if (byNight === undefined) return undefined;

    return {
      price: day,
      night: true,
      night_price: byNight,
      night_start: nightStart,
      night_end: nightEnd,
    };
  });

  const samePrice = (a: PriceChange, b: PriceChange): boolean =>
    a.price === b.price &&
    a.night === b.night &&
    (!a.night ||
      (a.night_price === b.night_price &&
        a.night_start === b.night_start &&
        a.night_end === b.night_end));

  const priceChanged = $derived.by((): boolean => {
    if (priceEmpty) return savedPrice !== null;
    if (draftPrice === undefined) return true;

    return savedPrice === null || !samePrice(draftPrice, savedPrice);
  });
  /** A saved price is being replaced, so a person says from when. */
  const asksPast = $derived(
    savedPrice !== null && draftPrice !== undefined && priceChanged,
  );

  /**
   * What this month costs at the saved price, in words. Undefined with no
   * price, while the fields hold another price, and until the figures are read.
   */
  const priceResult = $derived.by((): string | undefined => {
    const summary = power.summary;
    if (saved === undefined || saved.price === null) return undefined;
    if (priceChanged || summary === undefined) return undefined;
    const month = summary.this_month;
    if (!hasFigure(factOf(month)) || month.cost === null) {
      return t('power.automation.energy.result.none');
    }

    return t(
      summary.price_changed
        ? 'power.automation.energy.result.mixed'
        : 'power.automation.energy.result.month',
      { cost: formatCost(month.cost, summary.currency) },
    );
  });

  const dirty = $derived(
    saved !== undefined &&
      (keepChanged || zoneChanged || currencyChanged || priceChanged || paused),
  );
  const blocked = $derived(priceInvalid || nightPriceInvalid || sameHours);

  const hours = Array.from({ length: 24 }, (_, hour) => ({
    value: String(hour),
    label: hourLabel(hour),
  }));

  const hourOf = (value: string): number => Number.parseInt(value, 10) || 0;

  /** What a save sends: only what a person changed. */
  const changeOf = (): EnergySettingsChange => {
    const change: EnergySettingsChange = {};
    if (keepChanged || paused) {
      change.keep_days = keepDays;
      if (shorter || paused) change.shorten = true;
    }
    if (zoneChanged) change.time_zone = zone.trim();
    if (currencyChanged) change.currency = currency.trim();
    if (priceChanged) {
      if (draftPrice === undefined) {
        change.clear_price = true;
      } else {
        change.price = draftPrice;
        change.price_from = asksPast && past ? 'start' : 'now';
      }
    }

    return change;
  };

  const save = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    if (!dirty || blocked) return;
    saving = true;
    fault = undefined;
    justSaved = false;
    try {
      const answer = await power.api.saveEnergySettings(changeOf());
      // The month is read at the new price before the pane says anything.
      await power.refreshSummary();
      saved = answer;
      keepText = undefined;
      zoneText = undefined;
      currencyText = undefined;
      priceText = undefined;
      nightChoice = undefined;
      nightPriceText = undefined;
      nightStartChoice = undefined;
      nightEndChoice = undefined;
      past = false;
      justSaved = true;
    } catch (thrown) {
      fault = thrown;
    } finally {
      saving = false;
    }
  };
</script>

<form onsubmit={(event) => void save(event)} data-part="energy-settings">
  <SettingsPane
    title={t('power.automation.energy.title')}
    description={t('power.automation.energy.description')}
    edged
  >
    <div class={PANE_BODY}>
      {#if saved === undefined}
        <FaultNote fault={loadFault} />
        {#if loadFault === undefined}
          <ListingSkeleton label={t('power.automation.reading')} rows={4} />
        {/if}
      {:else}
        <SettingRow
          id={`${uid}-keep`}
          label={t('power.automation.energy.keep.label')}
          description={t('power.automation.energy.keep.hint')}
        >
          {#snippet control()}
            <div class="w-24">
              <Input
                type="number"
                min={MIN_KEEP}
                max={MAX_KEEP}
                aria-labelledby={`${uid}-keep-label`}
                aria-describedby={`${uid}-keep-description`}
                value={keep}
                oninput={typed(edit((value) => (keepText = value)))}
              />
            </div>
            <span class="text-muted-foreground text-sm">
              {t('power.automation.energy.keep.unit')}
            </span>
          {/snippet}
        </SettingRow>
        {#if paused}
          <Alert variant="warning" data-part="keep-paused">
            <AlertDescription>
              {t('power.automation.energy.keep.paused', { days: keepDays })}
            </AlertDescription>
          </Alert>
        {:else if shorter}
          <Alert variant="warning" data-part="keep-shorter">
            <AlertDescription>
              {t('power.automation.energy.keep.shorter', { days: keepDays })}
            </AlertDescription>
          </Alert>
        {/if}

        <SettingRow
          id={`${uid}-zone`}
          label={t('power.automation.energy.zone.label')}
          description={t('power.automation.energy.zone.hint')}
        >
          {#snippet control()}
            <div class="w-72 max-w-full" data-part="zone">
              {#if zones.length > 0}
                <Combobox
                  items={zones}
                  value={zone === '' ? undefined : zone}
                  onPick={edit((picked: string | undefined) => {
                    zoneText = picked ?? '';
                  })}
                  label={t('power.automation.energy.zone.label')}
                  placeholder={t('power.automation.energy.zone.server')}
                  noneLabel={t('power.automation.energy.zone.server')}
                  aria-describedby={`${uid}-zone-description`}
                />
              {:else}
                <Input
                  type="text"
                  autocomplete="off"
                  spellcheck={false}
                  aria-labelledby={`${uid}-zone-label`}
                  aria-describedby={`${uid}-zone-description`}
                  value={zone}
                  oninput={typed(edit((value) => (zoneText = value)))}
                />
              {/if}
            </div>
            {#if here !== '' && zone.trim() !== here}
              <Button
                variant="secondary"
                size="sm"
                data-part="use-zone"
                onclick={edit(() => (zoneText = here))}
              >
                {t('power.automation.energy.zone.use', { zone: here })}
              </Button>
            {/if}
          {/snippet}
        </SettingRow>

        <SettingRow
          id={`${uid}-currency`}
          label={t('power.automation.energy.currency.label')}
          description={t('power.automation.energy.currency.hint')}
        >
          {#snippet control()}
            <div class="w-24">
              <Input
                type="text"
                maxlength={MAX_CURRENCY}
                autocomplete="off"
                aria-labelledby={`${uid}-currency-label`}
                aria-describedby={`${uid}-currency-description`}
                value={currency}
                oninput={typed(edit((value) => (currencyText = value)))}
              />
            </div>
          {/snippet}
        </SettingRow>

        <SettingRow
          id={`${uid}-price`}
          label={t(
            night
              ? 'power.automation.energy.price.dayLabel'
              : 'power.automation.energy.price.label',
          )}
          description={t('power.automation.energy.price.hint')}
        >
          {#snippet control()}
            <div class="w-24">
              <Input
                type="text"
                inputmode="decimal"
                autocomplete="off"
                aria-labelledby={`${uid}-price-label`}
                aria-describedby={`${uid}-price-description`}
                aria-invalid={priceInvalid}
                value={price}
                oninput={typed(edit((value) => (priceText = value)))}
              />
            </div>
            <span class="text-muted-foreground text-sm">{currency.trim()}</span>
          {/snippet}
        </SettingRow>
        {#if priceInvalid}
          <p class="text-muted-foreground text-xs" data-part="price-invalid">
            {t('power.automation.energy.price.invalid')}
          </p>
        {:else if clearing}
          <Alert variant="warning" data-part="price-clearing">
            <AlertDescription>
              {t('power.automation.energy.price.clearing')}
            </AlertDescription>
          </Alert>
        {/if}

        <SettingRow
          id={`${uid}-night`}
          label={t('power.automation.energy.night.label')}
          description={t('power.automation.energy.night.hint')}
        >
          {#snippet control()}
            <Switch
              aria-labelledby={`${uid}-night-label`}
              aria-describedby={`${uid}-night-description`}
              checked={night}
              onCheckedChange={edit((next: boolean) => (nightChoice = next))}
            />
          {/snippet}
        </SettingRow>

        {#if night}
          <SettingRow
            id={`${uid}-night-price`}
            label={t('power.automation.energy.nightPrice.label')}
            description={t('power.automation.energy.nightPrice.hint')}
          >
            {#snippet control()}
              <div class="w-24">
                <Input
                  type="text"
                  inputmode="decimal"
                  autocomplete="off"
                  aria-labelledby={`${uid}-night-price-label`}
                  aria-describedby={`${uid}-night-price-description`}
                  aria-invalid={nightPriceInvalid}
                  value={nightPrice}
                  oninput={typed(edit((value) => (nightPriceText = value)))}
                />
              </div>
              <span class="text-muted-foreground text-sm">
                {currency.trim()}
              </span>
            {/snippet}
          </SettingRow>
          {#if nightPriceInvalid}
            <p
              class="text-muted-foreground text-xs"
              data-part="night-price-invalid"
            >
              {t('power.automation.energy.price.invalid')}
            </p>
          {/if}

          <SettingRow
            id={`${uid}-night-start`}
            label={t('power.automation.energy.nightStart.label')}
            description={t('power.automation.energy.nightStart.hint')}
          >
            {#snippet control()}
              <Select
                value={String(nightStart)}
                items={hours}
                onValueChange={edit(
                  (next: string) => (nightStartChoice = hourOf(next)),
                )}
              >
                <SelectTrigger
                  class="w-28"
                  aria-labelledby={`${uid}-night-start-label`}
                  aria-describedby={`${uid}-night-start-description`}
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent
                  aria-label={t('power.automation.energy.nightStart.label')}
                >
                  {#each hours as hour (hour.value)}
                    <SelectItem value={hour.value}>{hour.label}</SelectItem>
                  {/each}
                </SelectContent>
              </Select>
            {/snippet}
          </SettingRow>

          <SettingRow
            id={`${uid}-night-end`}
            label={t('power.automation.energy.nightEnd.label')}
            description={t('power.automation.energy.nightEnd.hint')}
          >
            {#snippet control()}
              <Select
                value={String(nightEnd)}
                items={hours}
                onValueChange={edit(
                  (next: string) => (nightEndChoice = hourOf(next)),
                )}
              >
                <SelectTrigger
                  class="w-28"
                  aria-labelledby={`${uid}-night-end-label`}
                  aria-describedby={`${uid}-night-end-description`}
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent
                  aria-label={t('power.automation.energy.nightEnd.label')}
                >
                  {#each hours as hour (hour.value)}
                    <SelectItem value={hour.value}>{hour.label}</SelectItem>
                  {/each}
                </SelectContent>
              </Select>
            {/snippet}
          </SettingRow>
          {#if sameHours}
            <p class="text-muted-foreground text-xs" data-part="night-invalid">
              {t('power.automation.energy.night.invalid')}
            </p>
          {/if}
        {/if}

        {#if asksPast}
          <SettingRow
            id={`${uid}-past`}
            label={t('power.automation.energy.past.label')}
            description={t('power.automation.energy.past.hint')}
          >
            {#snippet control()}
              <Switch
                aria-labelledby={`${uid}-past-label`}
                aria-describedby={`${uid}-past-description`}
                checked={past}
                onCheckedChange={edit((next: boolean) => (past = next))}
              />
            {/snippet}
          </SettingRow>
        {/if}

        <FaultNote {fault} />
        {#if justSaved}
          <Alert variant="success" data-part="saved">
            <AlertDescription>{t('power.connections.saved')}</AlertDescription>
          </Alert>
        {/if}
        {#if priceResult !== undefined}
          <Alert data-part="price-result">
            <AlertDescription>
              <div class="flex flex-wrap items-center justify-between gap-3">
                <span>{priceResult}</span>
                <LinkButton href={energyHref(host)}>
                  {t('power.automation.energy.result.open')}
                </LinkButton>
              </div>
            </AlertDescription>
          </Alert>
        {/if}
      {/if}
    </div>

    {#snippet actions()}
      <Button type="submit" disabled={!dirty || blocked} pending={saving}>
        {t('power.connections.save')}
      </Button>
    {/snippet}
  </SettingsPane>
</form>
