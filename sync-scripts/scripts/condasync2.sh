#!/usr/bin/env bash

# conda sync script

set -x

sync_channel () {
  mkdir -p "/srv/mirrors/storage/$1/$2/" || true
  /condasync \
    -upstream "$CONDASYNC_UPSTREAM" \
    -channel "$1" \
    -arch "$2" \
    -to "/srv/mirrors/storage/" > $MIRRORGO_LOGS_PATH/condasync.$1.$2.log.stdout 2> $MIRRORGO_LOGS_PATH/condasync.$1.$2.log.stderr
}

sync_channel "nvidia" "linux-64"
sync_channel "nvidia" "win-64"
sync_channel "nvidia" "linux-aarch64"

# use channel list from linux-64 for noarch
cp /srv/mirrors/storage/nvidia/labels-linux-64.txt /srv/mirrors/storage/nvidia/labels-noarch.overrides.txt

sync_channel "nvidia" "noarch"

