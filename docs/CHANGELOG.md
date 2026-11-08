# Changelog

[Українська версія](CHANGELOG.uk.md)

## Unreleased

### Fixed

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
- Persist catalog completion atomically with versions in schema 10; use a
  cross-replica, transaction-fenced fetch lease and recheck lifecycle at commit.


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
