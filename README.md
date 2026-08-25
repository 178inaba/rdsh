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

Save SQL as a Redash query and get a URL to share. The page behind that URL opens with data on it only if a result is linked to the query, and which call links one depends on what you already have:

```sh
rdsh query create --name "Weekly signups" --refresh -f signups.sql   # not run yet
```

`--refresh` executes the saved query once, after saving it, which is what links a result on every Redash version. It costs exactly one execution.

```sh
rdsh run -f signups.sql                                              # already run
rdsh query create --name "Weekly signups" -f signups.sql
```

Redash 26.3.0 and later link the result of a matching run to the query as it is saved, so this way costs nothing beyond the run you had already done. Older versions link a result only when the saved query is executed, and the query then opens empty. rdsh reads no version to tell the two apart: when the new query comes back with no result on its page, a line naming `rdsh query refresh <id>` goes to stderr and the command still exits 0 — so read stderr before reporting the URL as shared.

`query create` prints the URL and nothing else on stdout; the trailing segment is the query ID, which is what a Query Results data source needs for `query_<id>` references. New queries are published — visible in everyone's query list — unless `--draft` is passed:

```sh
rdsh query create --name "signups (part)" --draft -f part.sql
```

Editing a saved query keeps the same URL. The query is named by its ID or by its URL, and any combination of new SQL, `--name`, `--description`, and `--publish`/`--draft` can be changed in one call:

```sh
rdsh query update 42 -f signups.sql
rdsh query update https://redash.example.com/queries/42 --name "Weekly signups (EU)" --publish
```

A parametrized query needs its parameters defined, on either command, or the
query page stays empty for everyone else: Redash matches a result to a query
by the hash of its SQL with the stored defaults substituted, so a parameter
without a default can never link one. `--param-default name=value` declares
the parameter and sets its default, `--param-type name=type` gives it a type
(`text` unless said otherwise; the rest are `text-pattern`, `number`, `date`,
`datetime-local` and `datetime-with-seconds`), and `--param-regex
name=pattern` gives a `text-pattern` parameter the pattern its value must
match in full, implying that type on its own. All three are repeatable and
split at the first `=`, so a value or a pattern may contain one:

```sh
rdsh query create --name "Signups by team" --param-default days=7 \
  --param-type days=number --param-default team=core -f signups.sql
rdsh query update 42 --param-default days=30
```

On `update` this is an edit rather than a rewrite: only the keys the flags
carry are overwritten, so a title set in the Redash UI, a type no flag
mentions, and the definitions of every parameter not named all survive.
Parameters Redash offers that rdsh cannot express — ranges, enums and
dropdown queries — are refused rather than rewritten; define those in the
Redash UI.

Every parameter the SQL uses has to be covered whenever `create` runs, or
`update` is given new SQL or any `--param-*` flag; a metadata-only edit such
as a rename skips the check, so a query whose parameters have no defaults can
still be renamed. Defining parameters is also a one-way move: once a query
carries definitions, executing it with a name that is not among them is
refused.

To find a saved query in the first place — its ID, or whether it is still a draft — list them. Without an argument the queries you can see come back newest first; with one, it is a full-text search the server answers in its own relevance order:

```sh
rdsh query list
rdsh query list signups --format tsv
rdsh query list --mine --limit 100
```

The columns are `id`, `name`, `is_draft`, and `url`. Only the first 30 rows are printed unless `--limit` says otherwise; when the server holds more, a note saying so goes to stderr and the command still exits 0, so stdout stays parseable as the listing alone.

Reading one back prints its SQL and nothing else, so a query a colleague shared can be run for a result of your own without opening Redash:

```sh
rdsh query show 42 > signups.sql
rdsh run -f signups.sql
```

`rdsh query show` takes the same ID or URL as `update`. Its `--format` is the one exception to the shared output formats: `json` is the only value it accepts — a multi-line SQL body does not fit a row format — and it prints one object with `id`, `name`, `description`, `data_source_id`, `is_draft`, `url`, `query`, and `visualizations` — the charts on the query page, each as `id`, `type`, `name` and its stored `options`. That array is the only way to find a visualization's ID, which `rdsh visualization update` and `delete` take. Without `--format`, the SQL itself is what comes out.

Reading the SQL back and running it returns a result of your own; it does not touch the one Redash shows everyone on the query page. That shared result — and any chart drawn from it — only moves when the saved query is executed, which is what `rdsh query refresh` does. It prints the rows in the same formats as `rdsh run` and refreshes the shared result in the same call:

```sh
rdsh query refresh 42
rdsh query refresh https://redash.example.com/queries/42 --format json
```

Parameters are supplied with `--param name=value`, repeatable; everything after the first `=` is the value. A parameter left out is executed with the default stored on the query, and one with neither a stored default nor a `--param` fails the command, naming itself — Redash substitutes only what the request gives it.

```sh
rdsh query refresh 42 --param days=30 --param team=core
```

The shared result only advances when the query runs with its own stored defaults, because Redash matches an execution to a query by the hash of the text it ran. Override a default with `--param`, or cover a parameter that has no stored default, and the execution still succeeds and still prints the rows, but the query page keeps showing what it showed before; a line saying so goes to stderr and the command still exits 0. To make the overridden values the ones everyone sees, change the defaults themselves with `rdsh query update --param-default` and refresh again.

Unlike `rdsh run` and `rdsh query create`, `rdsh query update` never reads stdin — SQL is optional there, so falling back to it would turn whatever was piped in into the query. The update also carries the version the query had when it was read, so an edit made in the Redash UI in the meantime fails the command instead of being overwritten; read the query again and re-run.

### Visualizations

A saved query hands someone rows; a visualization on it hands them a chart. It lives on the query page, so the URL already shared keeps working and the chart is simply on it.

```sh
rdsh query refresh 42
rdsh visualization create --query 42 --name "Signups by day" --type line --x day --y count
```

`visualization` is also reachable as `viz`. `--type` takes `line`, `bar`, `area`, `scatter` and `pie` — all five are Redash's one chart type and differ only in how the series is drawn, `bar` being the upright one. `--y` is repeatable, which is how a chart gets several series:

```sh
rdsh viz create --query 42 --name Signups --type bar --x day --y ios --y android
```

Only that one setting is written. Everything else about the chart is left to Redash's own defaults, which is what keeps an rdsh-made chart looking like a UI-made one — but three of those defaults are not implied by the type's name:

- **`area` is not stacked.** Series are drawn over each other translucently, and the y axis tops out at the largest series rather than at the sum.
- **`pie` is ordered by share**, not by the row order of the result.
- **the x axis type is auto-detected**, so a column of date-like strings such as `2026-01` becomes a date axis rather than evenly spaced categories.

Each is reachable through `--options-file` (`series.stacking`, `piesort`, `xAxis.type`), as is every other chart setting.

The `refresh` in the first example is not incidental. Redash stores a visualization's options without validating them, so a chart naming a column the result does not have is accepted with a 200 and renders as a blank chart nobody is told about. `rdsh` therefore checks `--x` and `--y` against the result the query page already holds before creating anything, and a mismatch fails the command naming the columns that do exist. A query with no result on its page yet has nothing to check against, so nothing is created and the error says to run `rdsh query refresh` first. That is the state a `query create` reports on stderr, and a query saved with `--refresh` never reaches it — so a chart on a query saved that way needs no refresh of its own.

That result is read by ID, which is what lets creating a chart be a pure metadata call: it never executes the query and never adds anything to its cache. Parameter values do not enter into it either — the same SQL yields the same columns, so the check reads the same names whatever values the page is viewed with.

Editing changes only what is passed. The stored options are read first and everything the flags do not name is sent back as it was, so settings chosen in the Redash UI survive. `--query` is required on `update` and `delete` because Redash has no endpoint that reads a visualization by ID — which also means a mistyped ID is refused rather than applied to some other query's chart:

```sh
rdsh query show 42 --format json          # the visualizations array carries the IDs
rdsh visualization update 7 --query 42 --type bar
rdsh visualization delete 7 --query 42
```

That array carries each chart's stored `options` as well as its ID, and it is the only place to read them — Redash has no endpoint that returns a single visualization. Since `--options-file` replaces the options object rather than merging into it, an edit that means to change one key should start from what `query show` reports, or the keys it does not repeat are dropped from the chart.

Because `columnMapping` is keyed by column name, passing `--x` alone moves the x column and leaves the y columns alone — and a column can hold only one role, so moving one axis onto the column the other holds is refused rather than silently dropping that other axis. Swapping them means naming both in the same call:

```sh
rdsh visualization update 7 --query 42 --x count --y day
```

For chart settings and visualization types the typed flags do not reach — counters, tables, anything configured in depth — `--options-file` passes the raw Redash options JSON through and sends `--type` verbatim as the API type. It skips the column check, so the file is taken on trust:

```sh
rdsh visualization create --query 42 --name Total --type COUNTER --options-file counter.json
```

Dashboards are out of scope; a visualization belongs to its query.

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

Every command that talks to Redash takes `--timeout`, defaulting to 90s; exceeding it exits with code 124. `--timeout 0` removes the limit, and a negative duration is refused before anything is sent. Wherever a query is executed — `rdsh run`, `rdsh query refresh`, and `rdsh query create --refresh` — the expiry also cancels the server-side job, and the one `--timeout` bounds the whole command, so a heavy query saved with `--refresh` needs a longer one.

```sh
rdsh run -f heavy.sql --timeout 30m
```

On `rdsh auth login` it bounds the verification request alone — the prompts wait for you however long you take. `rdsh profile list` and `rdsh profile use` only read and write the config file, so they take no `--timeout`.

Two cases exit 1 rather than 124, both of them a `query create` that saved the query and then failed — publishing it, or the `--refresh` execution — including when the timeout is what stopped that step. The query exists either way, so stderr carries its URL: re-running the command would save a second query.

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

### Redash sandbox

`compose.yaml` also carries a throwaway Redash, behind a `redash` profile so
that nothing above starts it. One command brings it up — creating the admin
user, a seeded PostgreSQL database and the data source that reads it — and
prints the two lines that point rdsh at it:

```sh
eval "$(scripts/redash-up.sh)"
rdsh run --data-source sandbox "SELECT count(*) FROM signups"
```

`RDSH_URL` and `RDSH_API_KEY` are all rdsh needs, so no config file is written
and `rdsh auth login` never comes into it. A default data source can only come
from a profile, though, so `--data-source sandbox` has to be passed every time.
The seed database holds `signups` (a date, a number and two text columns) and
`events` (a timestamp and a decimal amount), so a parametrized query has
something of every type to filter on.

Running the script again against a ready stack changes nothing and prints the
same two lines. The UI at http://localhost:15000 takes `admin@example.com` with
the password `sandbox`.

The e2e suite runs against that sandbox, checking the Redash-side contracts
the in-process fakes cannot be wrong about on their own. It is behind a build
tag, so `go test ./...` never reaches it, and it fails rather than skips when
the two variables are unset:

```sh
go test -tags e2e ./...
```

Tearing the sandbox down leaves the lint caches above alone:

```sh
docker compose --profile redash down
docker volume rm rdsh-redash-postgres
```

Trying another Redash release means editing the `redash/redash` tag in
compose.yaml and re-running with `--reset`, which is that same removal followed
by a fresh start:

```sh
scripts/redash-up.sh --reset
```

Redash's migrations are forward-only, so an older image cannot start on a
volume a newer one initialised — hence the reset rather than a downgrade in
place. A PostgreSQL major bump needs it too, since the data directory is not
upgraded for you.
