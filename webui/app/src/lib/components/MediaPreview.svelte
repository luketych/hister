<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
  import { resolve } from '$app/paths';
  import { untrack } from 'svelte';
  import { apiFetch } from '#lib/api.js';
  import {
    failureMessage,
    modeLabel,
    parseMediaStatus,
    type MediaStatus,
  } from '#lib/media-preview.js';

  let { data }: { data: MediaStatus } = $props();
  let status = $state<MediaStatus>(untrack(() => data));
  let error = $state('');
  let polling = $state(false);
  let submitting = $state(false);
  let checking = $state(false);
  let checkedStatus = $state<MediaStatus | null>(null);
  let checkedAt = $state('');
  let refreshStatus: (manual?: boolean) => void = () => {};
  let canRetry = $derived(
    Boolean(
      status.attempt && ['failed', 'cancelled', 'interrupted'].includes(status.attempt.state),
    ),
  );
  let hasReadyMedia = $derived(status.available.some((artifact) => artifact.state === 'ready'));

  async function requestDownload() {
    if (submitting || checking || !status.document_id) return;
    submitting = true;
    error = '';
    checkedStatus = null;
    checkedAt = '';
    try {
      const retry =
        status.attempt && ['failed', 'cancelled', 'interrupted'].includes(status.attempt.state);
      const response = await apiFetch(`/yt_downloader/${retry ? 'retry' : 'start'}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(
          retry
            ? { document_id: status.document_id, attempt_id: status.attempt!.id }
            : { document_id: status.document_id },
        ),
      });
      if (!response.ok)
        throw new Error('Could not start the YouTube download. Check status and try again.');
      refreshStatus();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Could not start the YouTube download.';
    } finally {
      submitting = false;
    }
  }

  $effect(() => {
    const initial = data;
    status = initial;
    error = '';
    polling = initial.active;
    checking = false;
    checkedStatus = null;
    checkedAt = '';
    let current = initial;
    let disposed = false;
    let busy = false;
    let failures = 0;
    let deadline = Date.now() + 30 * 60 * 1000;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();

    function schedule(delay: number) {
      if (disposed || !current.active) return;
      if (Date.now() >= deadline) {
        polling = false;
        error = 'Automatic status checks paused. Check status to continue.';
        return;
      }
      timer = setTimeout(refresh, delay);
    }

    async function refresh(manual = false) {
      if (disposed || busy) return;
      clearTimeout(timer);
      busy = true;
      checking = true;
      if (manual) {
        checkedStatus = null;
        checkedAt = '';
      }
      let delay = 1000;
      try {
        const response = await apiFetch(
          `/yt_downloader/status?document_id=${encodeURIComponent(initial.document_id)}`,
          { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(10000)]) },
        );
        if (!response.ok) throw new Error('Status request failed');
        const next = parseMediaStatus(await response.text());
        if (!next || next.document_id !== initial.document_id) throw new Error('Invalid status');
        if (disposed) return;
        current = next;
        status = next;
        polling = next.active;
        error = '';
        if (manual) {
          checkedStatus = next;
          checkedAt = new Date().toISOString();
        }
        failures = 0;
      } catch {
        if (disposed) return;
        failures++;
        checkedStatus = null;
        checkedAt = '';
        error = 'Could not verify current status. Last confirmed state is shown.';
        delay = Math.min(30000, 1000 * 2 ** failures);
      } finally {
        busy = false;
        if (!disposed) checking = false;
        if (failures < 5) schedule(delay);
        else polling = false;
      }
    }

    refreshStatus = (manual = false) => {
      deadline = Date.now() + 30 * 60 * 1000;
      failures = 0;
      void refresh(manual);
    };
    // Immediate first read closes the window between the preview snapshot and
    // mounting. Subsequent requests are bounded, non-overlapping and active-only.
    if (initial.active) void refresh();
    return () => {
      disposed = true;
      clearTimeout(timer);
      controller.abort();
    };
  });
</script>

<article aria-label="Downloaded media" class="not-prose space-y-5">
  <h2 class="font-outfit text-text-brand text-xl font-bold">Downloaded media</h2>
  {#if error}
    <p role="alert" class="text-text-brand-muted text-sm">{error}</p>
  {/if}
  {#if status.unavailable}
    <p role="status">Media is unavailable while an ownership change is unresolved.</p>
  {:else}
    {#if status.available.length}
      <section aria-label="Saved download" class="space-y-2">
        <h3 class="font-semibold">Saved download</h3>
        {#each status.available as artifact (artifact.id)}
          <p>
            <strong>{artifact.role === 'video' ? 'Video' : 'Audio'}:</strong>
            {#if artifact.state === 'ready'}
              Ready — <a
                class="text-hister-teal underline"
                href={`${resolve('/')}api/yt_downloader/media?id=${encodeURIComponent(artifact.id)}`}
                target="_blank"
                rel="noopener noreferrer">Play {artifact.role}</a
              >
            {:else}
              {artifact.state === 'missing' ? 'Missing — not queued' : 'Retired — unavailable'}
            {/if}
          </p>
        {/each}
      </section>
    {/if}
    {#if status.attempt && status.attempt.state !== 'ready'}
      <section aria-label="Download attempt" class="space-y-2" aria-live="polite">
        <h3 class="font-semibold">{status.available.length ? 'New download' : 'Download'}</h3>
        <p>
          <strong>Video:</strong>
          {modeLabel(status.attempt.video, status.attempt.state, 'video')}
        </p>
        <p>
          <strong>Audio:</strong>
          {modeLabel(status.attempt.audio, status.attempt.state, 'audio')}
        </p>
        {#if failureMessage(status.attempt.error_code)}
          <p>{failureMessage(status.attempt.error_code)}</p>
          {#if !status.active}
            <p class="text-text-brand-muted text-sm">
              Use Retry download to request another attempt.
            </p>
          {/if}
        {/if}
      </section>
    {:else if !status.available.length}
      <p>No committed media files are available. No download is queued.</p>
    {/if}
    {#if !status.unavailable && !status.active && (canRetry || !hasReadyMedia)}
      <button
        class="text-hister-teal mr-4 cursor-pointer text-sm underline disabled:cursor-wait"
        disabled={submitting || checking}
        onclick={requestDownload}
      >
        {submitting
          ? 'Starting download…'
          : canRetry
            ? 'Retry download'
            : 'Download video and audio'}
      </button>
    {/if}
    {#if status.active}
      <p class="text-text-brand-muted text-sm" role="status">
        {polling ? 'Checking accepted work automatically…' : 'Automatic status checks paused.'}
      </p>
    {/if}
  {/if}
  <button
    class="text-hister-teal cursor-pointer text-sm underline disabled:cursor-wait"
    disabled={checking || submitting}
    onclick={() => refreshStatus(true)}
  >
    {checking ? 'Checking status…' : 'Check status'}
  </button>
  {#if checkedStatus && !error}
    <p role="status" class="sr-only">Status checked. Details are available below.</p>
    <details class="text-text-brand-muted text-sm">
      <summary class="text-hister-teal cursor-pointer underline"
        >Status checked — view details</summary
      >
      <div class="mt-2 space-y-1">
        <p>Checked: <time datetime={checkedAt}>{new Date(checkedAt).toLocaleString()}</time></p>
        {#if checkedStatus.unavailable}
          <p>Media status is unavailable while an ownership change is unresolved.</p>
        {:else}
          <p>Download active: {checkedStatus.active ? 'Yes' : 'No'}</p>
          {#if checkedStatus.available.length}
            {#each checkedStatus.available as artifact (artifact.id)}
              <p>Saved {artifact.role}: {artifact.state}</p>
            {/each}
          {:else}
            <p>No saved media is registered.</p>
          {/if}
          {#if checkedStatus.attempt}
            <p>Latest attempt: {checkedStatus.attempt.state}</p>
            <p>
              Video: {modeLabel(checkedStatus.attempt.video, checkedStatus.attempt.state, 'video')}
            </p>
            <p>
              Audio: {modeLabel(checkedStatus.attempt.audio, checkedStatus.attempt.state, 'audio')}
            </p>
            {#if failureMessage(checkedStatus.attempt.error_code)}
              <p>{failureMessage(checkedStatus.attempt.error_code)}</p>
            {/if}
          {:else}
            <p>No download attempt has been recorded.</p>
          {/if}
        {/if}
      </div>
    </details>
  {/if}
</article>
