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

## Agent Skill

The repository ships an [Agent Skill](skills/rdsh/SKILL.md) that teaches AI coding agents how to use rdsh. Install it by copying the directory into your skills directory:

```sh
git clone https://github.com/178inaba/rdsh.git
mkdir -p ~/.claude/skills
cp -R rdsh/skills/rdsh ~/.claude/skills/
```

Copying works even if you delete the clone afterwards. If you keep a clone and want the skill to track updates, symlink it instead:

```sh
mkdir -p ~/.claude/skills
ln -s "$(pwd)/rdsh/skills/rdsh" ~/.claude/skills/rdsh
```

## Development

```sh
go test -race ./...

# Lint runs in Docker so the version matches CI — see compose.yaml
docker compose run --rm lint

# Let golangci-lint apply the fixes it can make itself
docker compose run --rm lint --fix
```
