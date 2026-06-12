#!/bin/sh
# Entrypoint for the harness shoka image (B-63 §2.3): render the config template by
# substituting the registration posture (REGISTRATION_MODE, default cimd) into the
# __REGISTRATION_MODE__ placeholder, then exec shoka. One image therefore serves BOTH
# the cimd and dcr runs — run.sh runs the stack once per mode and passes the value here.
set -eu

MODE="${REGISTRATION_MODE:-cimd}"
case "$MODE" in
  cimd | dcr) ;;
  *)
    echo "entrypoint: REGISTRATION_MODE must be cimd|dcr, got '$MODE'" >&2
    exit 64
    ;;
esac

sed "s/__REGISTRATION_MODE__/${MODE}/" /etc/shoka/shoka.yaml.tmpl > /etc/shoka/shoka.yaml
echo "entrypoint: rendered config with registration_mode=${MODE}" >&2
exec /usr/local/bin/shoka --config /etc/shoka/shoka.yaml
