#!/bin/sh
# Shoka deb prerm — stop + disable the service before removal/upgrade-teardown.
# Touches the service only; never the data dir.
set -e

case "$1" in
  remove|deconfigure)
    if [ -d /run/systemd/system ]; then
      systemctl stop shoka.service >/dev/null 2>&1 || true
      systemctl disable shoka.service >/dev/null 2>&1 || true
    fi
    ;;
esac

exit 0
