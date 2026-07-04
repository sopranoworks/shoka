#!/bin/sh
# Shoka deb postinst — create the service account + state dir, register the unit.
# Conservative: never force-starts the service (prints an enable hint instead).
set -e

case "$1" in
  configure)
    # System group + non-login user owning /var/lib/shoka. Idempotent.
    if ! getent group shoka >/dev/null 2>&1; then
      addgroup --system shoka
    fi
    if ! getent passwd shoka >/dev/null 2>&1; then
      adduser --system --ingroup shoka --home /var/lib/shoka \
        --no-create-home --shell /usr/sbin/nologin shoka
    fi

    # The dir is shipped (0750) but own it to the service account here so an
    # existing dir from a prior install is corrected too.
    if [ -d /var/lib/shoka ]; then
      chown -R shoka:shoka /var/lib/shoka || true
      chmod 0750 /var/lib/shoka || true
    fi

    # Config may contain bearer tokens, webhook secrets, TOTP encryption
    # keys — restrict to root (write) + service account (read).
    if [ -f /etc/shoka/shoka.yaml ]; then
      chown root:shoka /etc/shoka/shoka.yaml || true
      chmod 0640 /etc/shoka/shoka.yaml || true
    fi

    if [ -d /run/systemd/system ]; then
      systemctl daemon-reload || true
    fi

    echo "shoka: installed. Edit /etc/shoka/shoka.yaml, then enable + start with:"
    echo "         sudo systemctl enable --now shoka"
    ;;
esac

exit 0
