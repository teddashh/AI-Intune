# disk-clean

`disk-clean` is the allowlist-only housekeeping script the agent runs.
The copy under `internal/maintenance/script/disk-clean` is embedded in the
agent binary and must stay byte-for-byte identical to this file.

The script is bash and parse-only. It never sources its conf. Version 1.1.0
keeps the 1.0.0 categories and adds three guards:

- `BUILTIN_PROTECT` is hard-coded. It is merged with `PROTECT_NAMES` on every
  run. A conf file can add names. It cannot remove these.
- The same names are excluded from the root tmpfiles estimate.
- A `TMP_GLOB_RULE` whose basename glob would match a protected name is
  refused and recorded as `refused:` in the summary.

The Hub renders the conf from a closed profile. The profile has no field that
replaces the protected list. See `docs/MAINTENANCE-DISK-CLEAN.md`.

User scope does not need root. Root scope exits 2 unless the process is
uid 0. The agent does not use sudo.

The script does not reboot, restart services, install packages, or run
`docker system prune`. The docker category removes dangling images and old
build cache only.
