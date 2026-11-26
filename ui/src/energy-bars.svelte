<!--
  The energy of each part of a period, as bars. The library has a line chart
  and no bar chart, and a line through seven or thirty sums reads as a trend
  that is not there.

  Every period of the span has its place, so one bar in an empty month still
  stands on its own day. A period that was on with no reading gets a low grey
  bar, and a period with no record gets nothing: the three facts stay apart
  here as in the tables.

  The line above the plot reads one period out. Moving the pointer over the
  plot, or tapping it, picks the period, and so do the arrow keys, Home and
  End while the plot has the focus. The plot is a slider over the periods,
  and the hidden list under it says every period, which is how the library's
  own chart does it. All geometry is in SVG attributes.

  The bars stand for energy, or for cost when the pane asks for that. The
  readout and the hidden list say both either way.
-->
<script lang="ts">
  import type { EnergyPoint, EnergySpan } from './api';
  import {
    axisLabel,
    factOf,
    hasFigure,
    readoutOf,
    repeatedHours,
  } from './energy';
  import { formatCostFine, formatEnergy } from './format';
  import { heldHost } from './host';

  let {
    points,
    currency,
    priceSet,
    span,
    measure = 'energy',
    class: className,
  }: {
    points: readonly EnergyPoint[];
    currency: string;
    /** With no price the readout names no cost. */
    priceSet: boolean;
    span: EnergySpan;
    /** What the height of a bar stands for. */
    measure?: 'energy' | 'cost';
    class?: string;
  } = $props();

  const t = heldHost().t;

  /** The height of the plot in its own units. Bars are in percent of it. */
  const HEIGHT = 100;
  /** The room at each side of a bar, in parts of a period. */
  const GAP = 0.14;
  /** The bar of a period that was on with no reading. */
  const UNMEASURED = 4;
  /** The least a measured bar is drawn with, so a small one is still seen. */
  const LEAST = 1;

  /** The period a person picked, undefined while the newest one shows. */
  let picked = $state<number | undefined>(undefined);

  const facts = $derived(points.map((point) => factOf(point)));

  /** What a bar is as high as: the energy of its period, or its cost. */
  const amountOf = (point: EnergyPoint): number =>
    measure === 'cost' ? (point.cost ?? 0) : point.energy_wh;

  /** A figure on the scale beside the plot. */
  const scaleText = (amount: number): string =>
    measure === 'cost'
      ? formatCostFine(amount, currency)
      : formatEnergy(amount);

  /** The top of the scale: a round figure at or above the largest period. */
  const top = $derived.by((): number => {
    const largest = Math.max(
      0,
      ...points.map((point, index) =>
        hasFigure(facts[index] ?? 'none') ? amountOf(point) : 0,
      ),
    );
    if (largest <= 0) return 0;
    const power = 10 ** Math.floor(Math.log10(largest));
    const step = [1, 2, 2.5, 5, 10].find((one) => one * power >= largest);

    return (step ?? 10) * power;
  });

  const bars = $derived(
    points.map((point, index) => {
      const fact = facts[index] ?? 'none';
      if (hasFigure(fact) && top > 0) {
        const amount = amountOf(point);
        const height = Math.max(
          (amount / top) * HEIGHT,
          amount > 0 ? LEAST : 0,
        );

        return { key: point.start, index, height, measured: true };
      }

      return {
        key: point.start,
        index,
        height: fact === 'notMeasured' ? UNMEASURED : 0,
        measured: false,
      };
    }),
  );

  /** The newest period with a record, where the readout starts. */
  const newest = $derived.by((): number => {
    for (let index = facts.length - 1; index >= 0; index -= 1) {
      if (facts[index] !== 'none') return index;
    }

    return -1;
  });
  const shown = $derived(
    picked !== undefined && picked < points.length ? picked : newest,
  );
  const shownPoint = $derived(shown >= 0 ? points[shown] : undefined);

  /** The hours that come twice, on the day the clocks go back. */
  const twice = $derived(repeatedHours(points));

  const say = (point: EnergyPoint): string =>
    readoutOf(
      t,
      priceSet ? point : { ...point, cost: null },
      span,
      currency,
      span === 'day' && twice.has(point.hour),
    );

  // How a name lies in the place of its period. A name is centred under its
  // bar and may reach over the places beside it. At the first and the last
  // place a name wider than the place starts or ends at the edge of the plot
  // instead, so it is not cut: the margins centre it while there is room,
  // and with no room left the place decides the side.
  const PLACE = {
    first: { box: 'justify-start', name: 'mx-auto' },
    middle: { box: 'justify-center', name: undefined },
    last: { box: 'justify-end', name: 'mx-auto' },
  } as const;

  const sideOf = (index: number, count: number): keyof typeof PLACE => {
    if (index === 0) return 'first';

    return index === count - 1 ? 'last' : 'middle';
  };

  /**
   * The periods that are named under the plot: the first, the middle and the
   * last one. Each name stands in the place of its own period.
   */
  const named = $derived(
    new Set([0, Math.floor((points.length - 1) / 2), points.length - 1]),
  );
  const places = $derived(
    points.map((point, index) => ({
      key: point.start,
      index,
      text: named.has(index) ? axisLabel(point, span) : undefined,
      side: sideOf(index, points.length),
    })),
  );

  /** Picks the period under the pointer. */
  const pick = (event: PointerEvent): void => {
    const plot = event.currentTarget;
    if (!(plot instanceof HTMLElement) || points.length === 0) return;
    const box = plot.getBoundingClientRect();
    if (box.width <= 0) return;
    const part = (event.clientX - box.left) / box.width;
    const along = getComputedStyle(plot).direction === 'rtl' ? 1 - part : part;
    picked = Math.min(
      points.length - 1,
      Math.max(0, Math.floor(along * points.length)),
    );
  };

  /** Moves the pick with the keys a slider takes. */
  const step = (event: KeyboardEvent): void => {
    const last = points.length - 1;
    if (last < 0 || !(event.currentTarget instanceof HTMLElement)) return;
    const rtl = getComputedStyle(event.currentTarget).direction === 'rtl';
    const from = shown >= 0 ? shown : 0;
    const to = {
      ArrowLeft: from + (rtl ? 1 : -1),
      ArrowRight: from + (rtl ? -1 : 1),
      ArrowDown: from - 1,
      ArrowUp: from + 1,
      Home: 0,
      End: last,
    }[event.key];
    if (to === undefined) return;
    event.preventDefault();
    picked = Math.min(last, Math.max(0, to));
  };
</script>

<div class={['flex flex-col gap-3', className]} data-part="energy-chart">
  <p
    class="text-sm font-medium tabular-nums"
    data-part="energy-readout"
    aria-hidden="true"
  >
    {shownPoint === undefined ? t('power.energy.emptySpan') : say(shownPoint)}
  </p>
  <div
    role="group"
    aria-label={t(
      measure === 'cost'
        ? 'power.energy.chart.labelCost'
        : 'power.energy.chart.label',
    )}
    class="text-muted-foreground flex w-full text-xs"
  >
    {#if top > 0}
      <div
        class="flex h-40 flex-none flex-col justify-between pe-2 text-end tabular-nums"
      >
        <span data-part="energy-scale">{scaleText(top)}</span>
        <span>{scaleText(top / 2)}</span>
        <span>{scaleText(0)}</span>
      </div>
    {/if}
    <div class="flex min-w-0 flex-1 flex-col gap-1">
      <div
        class="border-border focus-visible:ring-ring h-40 touch-pan-y border-b focus-visible:ring-2 focus-visible:outline-none"
        data-part="energy-plot"
        role="slider"
        tabindex="0"
        aria-label={t('power.energy.chart.pick')}
        aria-valuemin={0}
        aria-valuemax={Math.max(points.length - 1, 0)}
        aria-valuenow={Math.max(shown, 0)}
        aria-valuetext={shownPoint === undefined
          ? t('power.energy.emptySpan')
          : say(shownPoint)}
        onpointermove={pick}
        onpointerdown={pick}
        onkeydown={step}
      >
        <svg
          aria-hidden="true"
          viewBox="0 0 {Math.max(points.length, 1)} {HEIGHT}"
          preserveAspectRatio="none"
          class="size-full rtl:-scale-x-100"
        >
          {#if shown >= 0}
            <rect
              x={shown}
              y="0"
              width="1"
              height={HEIGHT}
              class="fill-accent"
              data-part="energy-picked"
            />
          {/if}
          {#if top > 0}
            <line
              x1="0"
              x2={points.length}
              y1="0"
              y2="0"
              vector-effect="non-scaling-stroke"
              class="stroke-border"
            />
            <line
              x1="0"
              x2={points.length}
              y1={HEIGHT / 2}
              y2={HEIGHT / 2}
              vector-effect="non-scaling-stroke"
              class="stroke-border"
            />
          {/if}
          {#each bars as bar (bar.key)}
            {#if bar.height > 0}
              <rect
                x={bar.index + GAP}
                y={HEIGHT - bar.height}
                width={1 - 2 * GAP}
                height={bar.height}
                class={bar.measured ? 'fill-primary' : 'fill-muted-foreground'}
                data-part="energy-bar"
                data-measured={bar.measured}
              />
            {/if}
          {/each}
        </svg>
      </div>
      <div class="flex tabular-nums" data-part="energy-axis">
        {#each places as place (place.key)}
          <div class={['flex min-w-0 flex-1', PLACE[place.side].box]}>
            {#if place.text !== undefined}
              <span
                class={['whitespace-nowrap', PLACE[place.side].name]}
                data-part="energy-tick"
                data-index={place.index}
              >
                {place.text}
              </span>
            {/if}
          </div>
        {/each}
      </div>
    </div>
  </div>
  <ul class="sr-only">
    {#each points as point, index (point.start)}
      {#if facts[index] !== 'none'}
        <li>{say(point)}</li>
      {/if}
    {/each}
  </ul>
</div>
