# Changelog

[Українська версія](CHANGELOG.uk.md)

## Unreleased

### Fixed

- Concurrent protected requests wait up to five seconds for an in-progress
  access-config refresh when no usable snapshot exists, instead of immediately
  returning 503. Usable snapshots remain non-blocking; canceled or timed-out
  waits fail closed.
