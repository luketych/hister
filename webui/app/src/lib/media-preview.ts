// SPDX-License-Identifier: AGPL-3.0-or-later

export type MediaState =
  | 'queued'
  | 'starting'
  | 'downloading'
  | 'downloaded'
  | 'finalizing'
  | 'ready'
  | 'failed'
  | 'cancelled'
  | 'interrupted';

export interface MediaStatus {
  document_id: string;
  available: { id: string; role: 'video' | 'audio'; state: 'ready' | 'missing' | 'retired' }[];
  attempt: {
    id: string;
    generation: number;
    state: MediaState;
    video: MediaState;
    audio: MediaState;
    error_code?: string;
  } | null;
  active: boolean;
  unavailable?: string;
}

const states = new Set<MediaState>([
  'queued',
  'starting',
  'downloading',
  'downloaded',
  'finalizing',
  'ready',
  'failed',
  'cancelled',
  'interrupted',
]);

/** Unknown/malformed status must not manufacture ready links. */
export function parseMediaStatus(content: string): MediaStatus | null {
  try {
    const data = JSON.parse(content);
    if (
      !data ||
      typeof data.document_id !== 'string' ||
      !data.document_id ||
      typeof data.active !== 'boolean' ||
      !Array.isArray(data.available) ||
      data.available.length > 2
    )
      return null;
    const roles = new Set();
    for (const artifact of data.available) {
      if (
        !artifact ||
        typeof artifact.id !== 'string' ||
        !artifact.id ||
        !['video', 'audio'].includes(artifact.role) ||
        !['ready', 'missing', 'retired'].includes(artifact.state) ||
        roles.has(artifact.role)
      )
        return null;
      roles.add(artifact.role);
    }
    if (data.attempt !== null) {
      const attempt = data.attempt;
      if (
        !attempt ||
        typeof attempt.id !== 'string' ||
        !attempt.id ||
        !Number.isSafeInteger(attempt.generation) ||
        attempt.generation < 1 ||
        !states.has(attempt.state) ||
        !states.has(attempt.video) ||
        !states.has(attempt.audio)
      )
        return null;
    }
    return data;
  } catch {
    return null;
  }
}

export function modeLabel(state: MediaState, attempt: MediaState, role: 'video' | 'audio'): string {
  if (state === 'downloaded') {
    if (attempt === 'finalizing') return 'Finalizing';
    if (['failed', 'cancelled', 'interrupted'].includes(attempt))
      return 'Downloaded — not published';
    return `Downloaded — waiting for ${role === 'video' ? 'audio' : 'video'}`;
  }
  const labels: Record<MediaState, string> = {
    queued: 'In queue',
    starting: 'Starting',
    downloading: 'Downloading',
    downloaded: 'Downloaded',
    finalizing: 'Finalizing',
    ready: 'Completed',
    failed: 'Failed',
    cancelled: 'Cancelled',
    interrupted: 'Interrupted',
  };
  return labels[state];
}

export function failureMessage(code?: string): string {
  switch (code) {
    case 'start_failed':
      return 'The downloader could not start.';
    case 'download_failed':
      return 'The download failed.';
    case 'validation_failed':
      return 'The downloaded files could not be verified.';
    case 'publication_failed':
      return 'The files could not be safely published.';
    case 'index_write_failed':
      return 'Indexing or queue acceptance did not finish.';
    case 'dispatcher_disabled':
      return 'Work was interrupted because the downloader is disabled.';
    case 'interrupted':
      return 'Work was interrupted. No new files were published.';
    case 'cancelled':
      return 'The download was cancelled.';
    case 'ownership_changed':
      return 'Work was cancelled because ownership changed.';
    default:
      return '';
  }
}
