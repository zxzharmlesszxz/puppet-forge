# Backup, Restore, and Artifact Reconciliation

[Українська версія](BACKUP_RESTORE.uk.md)

> **State and failure map:** [SCHEMA.md](SCHEMA.md) identifies every durable and
> process-local state boundary, multi-instance coordination mechanism, and
> verification point used by this recovery procedure.

Back up the metadata database and artifact bucket as one recovery set. Also back up the deployment configuration and independent secret values for `ACCESS_TOKEN_PEPPER`, `MANAGE_SESSION_SECRET`, `OIDC_COOKIE_SECRET`, OIDC client credentials, storage credentials, and any bootstrap `ADMIN_TOKEN`. Losing or changing a secret invalidates the corresponding stored token hashes or browser sessions.

## Backup

1. Pause module publishing and deletion, or take storage/database snapshots that represent the same point in time.
2. Back up PostgreSQL with the platform snapshot mechanism or `pg_dump --format=custom`. For SQLite, stop the only writer and copy the database file, or use SQLite's online backup API.
   The database backup includes the durable `artifact_deletions` outbox. Pending
   object cleanup therefore resumes automatically after restore and startup.
3. Snapshot or version-copy the complete configured bucket. At minimum, include both the local-release namespace below `ARTIFACT_PREFIX` and the independent `upstream-cache/` namespace referenced by indexed upstream releases.
4. Export the effective non-secret configuration and store the required secrets in the organization's secret backup system.
5. Record the application version and verify that both database and object backups completed.

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

6. Resolve missing or corrupt artifacts from backup. Do not delete their SQL metadata automatically.
7. To remove only objects that have no SQL release record, run the explicit repair mode:

   ```bash
   puppet-forge --reconcile-artifacts --reconcile-repair
   ```

   Repair mode deletes reported orphan objects only. Before each deletion it takes the corresponding module lock, rechecks current SQL references, and skips objects created or changed after the scan started, so concurrent publishing cannot turn a stale report into deletion of a live artifact. It never deletes release metadata and never attempts to replace corrupt artifacts.
8. Repeat report mode until it is clean, then start one replica, verify `/readyz`, downloads, OIDC/token login, and publish/delete behavior before restoring normal replica count.

## Disaster-Recovery Test

Test the complete procedure periodically in an isolated environment. A backup is not considered valid until the database, artifacts, secrets, reconciliation, and representative module downloads have all been restored successfully.
