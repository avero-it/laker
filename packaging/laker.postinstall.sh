#!/bin/sh
# Runs after install and upgrade. Creates the service user and directories; never starts the
# service (it needs a configuration first) and never touches existing data.
set -e
name=laker
if ! getent passwd "$name" >/dev/null; then
  useradd --system --user-group --home-dir "/var/lib/$name" --no-create-home \
    --shell /usr/sbin/nologin "$name"
fi
install -d -o "$name" -g "$name" -m 0750 "/var/lib/$name"
install -d -o root -g "$name" -m 0750 "/etc/$name"
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  # an upgrade of a running service restarts it onto the new binary
  if systemctl is-active --quiet "$name"; then systemctl restart "$name" || true; fi
fi
echo "$name installed. Next: copy /usr/share/doc/$name/config.example.yaml to /etc/$name/config.yaml,"
echo "edit it, then: sudo systemctl enable --now $name"
