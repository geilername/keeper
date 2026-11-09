#!/bin/sh
# Rebuilds demo/sets/<name>/ from the CC0 sources: fetch (checksummed), then
# generate.py, both inside the pinned generator container. Files are written
# as the calling user.
set -eu
cd "$(dirname "$0")"

docker build -q -t keeper-demo-gen . >/dev/null
docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:/demo" keeper-demo-gen \
	sh -c './fetch.sh && python -I generate.py'
