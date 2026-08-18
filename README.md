# rdsh

[![CI](https://github.com/178inaba/rdsh/actions/workflows/ci.yml/badge.svg)](https://github.com/178inaba/rdsh/actions/workflows/ci.yml)

A CLI that runs ad-hoc SQL on [Redash](https://redash.io/) and prints the result — one command per round trip, designed so AI coding agents can call it from a shell.

## Install

```sh
go install github.com/178inaba/rdsh@latest
```

## Setup

One-time interactive setup — the key is verified before saving:

```sh
rdsh auth login
```

## Usage

```sh
# Run a query (CSV by default; also --format tsv or json)
rdsh query "SELECT 1"

# From stdin or a file, JSON output, explicit data source
echo "SELECT id FROM users LIMIT 10" | rdsh query --format json --data-source warehouse
rdsh query -f query.sql
```

Run `rdsh --help` for the full reference.

### Profiles

```sh
rdsh profile list
rdsh profile use staging
rdsh data-source list
```

### Environment variables

```sh
RDSH_URL=https://redash.example.com RDSH_API_KEY=... rdsh query "SELECT 1" --data-source 3
```

### Timeouts

The default timeout is 90s; exceeding it exits with code 124, cancelling the server-side job.

```sh
rdsh query -f heavy.sql --timeout 30m
```

### Interrupts

Ctrl-C and `SIGTERM` cancel the in-flight query rather than leaving it to run to completion. At an `rdsh auth login` prompt they end the command straight away, without writing or changing a profile.

An interrupted run is not reported as a failure: rdsh prints nothing and terminates by the signal itself, so a shell sees the same thing it sees from `curl` or `git` and a loop over `rdsh` invocations stops. Interrupt a second time to skip waiting for the server-side job to be cancelled.

## Agent Skill

The repository ships an [Agent Skill](skills/rdsh/SKILL.md) that teaches AI coding agents how to use rdsh. In Claude Code, install it as a plugin:

```sh
claude plugin marketplace add 178inaba/rdsh
claude plugin install rdsh@rdsh
```

For other agents, use a skill installer that consumes GitHub repos directly, e.g. [`npx skills`](https://www.npmjs.com/package/skills):

```sh
npx skills add 178inaba/rdsh
```

## Development

```sh
go test -race ./...

# Lint runs in Docker so the version matches CI — see compose.yaml
docker compose run --rm lint

# Let golangci-lint apply the fixes it can make itself
docker compose run --rm lint --fix
```
