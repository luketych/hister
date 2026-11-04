# Independent security review: media queue startup recovery

**Status:** M-01 is addressed by the database-lease mitigation below. Final independent code inspection found no actionable blocker; local SQLite/PostgreSQL, race, static, and real-producer acceptance checks passed. Final-head CI, packaging, and manual playback evidence are tracked in PR #1.

**Reviewed baseline:** `origin/master` at merge base `7741a6fffc146773a3bf0e2a960457678c956688`.

## Finding M-01 — Startup recovery can interrupt another live dispatcher

**Severity:** Medium, conditional on multiple Hister processes sharing one database.

**Affected code:** `server/media/queue.go` startup; `server/model/media.go` `InterruptMediaAttempts`.

**Cause:** Startup recovery marked every `starting`, `downloading`, and `finalizing` attempt interrupted and cleared its claim token. The recovery contract required old workers to be stopped, but the database did not coordinate that requirement across processes.

**Impact:** A second instance could mark a live worker's attempt interrupted while its downloader continued running. The original worker could no longer publish, losing the attempt and potentially leaving unregistered files behind.

## Remediation

- Added a database-backed `MediaQueueLease` with a 30-second expiration and 5-second renewal interval. Database time is used for expiry checks.
- Dispatcher startup and recovery use the same lease. Recovery and attempt claims verify/renew ownership in the same transaction, so an expired owner cannot claim work or release a successor's lease.
- A queue stops accepting work and cancels workers when renewal fails. Graceful shutdown stops workers before releasing the lease.
- Startup with media disabled uses the same lease; if another dispatcher owns it, startup skips recovery instead of interrupting that dispatcher's attempts.
- A process crash releases ownership by expiry; recovery may be delayed by up to 30 seconds. Lease fencing prevents stale database claims/publication after a successor recovers the attempts. As with any TTL lease, a long process pause or database outage can cause failover; workers are cancelled when lease loss is detected.

The lease is an additive AutoMigrate table. No existing media, database rows, or files were changed beyond test fixtures.

## Final inspection and validation

A separate read-only review of this mitigation found no actionable blocker. It confirmed lease-protected startup/recovery and claims, stale-owner release protection, worker cancellation after lease loss, and worker shutdown before lease release. This conclusion is scoped to M-01 and is not a claim of exhaustive security proof.

Passed locally on the integration tree:

```bash
nix develop --command go test -race -p 2 ./... \
  -skip '^Test(ResolveHistoryImportsRejectsUnknownBrowser|WatcherIgnoresDirectoryWrites)$' \
  -count=1
nix develop --command go vet ./...
nix develop --command nix shell --inputs-from . nixpkgs#golangci-lint \
  --command golangci-lint run --timeout 5m
git diff --check
git diff --cached --check
```

The two explicitly skipped macOS tests were previously reproduced failing on unmodified upstream: Safari history-import assumptions and a kqueue/fsnotify test race. They were not changed or globally disabled by this mitigation. Linux and Windows CI retain their normal, unskipped suite.

The initial unrestricted, resource-concurrent race run timed out in `TestMediaCommandsRetryIsIdempotentAndKeepsPreviousDownload`. Without changing its assertions or deadline, that test then passed ten consecutive race-enabled runs; the subsequent broad race run above also passed. The first failed run is not represented as a pass.

### PostgreSQL ownership and contention

Using an isolated PostgreSQL **16.15** instance over a protected UNIX socket, with TCP disabled, the updated CI selection passed with `-race -count=3`: **nine executions passed, none skipped or failed**, including all three executions of the new lease test.

```bash
# Set HISTER_TEST_POSTGRES_DSN to an isolated local test instance first.
nix develop --command go test -race ./server/model \
  -run '^TestMedia(SourceBootstrapPostgresSerializesApplyAndRejectsStalePlans|RetentionPostgresRetiresOnlyHistoricalArtifacts|QueuePostgresLockExcludesConcurrentOwners)$' \
  -count=3 -v
```

The owned PostgreSQL process was stopped and its temporary data removed. The workflow explicitly includes `TestMediaQueuePostgresLockExcludesConcurrentOwners`, so future CI runs exercise the new coverage rather than silently omitting it.

### Real-producer acceptance

With an isolated wheel built from producer `adfa5d71d75507c290c89cb599f13140abd5b256` (package 0.2; yt-dlp 2026.10.26), `TestLocalMediaCandidateEndToEnd` passed three consecutive local-media runs plus one race-enabled run. This includes real durable worker publication under the new lease. The test uses generated loopback media, not live YouTube.

All integration runs used isolated test data. No live cleanup/import or mutation of a normal Hister database was performed. Final publication and any later revalidation are recorded on [PR #1](https://github.com/luketych/hister/pull/1).
