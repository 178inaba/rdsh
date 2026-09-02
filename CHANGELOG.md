# Changelog

## [v1.0.4](https://github.com/178inaba/rdsh/compare/v1.0.3...v1.0.4) - 2026-09-02

### Changes
- Pin the npx skills install command to the stable tag by @178inaba in https://github.com/178inaba/rdsh/pull/83
- Move the sandbox postgres volume to /var/lib/postgresql and bump the image to 18-alpine by @178inaba in https://github.com/178inaba/rdsh/pull/89
- Take the release body from GitHub's generated notes, with dependency updates last by @178inaba in https://github.com/178inaba/rdsh/pull/92

## [v1.0.3](https://github.com/178inaba/rdsh/compare/v1.0.2...v1.0.3) - 2026-09-01

- Enable revive and adopt the newer standard-library forms Go 1.27 offers by @178inaba in https://github.com/178inaba/rdsh/pull/79
- Port the JSON handling to encoding/json/v2 by @178inaba in https://github.com/178inaba/rdsh/pull/82

## [v1.0.2](https://github.com/178inaba/rdsh/compare/v1.0.1...v1.0.2) - 2026-08-31

- Mint the release token with client-id, not the deprecated app-id by @178inaba in https://github.com/178inaba/rdsh/pull/73
- Attest with actions/attest directly instead of the attest-build-provenance wrapper by @178inaba in https://github.com/178inaba/rdsh/pull/75
- Tie the plugin to releases with a stable tag and a tagpr-bumped version by @178inaba in https://github.com/178inaba/rdsh/pull/78

## [v1.0.1](https://github.com/178inaba/rdsh/compare/v1.0.0...v1.0.1) - 2026-08-31

- Attest the release archives with build provenance and document how to verify a download by @178inaba in https://github.com/178inaba/rdsh/pull/68

## [v0.0.1](https://github.com/178inaba/rdsh/commits/v0.0.1) - 2026-08-30

- Implement rdsh MVP: ad-hoc SQL execution CLI for Redash by @178inaba in https://github.com/178inaba/rdsh/pull/3
- Bundle a Claude Code Agent Skill and document manual installation by @178inaba in https://github.com/178inaba/rdsh/pull/4
- Add Dependabot config, a CI status badge, and CLAUDE.md, and document the tsv output format by @178inaba in https://github.com/178inaba/rdsh/pull/6
- Bump golangci/golangci-lint-action from 8 to 9 by @dependabot[bot] in https://github.com/178inaba/rdsh/pull/7
- Bump actions/checkout from 4 to 7 by @dependabot[bot] in https://github.com/178inaba/rdsh/pull/8
- Pin golangci-lint via compose.yaml and modernize the CI and Dependabot setup by @178inaba in https://github.com/178inaba/rdsh/pull/13
- Pass the root persistent flags to the constructors via a struct by @178inaba in https://github.com/178inaba/rdsh/pull/15
- Rename internal/output to internal/format and redash/client.go to redash.go by @178inaba in https://github.com/178inaba/rdsh/pull/16
- Restructure README to the shared section skeleton by @178inaba in https://github.com/178inaba/rdsh/pull/18
- Give the agent-contract bullets their own heading in CLAUDE.md by @178inaba in https://github.com/178inaba/rdsh/pull/19
- Ship the Agent Skill as a Claude Code plugin with a self-hosted marketplace by @178inaba in https://github.com/178inaba/rdsh/pull/22
- Ignore .claude/worktrees so linked worktrees do not dirty git status by @178inaba in https://github.com/178inaba/rdsh/pull/24
- Make auth login's prompts cancellable so Ctrl-C does not save the profile anyway by @178inaba in https://github.com/178inaba/rdsh/pull/25
- Die of the signal instead of reporting an interrupt as a failure by @178inaba in https://github.com/178inaba/rdsh/pull/29
- Rename rdsh query to rdsh run to free the query namespace by @178inaba in https://github.com/178inaba/rdsh/pull/36
- Add rdsh query create to save and publish queries by @178inaba in https://github.com/178inaba/rdsh/pull/37
- Add rdsh query update to edit a saved query by @178inaba in https://github.com/178inaba/rdsh/pull/38
- Add rdsh query list to find saved queries and their IDs by @178inaba in https://github.com/178inaba/rdsh/pull/39
- Add rdsh query show to read a saved query's SQL and metadata by @178inaba in https://github.com/178inaba/rdsh/pull/40
- Report a mistyped subcommand under a group command instead of exiting 0 by @178inaba in https://github.com/178inaba/rdsh/pull/41
- Bound auth login and data-source list with --timeout by @178inaba in https://github.com/178inaba/rdsh/pull/42
- Exit 124 when --timeout expires resolving a data source name by @178inaba in https://github.com/178inaba/rdsh/pull/47
- Add rdsh query refresh to execute a saved query and refresh its shared result by @178inaba in https://github.com/178inaba/rdsh/pull/48
- Let rdsh query create and update define parameter types and default values by @178inaba in https://github.com/178inaba/rdsh/pull/49
- Add a docker compose Redash sandbox with seed data for trying rdsh end to end by @178inaba in https://github.com/178inaba/rdsh/pull/55
- Add rdsh visualization to manage charts on saved queries by @178inaba in https://github.com/178inaba/rdsh/pull/50
- Write down that a command does not execute unasked by @178inaba in https://github.com/178inaba/rdsh/pull/57
- Add e2e tests against the Redash sandbox and run them in CI by @178inaba in https://github.com/178inaba/rdsh/pull/58
- Move the module to go 1.27 and the linter to a build that accepts it by @178inaba in https://github.com/178inaba/rdsh/pull/60
- Fail the lint run on unformatted code by @178inaba in https://github.com/178inaba/rdsh/pull/62
- Tell the caller when query create leaves the query page empty, and add --refresh to fill it by @178inaba in https://github.com/178inaba/rdsh/pull/61
- Release rdsh with tagpr and GoReleaser, and report the version from --version by @178inaba in https://github.com/178inaba/rdsh/pull/66
