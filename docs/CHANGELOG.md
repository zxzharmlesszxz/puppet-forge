# Changelog

[Українська версія](CHANGELOG.uk.md)

## Unreleased

## v0.9.1

### Fixed

- Suppress stalled-refresh alerts for disabled workers, derive Helm target
  selectors from the metrics Service name, and discover arbitrary Forge job
  names in Grafana. Clarify legacy-version and refresh-age panel semantics.
- Forward `PUBLISH_ALLOWED_CIDRS` through Compose, including its configuration
  example and regression coverage.
- Require the OpenVox installer gate in CI as well as local preflight.
- Synchronize deployment, architecture, and runtime diagrams with the current
  access and deletion model.
- Expose bounded lifecycle inventory, approval age, administrative outcomes,
  authorization/rate-limit gates, audit persistence, and access-config refresh.
  Update standalone/Helm alerts and Grafana; use current inventory gauges rather
  than historical samples that could retain removed resources or consumer labels.
- Update SeaweedFS to 4.53 in Compose and S3 integration jobs; refresh the
  Alpine 3.24.4 runtime image digest.
- Require a non-blank root README for new local publications and replacements
  through both APIs and Manage, before storage or catalog writes. Preserve
  existing releases and upstream behavior; cover validation and all upload paths.
- Add a global-admin-only upstream archive cache cleanup action with CSRF,
  transactional audit, durable retry, and shared-object reference protection.
  Keep release/catalog/moderation data and all local archives intact.
- Include the standard OpenVox 8 installer test in release preflight and release
  push; use the digest-pinned 8.31.0 agent image by default, with an explicit override.
- Use one team access form for creation and editing, with groups on the left,
  email addresses on the right, and separate reader/administrator sections.
  Mobile layouts retain the same field order in one column; access rules and
  verified-email requirements are unchanged.
- Show the executable version and project authorship in the shared public and
  management footer. Release builds use the same version as the CLI and metrics;
  unversioned development processes display `dev`.

## v0.9.0

### Fixed

- Delete now removes data without a permanent ban for both sources: upstream
  returns on demand; local requires republication and approval. Schema 11 removes
  legacy Deleted/tombstone bans, preserves explicit blocks and audit history,
  invalidates catalog completion on deletion, and records minimal source
  provenance to prevent silent upstream replacement of deleted local modules.
  Blocked resources remain blocked through deletion and republication; an
  administrator can explicitly unblock them even when their data is absent.
- Accept official Forge metadata namespace discrepancies backed by an explicit
  matching release slug and matching module name/version; normalize only the API
  metadata name, preserving archive checksums and other metadata fields. Cover
  apt 0.0.1, 0.0.2, and 10.0.0, old-cache repair, and collection/single responses
  without manual exclusions or changes to local publication validation.
- Concurrent protected requests wait up to five seconds for an in-progress
  access-config refresh when no usable snapshot exists, instead of immediately
  returning 503. Usable snapshots remain non-blocking; canceled or timed-out
  waits fail closed.
- Index unknown upstream modules on the first exact V3 release-collection query,
  independently of version filters, offsets, or an empty readable result.
- Persist catalog completion atomically with versions in schema 11; use a
  cross-replica, transaction-fenced fetch lease and recheck lifecycle at commit.
- Preserve local-only provenance and explicit blocks. Delete invalidates catalog
  completion so upstream data can return on demand. Failed imports remain
  retryable; 404 stays an empty collection.

- Preserve and validate full upstream metadata in V3 collections and individual
  releases, including dependencies, requirements, and additional fields.
- Repair legacy SQL cache records even with a populated file URI; serialize
  repairs across replicas without overwriting concurrent artifact changes.
- Return controlled errors for missing or invalid required metadata instead of
  incomplete successes or raw upstream fallbacks.
- Return origin-relative V3 archive and pagination URIs, retaining filters and
  nulls for absent pages, for standard OpenVox 8 installer compatibility.

### Tests

- Cover first-query indexing, two replicas, failures/recovery, lifecycle races,
  and atomic catalog rollback on PostgreSQL and SQLite. The standard OpenVox
  fixture now starts without upstream modules or cached upstream archives.

- Add metadata, cache, pagination, upstream-failure, and concurrency regressions.
- Add an opt-in standard OpenVox 8 Docker installer test and a live authenticated
  smoke target. Real private Common and full sshd resource coverage remain
  environment-specific acceptance checks, not asserted by unexecuted tests.
- Periodic 503 investigation remains separate; no root cause is claimed.
