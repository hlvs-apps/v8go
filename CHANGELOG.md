# Changelog

## [0.1.0.0] - 2026-09-16

### Added

- Create and restore trusted startup snapshots with private captured values, fresh runtime callbacks, and independent application contexts.
- Validate snapshot integrity and native-build compatibility before loading, and expose startup and code-cache compatibility identities.
- Validate captured functions during preparation, settling their queued microtasks before continuing.

### Fixed

- Reject empty compiler caches and safely handle unavailable generated caches.
- Keep snapshot and ordinary isolates safe when used together, with explicit native blob ownership and serialized snapshot creation.
- Isolates restored from the same snapshot share one native copy of it instead of one copy each, so a pool of N isolates no longer holds N copies.
- Refuse snapshots on platforms without a native archive identity instead of weakening the compatibility check.

### Changed

- Exercise snapshots in native Linux and macOS architecture CI lanes and the Windows amd64 suite, with generated native-archive identity checks.
