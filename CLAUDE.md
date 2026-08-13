# CLAUDE.md

## Agent contract

rdsh's primary consumer is an AI coding agent calling it from a shell; the conventions below exist to keep that contract stable.

- **Three-way documentation sync** — user-facing CLI behaviour is stated in `README.md`, `skills/rdsh/SKILL.md`, and the cobra help strings under `internal/cmd/`. Changing the CLI means updating all three in the same PR. Overlaps today: the 90 s default `--timeout`, the exit codes, and the default output format.
- **Output format and exit codes are a contract** — stdout format (CSV/TSV/JSON) and the exit codes produced by `exitCode` in `internal/cmd/root.go` (`0`, `124` on timeout following the GNU `timeout` convention, `1` for everything else) are branched on mechanically by agent consumers. Changing either is a breaking change, held to a higher bar than for a human-facing CLI.
- **`skills/rdsh/SKILL.md` is router-style** — activation conditions and workflow only; per-command syntax stays delegated to `rdsh --help`. This was settled in 178inaba/rdsh#2, and per-command detail has already drifted back in once.

## Code conventions

Shared across the sibling CLIs (cflio, rdsh, slio):

- cobra commands are wired constructor-style (`newXCmd()` returning `*cobra.Command`); no package-level command or flag variables.
- Output formatting lives in `internal/format`. An API-client package is named after the service, and its main file after the package.
