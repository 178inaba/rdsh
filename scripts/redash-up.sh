#!/usr/bin/env bash
# Brings up the local Redash sandbox defined in compose.yaml and prints the two
# lines that point rdsh at it:
#
#     eval "$(scripts/redash-up.sh)"
#
# Running it again against a ready stack changes nothing and prints the same
# two lines. --reset throws the Redash containers and their database away
# first, which is how a different Redash version is switched to.
set -euo pipefail

# The fixed values of this sandbox. Nothing outside this script and the
# README's Development section needs to know them, except the data source
# name: the e2e suite passes it to the commands that take --data-source,
# since the environment pair below leaves no profile to carry a default.
readonly redash_url='http://localhost:15000'
readonly api_key='rdsh-sandbox-api-key' # users.api_key is a String(40)
readonly admin_email='admin@example.com'
readonly admin_name='admin'
readonly admin_password='sandbox'
readonly data_source='sandbox'
readonly seed_db='testdata'
readonly pg_volume='rdsh-redash-postgres'

usage() {
	cat <<'USAGE'
Usage: scripts/redash-up.sh [--reset]

Starts the local Redash sandbox and prints the export lines that point rdsh at
it, so that `eval "$(scripts/redash-up.sh)"` configures the current shell.

  --reset  remove the Redash containers and database before starting, which is
           what switching the redash/redash tag in compose.yaml needs
USAGE
}

compose() {
	docker compose --profile redash "$@"
}

# Redash's own tables live in the `postgres` database; the seed data is in a
# separate one. The image's default pg_hba trusts local socket connections, so
# no password is needed here.
psql_q() {
	compose exec -T postgres psql -U postgres -d postgres -tAc "$1"
}

# Runs one step of the setup unless its query already returns a row. Every step
# is guarded this way, which is what makes a second run a no-op — and it has to
# be a check rather than a tolerated failure, because `manage users create_root`
# exits 1 when the user is already there.
setup_step() {
	local query=$1 message=$2 done_already
	shift 2
	done_already=$(psql_q "$query")
	if [[ -z $done_already ]]; then
		echo "$message"
		"$@"
	fi
}

register_data_source() {
	# Read the password back rather than repeating it, so that compose.yaml
	# stays the one place the sandbox database's password is written.
	local password
	password=$(compose exec -T postgres printenv POSTGRES_PASSWORD)
	# The image's entrypoint passes `manage` arguments on through an unquoted
	# $*, so a space anywhere in the JSON would split it into arguments Redash
	# then rejects.
	compose run --rm -T server manage ds new "$data_source" --type pg \
		--options "{\"host\":\"postgres\",\"port\":5432,\"user\":\"postgres\",\"password\":\"${password}\",\"dbname\":\"${seed_db}\"}"
}

wait_for_server() {
	local deadline=$((SECONDS + 300))
	until curl -fs --max-time 5 -o /dev/null "${redash_url}/ping"; do
		if ((SECONDS > deadline)); then
			echo "redash-up.sh: ${redash_url}/ping did not answer within 5 minutes" >&2
			return 1
		fi
		sleep 2
	done
}

main() {
	local reset=no
	case "${1-}" in
	--reset) reset=yes ;;
	'') ;;
	*)
		echo "redash-up.sh: unknown argument: $1" >&2
		usage
		return 2
		;;
	esac

	if [[ $reset == yes ]]; then
		echo '==> Removing the Redash containers and database'
		# No -v: that would take the lint caches with it. The Redash volume
		# has a fixed name in compose.yaml so it can be named on its own.
		compose down
		docker volume rm -f "$pg_volume"
	fi

	echo '==> Starting PostgreSQL and Redis'
	compose up -d --wait postgres redis

	# The schema has to exist before the server starts, as it does in the
	# official setup script.
	setup_step "SELECT to_regclass('users')" '==> Creating the Redash schema' \
		compose run --rm -T server create_db

	# Naming the services matters: a bare `up` would also start lint, which
	# has no profile and is therefore always enabled.
	echo '==> Starting Redash'
	compose up -d --wait postgres redis server scheduler worker
	echo "==> Waiting for ${redash_url}/ping"
	wait_for_server

	setup_step "SELECT 1 FROM users WHERE email = '${admin_email}'" \
		'==> Creating the admin user' \
		compose run --rm -T server manage users create_root \
		"$admin_email" "$admin_name" --password "$admin_password"

	# Redash generates a random API key; this replaces it with the fixed one
	# the export line below carries. Running it every time also repairs a key
	# that was regenerated in the UI.
	echo '==> Setting the admin API key'
	psql_q "UPDATE users SET api_key = '${api_key}' WHERE email = '${admin_email}'"

	setup_step "SELECT 1 FROM data_sources WHERE name = '${data_source}'" \
		"==> Registering the ${data_source} data source" register_data_source
}

case "${1-}" in
-h | --help)
	usage
	exit 0
	;;
esac

# Everything above writes to stderr: `docker compose run` forwards the
# container's own output to this script's stdout, which has to carry the two
# export lines and nothing else for `eval` to be safe.
main "$@" >&2

printf 'export RDSH_URL=%s\n' "$redash_url"
printf 'export RDSH_API_KEY=%s\n' "$api_key"
