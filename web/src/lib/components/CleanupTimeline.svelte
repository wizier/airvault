<script lang="ts">
  // The phone's backups on a line from the oldest to the latest, marked with
  // what cleanup does to each.
  import type { RestorePoint } from '../api/backups';

  let { points, remove }: { points: RestorePoint[]; remove: ReadonlySet<string> } = $props();

  const times = $derived(points.map((point) => new Date(point.created).getTime()));
  const oldest = $derived(Math.min(...times));
  const latest = $derived(Math.max(...times));
  const removed = $derived(points.filter((point) => remove.has(point.snapshotId)));
  const staying = $derived(points.filter((point) => !remove.has(point.snapshotId)));

  function left(point: RestorePoint): string {
    const span = latest - oldest;
    const at = new Date(point.created).getTime();
    return `${span > 0 ? ((at - oldest) / span) * 100 : 100}%`;
  }

  function day(time: number): string {
    return new Date(time).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }
</script>

<div class="flex flex-col gap-1.5 rounded-box bg-base-200 px-4 py-3">
  <div class="flex justify-between text-[11px] text-base-content/50">
    <span>{day(oldest)}</span>
    <span>{day(latest)}</span>
  </div>
  <div class="relative mx-1.5 h-5" role="img" aria-label={`${staying.length} backups stay, ${removed.length} are removed`}>
    {#each removed as point (point.snapshotId)}
      <span class="absolute top-1/2 h-2 w-px -translate-x-1/2 -translate-y-1/2 bg-base-content/25" style:left={left(point)}></span>
    {/each}
    {#each staying as point (point.snapshotId)}
      <span
        class="absolute top-1/2 h-4 w-0.5 -translate-x-1/2 -translate-y-1/2 rounded-full bg-primary"
        style:left={left(point)}
      ></span>
    {/each}
  </div>
  <div class="flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-base-content/60">
    <span class="flex items-center gap-1.5"><span class="h-3 w-0.5 rounded-full bg-primary"></span>stays</span>
    <span class="flex items-center gap-1.5"><span class="h-2 w-px bg-base-content/40"></span>removed</span>
  </div>
</div>
