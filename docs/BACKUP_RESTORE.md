# Backup, Restore, and Artifact Reconciliation

[Українська версія](BACKUP_RESTORE.uk.md)

> **State and failure map:** [SCHEMA.md](SCHEMA.md) identifies every durable and
> process-local state boundary, multi-instance coordination mechanism, and
> verification point used by this recovery procedure.

Back up the metadata database and artifact bucket as one recovery set. Also back up the deployment configuration and independent secret values for `ACCESS_TOKEN_PEPPER`, `MANAGE_SESSION_SECRET`, `OIDC_COOKIE_SECRET`, OIDC client credentials, storage credentials, and any bootstrap `ADMIN_TOKEN`. Losing or changing a secret invalidates the corresponding stored token hashes or browser sessions.

## Backup

1. For sequential database and bucket backups, stop all Forge replicas and any
   separate mutation/repair jobs. Wait for in-flight requests and workers to
   finish, and prevent automatic restarts until both backups complete. Pausing
   publishing and deletion alone is insufficient: Clear cache, background
   refresh/cleanup, and client-triggered upstream caching can still change SQL
   or storage. Alternatively, use a coordinated database/storage snapshot
   procedure that guarantees one consistent recovery point; two independent
   live snapshots do not provide that guarantee.
2. Back up PostgreSQL with the platform snapshot mechanism or `pg_dump --format=custom`. For SQLite, stop the only writer and copy the database file, or use SQLite's online backup API.
   The database backup includes the durable `artifact_deletions` outbox. Pending
   object cleanup therefore resumes automatically after restore and startup. It
   also includes retained `release_consumers` history used by the Grafana legacy
   usage view; normal `RELEASE_CONSUMER_TTL` cleanup resumes after startup.
3. Snapshot or version-copy the complete configured bucket. At minimum, include both the local-release namespace below `ARTIFACT_PREFIX` and the independent `upstream-cache/` namespace referenced by indexed upstream releases.
4. Export the effective non-secret configuration and store the required secrets in the organization's secret backup system.
5. Record the application version and verify that both database and object
   backups completed before restarting replicas or resuming mutation jobs.

## Restore

1. Keep the service scaled to zero and block publish traffic.
2. Restore object storage first, including both the local-release namespace below `ARTIFACT_PREFIX` and `upstream-cache/` from the same recovery set.
3. Restore PostgreSQL or SQLite metadata from the matching recovery set.
   Do not remove restored `artifact_deletions` rows manually. The singleton
   cleanup worker rechecks each object path against current release metadata
   before deleting it and retries transient storage failures.
4. Restore the original hashing/session/OIDC secrets and runtime configuration.
5. Run reconciliation in report mode before starting the service:

   ```bash
   puppet-forge --reconcile-artifacts
   ```

   The command exits non-zero when it finds missing objects, checksum/size mismatches, or orphan objects. Its JSON report can be archived with the recovery record.

   Reconciliation checks every non-empty `storage_path` registered in SQL,
   including upstream archives, for existence, size, and checksum. Orphan
   discovery scans only the configured `ARTIFACT_PREFIX` (normally `modules/`).
   It does not discover unregistered objects in the separate `upstream-cache/`
   namespace. Those are handled by the daily lease-elected upstream cleanup
   worker after startup, subject to `UPSTREAM_ARTIFACT_ORPHAN_TTL` and current
   reference checks. A clean report is not proof that the entire bucket has no
   orphan objects.

6. Resolve missing or corrupt artifacts from backup. Do not delete their SQL metadata automatically.
7. To remove eligible orphan artifacts under `ARTIFACT_PREFIX`, run the explicit
   repair mode:

   ```bash
   puppet-forge --reconcile-artifacts --reconcile-repair
   ```

   Repair mode deletes only reported orphans with a recognized artifact path
   under that prefix; unrecognized paths require operator investigation.
   Before each deletion it takes the corresponding module lock, rechecks
   current SQL references, and skips objects created or changed after the scan
   started, so concurrent publishing cannot turn a stale report into deletion
   of a live artifact. It never deletes release metadata, replaces corrupt
   artifacts, or performs orphan cleanup outside the configured prefix.
8. Repeat report mode until it is clean, then start one replica, verify `/readyz`, downloads, OIDC/token login, and publish/delete behavior before restoring normal replica count.

## Disaster-Recovery Test

Test the complete procedure periodically in an isolated environment. A backup is not considered valid until the database, artifacts, secrets, reconciliation, and representative module downloads have all been restored successfully.
