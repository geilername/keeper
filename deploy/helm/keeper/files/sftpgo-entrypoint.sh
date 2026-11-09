#!/bin/sh
# Renders initial-data.json with the camera password from the environment,
# then starts SFTPGo, whose memory data provider loads it
# (SFTPGO_DATA_PROVIDER__NAME in sftpgo.env).
# SFTPGo hashes the plain-text password on load; it is never written to the
# repo or to a volume.
set -eu

pw="${CAMERA_FTP_PASSWORD:-camera}"
case "$pw" in
*"
"*) echo "CAMERA_FTP_PASSWORD must be a single line" >&2; exit 1 ;;
esac
# Escape for a JSON string (\ and "), then for the sed replacement (\ & |).
pw=$(printf '%s' "$pw" | sed -e 's/[\\"]/\\&/g' -e 's/[\\&|]/\\&/g')

umask 077
sed "s|__CAMERA_FTP_PASSWORD__|$pw|" /etc/keeper/initial-data.json >"$SFTPGO_DATA_PROVIDER__NAME"

exec sftpgo serve "$@"
