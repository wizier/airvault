<script lang="ts">
  // The inline script in index.html applies the saved theme before first paint;
  // this toggle reads it back from <html> and persists changes under the same
  // key and daisyUI theme names (see src/app.css).
  import Icon from './Icon.svelte';

  const root = document.documentElement;
  let dark = $state(root.dataset.theme !== 'airvault');

  function toggle(): void {
    dark = !dark;
    root.dataset.theme = dark ? 'airvaultdark' : 'airvault';
    try {
      localStorage.setItem('airvault-theme', dark ? 'dark' : 'light');
    } catch {
      /* private mode / storage disabled — ignore */
    }
  }
</script>

<label
  class="btn btn-ghost btn-circle swap swap-rotate"
  title={dark ? 'Switch to light theme' : 'Switch to dark theme'}
>
  <input type="checkbox" checked={dark} onchange={toggle} aria-label="Toggle color theme" />
  <Icon name="sun" size={18} class="swap-on" />
  <Icon name="moon" size={18} class="swap-off" />
</label>
