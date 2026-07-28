<script lang="ts">
  import { theme } from '../theme.svelte';
  import Icon from './Icon.svelte';
</script>

<label
  class="btn btn-ghost btn-circle swap swap-rotate"
  title={theme.isDark ? 'Switch to light theme' : 'Switch to dark theme'}
>
  <input
    type="checkbox"
    checked={theme.isDark}
    onchange={() => theme.toggle()}
    aria-label="Toggle color theme"
  />
  <Icon name="sun" size={18} class="swap-on" />
  <Icon name="moon" size={18} class="swap-off" />
</label>
