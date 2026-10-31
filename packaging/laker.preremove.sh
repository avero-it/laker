#!/bin/sh
# Stops the service on removal. Configuration, state and the lake are never deleted.
set -e
name=laker
# $1 is "remove" on removal and "upgrade" on upgrade (deb); only stop for a real removal
if [ "$1" = "remove" ] && [ -d /run/systemd/system ]; then
  systemctl disable --now "$name" || true
fi
