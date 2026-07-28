<script lang="ts">
  import { push } from 'svelte-spa-router';
  import Logo from '../lib/components/Logo.svelte';
  import Icon from '../lib/components/Icon.svelte';
  import { login } from '../lib/api/session';
  import { errRef } from '../lib/api/client';
  import { errorRefText, type ErrorRef } from '../lib/error-text';

  let token = $state('');
  let busy = $state(false);
  let failure = $state<ErrorRef | null>(null);
  const error = $derived(failure ? errorRefText(failure) : null);

  async function submit(event: Event) {
    event.preventDefault();
    const value = token.trim();
    if (!value || busy) return;
    busy = true;
    failure = null;
    try {
      await login(value);
      push('/');
    } catch (err) {
      failure = errRef(err, 'sign_in_failed');
    } finally {
      busy = false;
    }
  }

</script>

<div class="flex min-h-screen items-center justify-center bg-base-200 px-4">
  <div class="card w-full max-w-sm border border-base-300 bg-base-100 shadow-sm">
    <form class="card-body gap-4" onsubmit={submit}>
      <div class="flex flex-col items-center gap-2 text-center">
        <Logo size={40} />
        <h1 class="text-lg font-semibold tracking-tight">Sign in to AirVault</h1>
        <p class="text-sm text-base-content/60">
          Paste your Web UI token
        </p>
      </div>

      <input
        type="password"
        class="input w-full"
        placeholder="Web UI token"
        autocomplete="current-password"
        aria-label="Web UI token"
        bind:value={token}
        disabled={busy}
        {@attach (node) => node.focus()}
      />

      {#if error}
        <div role="alert" class="alert alert-error alert-soft">
          <Icon name="alert" size={16} />
          <span>{error}</span>
        </div>
      {/if}

      <button type="submit" class="btn btn-primary" disabled={busy || !token.trim()}>
        {#if busy}<span class="loading loading-spinner loading-sm"></span>{/if}
        Sign in
      </button>
    </form>
  </div>
</div>
