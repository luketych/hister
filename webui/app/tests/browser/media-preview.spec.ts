// SPDX-License-Identifier: AGPL-3.0-or-later
import { expect, test, type Page } from '@playwright/test';
import { mockAPI, previewPath } from './fixtures';
import type { MediaStatus } from '../../src/lib/media-preview';

const sourceURL = 'https://www.youtube.com/watch?v=abcdefghijk';
const queued: MediaStatus = {
  document_id: 'document-1',
  available: [],
  active: true,
  attempt: { id: 'attempt-1', generation: 1, state: 'queued', video: 'queued', audio: 'queued' },
};
const ready: MediaStatus = {
  document_id: 'document-1',
  available: [
    { id: 'video-1', role: 'video', state: 'ready' },
    { id: 'audio-1', role: 'audio', state: 'ready' },
  ],
  active: false,
  attempt: { id: 'attempt-1', generation: 1, state: 'ready', video: 'ready', audio: 'ready' },
};

async function previewStatus(page: Page, status: unknown) {
  await page.route('**/api/preview?**', (route) =>
    route.fulfill({
      json: { title: 'Saved video', template: 'yt_downloader', content: JSON.stringify(status) },
    }),
  );
}

test.beforeEach(async ({ page }) => {
  await mockAPI(page);
  await page.clock.install();
});

test('extractor menu uses only the canonical downloader name', async ({ page }) => {
  await page.route('**/api/extractors?**', (route) =>
    route.fulfill({
      json: [
        { name: 'basic', description: 'Stored content' },
        { name: 'yt_downloader', description: 'Downloaded media status' },
      ],
    }),
  );
  await page.route('**/api/preview?**', (route) => {
    const name = new URL(route.request().url()).searchParams.get('extractor');
    return route.fulfill({
      json:
        name === 'yt_downloader'
          ? { title: 'Saved video', template: 'yt_downloader', content: JSON.stringify(ready) }
          : { title: 'Saved video', content: '<p>Normal content preview</p>' },
    });
  });
  await page.goto(previewPath(sourceURL));
  await expect(page.getByText('Normal content preview', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Change extractor' }).click();
  await expect(
    page.getByRole('menuitemradio', { name: 'yt_video_downloader', exact: true }),
  ).toHaveCount(0);
  await page.getByRole('menuitemradio', { name: 'yt_downloader', exact: true }).click();
  await expect(page.getByRole('article', { name: 'Downloaded media' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Play video', exact: true })).toHaveAttribute(
    'href',
    '/api/yt_downloader/media?id=video-1',
  );
});

test('the preview explicitly starts both media modes and reports command failures', async ({
  page,
}) => {
  await previewStatus(page, {
    document_id: 'document-1',
    available: [],
    active: false,
    attempt: null,
  });
  let calls = 0;
  await page.route('**/api/yt_downloader/start', async (route) => {
    calls++;
    expect(route.request().method()).toBe('POST');
    expect(route.request().postDataJSON()).toEqual({ document_id: 'document-1' });
    await route.fulfill({ status: 503, json: { error: 'unavailable' } });
  });
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  const button = article.getByRole('button', { name: 'Download video and audio' });
  await button.click();
  await expect(article.getByRole('alert')).toContainText('Could not start the YouTube download');
  expect(calls).toBe(1);
  await button.click();
  expect(calls).toBe(2);
});

test('first download shows queued progress; rapid clicks submit only once', async ({ page }) => {
  await previewStatus(page, {
    document_id: 'document-1',
    available: [],
    active: false,
    attempt: null,
  });
  let calls = 0;
  let release!: () => void;
  const pendingResponse = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route('**/api/yt_downloader/start', async (route) => {
    calls++;
    await pendingResponse;
    await route.fulfill({
      status: 202,
      json: { attempt_id: 'attempt-1', state: 'queued', replayed: false },
    });
  });
  await page.route('**/api/yt_downloader/status?**', (route) => route.fulfill({ json: queued }));
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  const actionButton = article.getByRole('button', {
    name: /Download video and audio|Starting download/,
  });
  await actionButton.click();
  // The accessible name changes immediately while the command is in flight;
  // match both labels so this assertion targets that same disabled button.
  await expect(actionButton).toBeDisabled();
  await actionButton.click({ force: true });
  expect(calls).toBe(1);
  release();
  await expect(article).toContainText('Video: In queue');
  await expect(article).toContainText('Audio: In queue');
  await expect(
    article.getByRole('button', { name: /Download video and audio|Starting download/ }),
  ).toHaveCount(0);
});

test('failed attempt retry posts its attempt ID and restores active progress', async ({ page }) => {
  const failed: MediaStatus = {
    document_id: 'document-1',
    available: [],
    active: false,
    attempt: {
      id: 'failed-attempt',
      generation: 1,
      state: 'failed',
      video: 'failed',
      audio: 'failed',
      error_code: 'download_failed',
    },
  };
  await previewStatus(page, failed);
  await page.route('**/api/yt_downloader/retry', async (route) => {
    expect(route.request().method()).toBe('POST');
    expect(route.request().postDataJSON()).toEqual({
      document_id: 'document-1',
      attempt_id: 'failed-attempt',
    });
    await route.fulfill({
      status: 202,
      json: { attempt_id: 'attempt-2', state: 'queued', replayed: false },
    });
  });
  await page.route('**/api/yt_downloader/status?**', (route) => route.fulfill({ json: queued }));
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  await article.getByRole('button', { name: 'Retry download' }).click();
  await expect(article).toContainText('Video: In queue');
  await expect(article).toContainText('Audio: In queue');
});

test('pending files never link; publication appears automatically and stops polling', async ({
  page,
}) => {
  let current = structuredClone(queued);
  let reads = 0;
  await previewStatus(page, queued);
  await page.route('**/api/yt_downloader/status?**', (route) => {
    reads++;
    expect(new URL(route.request().url()).searchParams.get('document_id')).toBe('document-1');
    return route.fulfill({ json: current });
  });
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  await expect(article).toContainText('Video: In queue');
  await expect(article.getByRole('link')).toHaveCount(0);
  await expect.poll(() => reads).toBe(1);

  current.attempt = { ...queued.attempt!, state: 'downloading', video: 'downloading' };
  await page.clock.fastForward(1000);
  await expect(article).toContainText('Video: Downloading');
  await expect(article.getByRole('link')).toHaveCount(0);
  current.attempt = {
    ...queued.attempt!,
    state: 'downloading',
    video: 'downloaded',
    audio: 'downloading',
  };
  await page.clock.fastForward(1000);
  await expect(article).toContainText('Video: Downloaded — waiting for audio');
  await expect(article).toContainText('Audio: Downloading');
  await expect(article.getByRole('link')).toHaveCount(0);
  current.attempt = {
    ...queued.attempt!,
    state: 'finalizing',
    video: 'downloaded',
    audio: 'downloaded',
  };
  await page.clock.fastForward(1000);
  await expect(article).toContainText('Video: Finalizing');
  await expect(article.getByRole('link')).toHaveCount(0);

  current = structuredClone(ready);
  await page.clock.fastForward(1000);
  await expect(article.getByRole('link', { name: 'Play video', exact: true })).toHaveAttribute(
    'href',
    '/api/yt_downloader/media?id=video-1',
  );
  await expect(article.getByRole('link', { name: 'Play audio', exact: true })).toHaveAttribute(
    'href',
    '/api/yt_downloader/media?id=audio-1',
  );
  const terminalReads = reads;
  await page.clock.fastForward(60000);
  expect(reads).toBe(terminalReads);
});

test('failed refresh keeps saved playback and retries the latest attempt', async ({ page }) => {
  let current: MediaStatus = {
    ...structuredClone(queued),
    available: structuredClone(ready.available),
  };
  await previewStatus(page, current);
  await page.route('**/api/yt_downloader/status?**', (route) => route.fulfill({ json: current }));
  await page.route('**/api/yt_downloader/retry', async (route) => {
    expect(route.request().method()).toBe('POST');
    expect(route.request().postDataJSON()).toEqual({
      document_id: 'document-1',
      attempt_id: 'attempt-1',
    });
    current = {
      ...current,
      active: true,
      attempt: { ...queued.attempt!, id: 'attempt-2', generation: 2 },
    };
    await route.fulfill({
      status: 202,
      json: { attempt_id: 'attempt-2', state: 'queued', replayed: false },
    });
  });
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  await expect(article).toContainText('New download');
  await expect(article).toContainText('Video: In queue');
  await expect(article.getByRole('link')).toHaveCount(2);
  current = {
    ...current,
    active: false,
    attempt: {
      ...queued.attempt!,
      state: 'failed',
      video: 'downloaded',
      audio: 'failed',
      error_code: 'download_failed',
    },
  };
  await page.clock.fastForward(1000);
  await expect(article).toContainText('Downloaded — not published');
  await expect(article).toContainText('Audio: Failed');
  await expect(article).toContainText('The download failed.');
  await expect(article.getByRole('link')).toHaveCount(2);
  await expect(article.getByRole('link', { name: 'Play video', exact: true })).toHaveAttribute(
    'href',
    '/api/yt_downloader/media?id=video-1',
  );
  await article.getByRole('button', { name: 'Retry download' }).click();
  await expect(article).toContainText('Video: In queue');
  await expect(article).toContainText('Audio: In queue');
  await expect(article.getByRole('link')).toHaveCount(2);
  await expect(article.getByRole('link', { name: 'Play audio', exact: true })).toHaveAttribute(
    'href',
    '/api/yt_downloader/media?id=audio-1',
  );
  await expect(article.getByRole('button', { name: 'Retry download' })).toHaveCount(0);
});

test('missing, retired and absent files are not ready or queued', async ({ page }) => {
  let reads = 0;
  await page.route('**/api/yt_downloader/status?**', (route) => {
    reads++;
    return route.fulfill({ json: ready });
  });
  await previewStatus(page, {
    ...ready,
    available: [
      { id: 'video-1', role: 'video', state: 'missing' },
      { id: 'audio-1', role: 'audio', state: 'retired' },
    ],
  });
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  await expect(article).toContainText('Missing — not queued');
  await expect(article).toContainText('Retired — unavailable');
  await expect(article.getByRole('link')).toHaveCount(0);
  await page.clock.fastForward(10000);
  expect(reads).toBe(0);
  await previewStatus(page, {
    document_id: 'document-1',
    available: [],
    attempt: null,
    active: false,
  });
  await page.reload();
  await expect(article).toContainText('No download is queued.');
  await expect(article.getByRole('link')).toHaveCount(0);
});

test('failed checks back off, preserve last confirmed state, and can be retried', async ({
  page,
}) => {
  let reads = 0;
  await previewStatus(page, queued);
  await page.route('**/api/yt_downloader/status?**', (route) => {
    reads++;
    return route.fulfill({ status: 503, body: 'unavailable' });
  });
  await page.goto(previewPath(sourceURL));
  const article = page.getByRole('article', { name: 'Downloaded media' });
  await expect(article.getByRole('alert')).toContainText('Last confirmed state is shown');
  expect(reads).toBe(1);
  await page.clock.fastForward(1000);
  expect(reads).toBe(1);
  await page.clock.fastForward(1000);
  await expect.poll(() => reads).toBe(2);
  for (const [delay, count] of [
    [4000, 3],
    [8000, 4],
    [16000, 5],
  ]) {
    await page.clock.fastForward(delay);
    await expect.poll(() => reads).toBe(count);
  }
  await expect(article).toContainText('Automatic status checks paused.');
  await page.clock.fastForward(60000);
  expect(reads).toBe(5);
  await expect(article.getByRole('link')).toHaveCount(0);
  await page.route('**/api/yt_downloader/status?**', (route) => route.fulfill({ json: ready }));
  await article.getByRole('button', { name: 'Check status' }).click();
  await expect(article.getByRole('link')).toHaveCount(2);
  await expect(article.getByRole('alert')).toHaveCount(0);
});

test('closing a preview stops active status polling', async ({ page, isMobile }) => {
  let reads = 0;
  await previewStatus(page, queued);
  await page.route('**/api/yt_downloader/status?**', (route) => {
    reads++;
    return route.fulfill({ json: queued });
  });
  await page.goto('/?q=video');
  if (isMobile)
    await page
      .locator('[data-result]')
      .first()
      .getByRole('link', { name: 'Preview', exact: true })
      .click();
  await expect(page.getByRole('article', { name: 'Downloaded media' })).toBeVisible();
  await expect.poll(() => reads).toBeGreaterThan(0);
  await page.getByRole('button', { name: 'Close preview', exact: true }).click();
  await expect(page.locator('.preview-panel')).toHaveCount(0);
  const closedReads = reads;
  await page.clock.fastForward(60000);
  expect(reads).toBe(closedReads);
});

test('malformed or mismatched responses cannot manufacture ready links', async ({ page }) => {
  await previewStatus(page, {
    ...ready,
    available: [{ id: 'fake', role: 'video', state: 'queued' }],
  });
  await page.goto(previewPath(sourceURL));
  await expect(page.getByRole('alert')).toContainText('Could not load this preview');
  await expect(page.getByRole('link', { name: 'Play video', exact: true })).toHaveCount(0);
  await previewStatus(page, queued);
  await page.route('**/api/yt_downloader/status?**', (route) =>
    route.fulfill({ json: { ...ready, document_id: 'other-document' } }),
  );
  await page.reload();
  const article = page.getByRole('article', { name: 'Downloaded media' });
  await expect(article.getByRole('alert')).toContainText('Could not verify current status');
  await expect(article.getByRole('link')).toHaveCount(0);
});
