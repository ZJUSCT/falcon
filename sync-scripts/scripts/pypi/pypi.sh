#!/usr/bin/env bash

########
#
# pypi bandersnatch wrapper
#
########

set -e
set -u
set -E
set -o pipefail

echo "rsync job started for pypi"

bandersnatch mirror > $MIRRORGO_LOGS_PATH/pypi.log 2>&1

echo "rsync finished"
