<script lang="ts" module>
  import type { AutoWindow, ScheduleSettings } from '../api/devices';

  export interface ScheduleForm<Days extends number> {
    on: boolean;
    everyDays: Days;
    windowOn: boolean;
    start: string;
    end: string;
  }

  /** The saved settings, with `blank` for what was never set. */
  export function scheduleForm<Days extends number>(
    saved: ScheduleSettings<Days> | undefined,
    blank: { everyDays: Days; start: string; end: string },
  ): ScheduleForm<Days> {
    return {
      on: saved?.enabled ?? false,
      everyDays: saved?.everyDays ?? blank.everyDays,
      windowOn: !!saved?.window,
      start: saved?.window?.start ?? blank.start,
      end: saved?.window?.end ?? blank.end,
    };
  }

  function windowOf({ windowOn, start, end }: ScheduleForm<number>): AutoWindow | undefined {
    return windowOn && start && end && start !== end ? { start, end } : undefined;
  }

  export function scheduleSettings<Days extends number>(form: ScheduleForm<Days>): ScheduleSettings<Days> {
    return { enabled: form.on, everyDays: form.everyDays, window: windowOf(form) };
  }

  /** The fields of a task turned off are hidden, so only one that is on must have a valid window. */
  export function windowInvalid(form: ScheduleForm<number>): boolean {
    return form.on && form.windowOn && !windowOf(form);
  }
</script>

<script lang="ts" generics="Days extends number">
  // How often an automatic task runs and the daily window it may start in.
  let {
    presets,
    labels,
    form = $bindable(),
    timeZone,
    disabled,
  }: {
    presets: readonly Days[];
    labels: Record<Days, string>;
    form: ScheduleForm<Days>;
    timeZone: string | undefined;
    disabled: boolean;
  } = $props();
</script>

<label class="flex flex-wrap items-center gap-x-2 gap-y-1">
  <span>How often</span>
  <select class="select select-sm w-44" bind:value={form.everyDays} {disabled}>
    {#each presets as days (days)}
      <option value={days}>{labels[days]}</option>
    {/each}
  </select>
</label>

<div class="flex flex-col gap-2">
  <label class="label cursor-pointer gap-2 text-sm text-base-content">
    <input type="checkbox" class="checkbox checkbox-sm" bind:checked={form.windowOn} {disabled} />
    <span>Only between</span>
  </label>
  {#if form.windowOn}
    <div class="flex items-center gap-2">
      <input type="time" class="input input-sm w-32" bind:value={form.start} {disabled} aria-label="From" />
      <span class="text-base-content/60">–</span>
      <input type="time" class="input input-sm w-32" bind:value={form.end} {disabled} aria-label="Until" />
    </div>
    <p class="text-xs text-base-content/60">Server time ({timeZone}); the window may cross midnight</p>
  {/if}
</div>
