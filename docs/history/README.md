# Historical investigations

These notes retain evidence and reasoning from earlier source snapshots.
They can describe defects that are already fixed, rejected experiments,
superseded defaults, private local artifact paths, and proposals that were
never implemented. Use the [current guides](../README.md) for setup and code
behavior. A past performance result is not a promise for another machine.

## Audits and fixes

- [23 October audit](AUDIT_2026-10-23.md) and its
  [frozen regression fixtures](../../tests/audit/20261023/README.md).
- [24 October fixes](FIXES_2026-10-24.md).
- [Reply-burst fix](BURST_FIX_2026-10-24.md).

## Measurements and research

- [Uplink research](UPLINK_RESEARCH.md) and [test results](UPLINK_TEST_RESULTS.md).
- [Profiling results](PROFILING_RESULTS.md).
- [Download target investigation](DOWNLOAD_400_CHECK.md).
- [Upload profile comparison](UPLOAD_PROFILE_CHECK.md).
- [Flaky-download investigation](FLAKY_DOWNLOAD_CHECK.md).
- [Multipath design proposal](phase-multipath.md), not an implemented feature.
- [Previous turbo guide](TURBO_README.md), retained as historical context;
  command defaults and old path references can be obsolete.

Deployment-specific troubleshooting and gateway setup are maintained outside
this source repository. The ordinary package regression tests are the current
executable checks; old prose does not override them.
