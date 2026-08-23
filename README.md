# rdsh

[![CI](https://github.com/178inaba/rdsh/actions/workflows/ci.yml/badge.svg)](https://github.com/178inaba/rdsh/actions/workflows/ci.yml)

A CLI that runs ad-hoc SQL on [Redash](https://redash.io/) and manages saved queries there — one command per round trip, designed so AI coding agents can call it from a shell.

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
rdsh run "SELECT 1"

# From stdin or a file, JSON output, explicit data source
echo "SELECT id FROM users LIMIT 10" | rdsh run --format json --data-source warehouse
rdsh run -f query.sql
```

Run `rdsh --help` for the full reference.

### Saved queries

Save SQL as a Redash query and get a URL to share. Run it first: Redash attaches the latest result of the same SQL and data source to the query as it is created, so the URL opens with data on it instead of needing a second execution.

```sh
rdsh run -f signups.sql
rdsh query create --name "Weekly signups" -f signups.sql
```

`query create` prints the URL and nothing else; the trailing segment is the query ID, which is what a Query Results data source needs for `query_<id>` references. New queries are published — visible in everyone's query list — unless `--draft` is passed:

```sh
rdsh query create --name "signups (part)" --draft -f part.sql
```

### Profiles

```sh
rdsh profile list
rdsh profile use staging
rdsh data-source list
```

### Environment variables

```sh
RDSH_URL=https://redash.example.com RDSH_API_KEY=... rdsh run "SELECT 1" --data-source 3
```

### Timeouts

`rdsh run` and `rdsh query create` both take `--timeout`, defaulting to 90s; exceeding it exits with code 124. For `rdsh run` that also cancels the server-side job.

```sh
rdsh run -f heavy.sql --timeout 30m
```

One case exits 1 rather than 124: a `query create` that saved the query but could not publish it, including when the timeout is what stopped the publish. The query exists as a draft, so stderr carries its URL — re-running the command would save a second query.

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
