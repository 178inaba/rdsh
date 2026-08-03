# rdsh

A CLI that runs ad-hoc SQL on [Redash](https://redash.io/) and prints the result — one command per round trip, designed so AI coding agents can call it from a shell.

## Install

```sh
go install github.com/178inaba/rdsh@latest
```

## Quick start

```sh
# One-time interactive setup (the key is verified before saving)
rdsh auth login

# Run a query (CSV by default)
rdsh query "SELECT 1"

# From stdin or a file, JSON output, explicit data source
echo "SELECT id FROM users LIMIT 10" | rdsh query --format json --data-source warehouse
rdsh query -f query.sql

# Long-running queries: default timeout is 90s and exits with code 124,
# cancelling the server-side job
rdsh query -f heavy.sql --timeout 30m

# Profiles and environment variables
rdsh profile list
rdsh profile use staging
rdsh data-source list
RDSH_URL=https://redash.example.com RDSH_API_KEY=... rdsh query "SELECT 1" --data-source 3
```

Run `rdsh --help` for the full reference.

## Agent Skill (Claude Code)

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
