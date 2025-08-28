#!/usr/bin/env bash

########
#
# rsync with timestamp wrapper
#
########
#
# Environment Variables:
# UPD_LOCAL: local update timestamp path
# UPD_REMOTE: remote update timestamp url
# SYNC_LOCAL: local sync timestamp path
# SYNC_REMOTE: remote sync timestamp url
# RSYNC_UPSTREAM: upstream path
# RSYNC_EXTRA_OPTS: extra sync options
#
########
#
# Comments:
#
# rsync options:
#
# -p  preserve permissions
# -r  recurse into directories
# -l  copy symlinks as symlinks
# -t  preserve modification times
# -v  increase verbosity
# -H  preserve hard links
# -S  turn sequences of nulls into sparse blocks
# -B  force a fixed checksum block-size
#
########

set -e
set -E
set -o pipefail

if [ -z "$RSYNC_UPSTREAM" ]; then
  echo "RSYNC_UPSTREAM not set"
  exit 1
fi
if [ -z "$UPD_LOCAL" ]; then
  echo "UPD_LOCAL not set"
  exit 1
fi
if [ -z "$UPD_REMOTE" ]; then
  echo "UPD_REMOTE not set"
  exit 1
fi

RSYNC_EXTRA_OPTS="${RSYNC_EXTRA_OPTS:-""}"

echo "rsync job started for $RSYNC_UPSTREAM"
exec 443>/srv/mirrors/storage/MIRROR_IS_SYNCING || exit 1
flock -n 443 || (echo "failed to acquire lock"; exit 1)
trap 'rm -f /srv/mirrors/storage/MIRROR_IS_SYNCING' EXIT

echo "checking lastupdate"
ur=$(curl -Ls "$UPD_REMOTE")
ul=$(cat "$UPD_LOCAL")
echo "remote updated at $ur ($(date -d @"$ur"))"
echo "local  updated at $ul ($(date -d @"$ul"))"

if [[ "$ur" == "$ul" ]]; then
    # skip update
    echo "skipping update"
    if [ -n "$SYNC_LOCAL" ]; then
        echo "updating lastsync"
        rsync \
            -prltH  \
            --log-file $MIRRORGO_LOGS_PATH/rsync-lastsync.log \
            --safe-links \
            --delete-delay \
            --delay-updates \
            --timeout=600 \
            --contimeout=60 \
            --no-motd \
            "$SYNC_REMOTE" \
            "$SYNC_LOCAL"
        lu=$(cat "$SYNC_LOCAL")
        echo "lastsync is $lu ($(date -d @"$lu"))"
    fi
    echo "rsync finished"
    exit 0
fi


rsync \
	--quiet \
	--log-file $MIRRORGO_LOGS_PATH/rsync.log \
	-4 \
	--bwlimit=0 \
	-prltvHSB8192 \
	--delete-delay \
	--delay-updates \
	--safe-links \
	--partial \
	--chmod=D755,F644 \
	--timeout 120 \
	--stats \
	--no-human-readable \
	--no-inc-recursive \
	--no-motd \
	--max-delete=500000 \
	--filter='- MIRROR_IS_SYNCING' \
	--filter='-p *.~tmp~' \
	$RSYNC_EXTRA_OPTS \
	"$RSYNC_UPSTREAM" \
	/srv/mirrors/storage

echo "rsync finished"

# use 12 consecutive '-'s to separate previous output from mirror size
echo "------------"

# print total size in rsync.log
# awk '{print $7}' prints the 7th column of the line, which is the size in bytes
# example line: "2023/04/10 20:32:32 [15] total size is 779176319868  speedup is 4897.49"
SIZE=$(tail $MIRRORGO_LOGS_PATH/rsync.log | grep 'total size is' | tail -n 1 | awk '{print $7}') || true
echo "SIZE=$SIZE"
