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
  let refreshStatus: () => void = () => {};

  $effect(() => {
    const initial = data;
    status = initial;
    error = '';
    polling = initial.active;
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

    async function refresh() {
      if (disposed || busy) return;
      clearTimeout(timer);
      busy = true;
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
        failures = 0;
      } catch {
        if (disposed) return;
        failures++;
        error = 'Could not verify current status. Last confirmed state is shown.';
        delay = Math.min(30000, 1000 * 2 ** failures);
      } finally {
        busy = false;
        if (failures < 5) schedule(delay);
        else polling = false;
      }
    }

    refreshStatus = () => {
      deadline = Date.now() + 30 * 60 * 1000;
      failures = 0;
      void refresh();
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
              Re-submit this page to request another download.
            </p>
          {/if}
        {/if}
      </section>
    {:else if !status.available.length}
      <p>No committed media files are available. No download is queued.</p>
    {/if}
    {#if status.active}
      <p class="text-text-brand-muted text-sm" role="status">
        {polling ? 'Checking accepted work automatically…' : 'Automatic status checks paused.'}
      </p>
    {/if}
  {/if}
  <button class="text-hister-teal cursor-pointer text-sm underline" onclick={() => refreshStatus()}>
    Check status
  </button>
</article>
