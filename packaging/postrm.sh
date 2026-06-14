#!/bin/sh
# Shoka deb postrm. Data at /var/lib/shoka and the service account are removed
# ONLY on `purge` — never on `remove`/`upgrade`, so an upgrade keeps all repos.
set -e

case "$1" in
  purge)
    rm -rf /var/lib/shoka
    if getent passwd shoka >/dev/null 2>&1; then
      deluser --system shoka >/dev/null 2>&1 || true
    fi
    if getent group shoka >/dev/null 2>&1; then
      delgroup --system shoka >/dev/null 2>&1 || true
    fi
    if [ -d /run/systemd/system ]; then
      systemctl daemon-reload || true
    fi
    ;;
  remove|upgrade|failed-upgrade|abort-install|abort-upgrade|disappear)
    # Intentionally preserve /var/lib/shoka (the repositories) and the account.
    ;;
esac

exit 0
