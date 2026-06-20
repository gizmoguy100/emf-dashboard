#!/bin/sh
set -eu

config_file="${APP_CONFIG_FILE:-/app/config/config.yaml}"

strip_yaml_scalar() {
	value=$1
	value=${value%%#*}
	value=$(printf '%s' "$value" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
	case "$value" in
		\"*\") value=${value#\"}; value=${value%\"} ;;
		\'*\') value=${value#\'}; value=${value%\'} ;;
	esac
	printf '%s' "$value"
}

domain_value() {
	key=$1
	awk -v wanted="$key" '
		/^domain:[[:space:]]*$/ { in_domain=1; next }
		in_domain && /^[^[:space:]]/ { in_domain=0 }
		in_domain {
			line=$0
			sub(/^[[:space:]]+/, "", line)
			if (line ~ "^" wanted ":[[:space:]]*") {
				sub("^[^:]+:[[:space:]]*", "", line)
				print line
				exit
			}
		}
	' "$config_file"
}

cloudflare_value() {
	key=$1
	awk -v wanted="$key" '
		/^domain:[[:space:]]*$/ { in_domain=1; next }
		in_domain && /^[^[:space:]]/ { in_domain=0; in_cloudflare=0 }
		in_domain && /^[[:space:]]+cloudflare:[[:space:]]*$/ { in_cloudflare=1; next }
		in_cloudflare && /^[[:space:]]{2}[^[:space:]]/ { in_cloudflare=0 }
		in_cloudflare {
			line=$0
			sub(/^[[:space:]]+/, "", line)
			if (line ~ "^" wanted ":[[:space:]]*") {
				sub("^[^:]+:[[:space:]]*", "", line)
				print line
				exit
			}
		}
	' "$config_file"
}

if [ ! -r "$config_file" ]; then
	echo "Caddy config bootstrap failed: cannot read $config_file" >&2
	exit 1
fi

if [ -z "${PUBLIC_HOSTNAME:-}" ]; then
	PUBLIC_HOSTNAME=$(strip_yaml_scalar "$(domain_value hostname)")
fi
if [ -z "${PUBLIC_HOSTNAME:-}" ]; then
	PUBLIC_HOSTNAME=$(strip_yaml_scalar "$(domain_value name)")
fi
if [ -z "${PUBLIC_HOSTNAME:-}" ]; then
	echo "Caddy config bootstrap failed: set domain.hostname in $config_file" >&2
	exit 1
fi
export PUBLIC_HOSTNAME

if [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
	CLOUDFLARE_API_TOKEN=$(strip_yaml_scalar "$(cloudflare_value api_token)")
fi
if [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
	echo "Caddy config bootstrap failed: set domain.cloudflare.api_token in $config_file" >&2
	exit 1
fi
export CLOUDFLARE_API_TOKEN

exec "$@"
