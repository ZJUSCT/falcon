#!/bin/sh

#########################
# ftpsync Sync Wrapper
#########################
# Comments:
#########################
# Environment Variables:
# LOG_PATH: where ftpsync saves logs, required by cleanlogs.py
#########################
# Parameters:
# $1: sync command, e.g. 'sync:archive:debian'
#########################

set -ex

FTPSYNC="${FTPSYNC:-"ftpsync"}"
LOG_PATH=$MIRRORGO_LOGS_PATH
# CLEAN_LOGS="${CLEAN_LOGS:-"/scripts/cleanlogs.py"}"
export LOG_PATH

echo "127.0.0.1 `hostname`" >> /etc/hosts

# ${CLEAN_LOGS} || exit 1

# if [ -n "$MAX_DELETE" ]; then
#     sed -i "s/max-delete=40000/max-delete=$MAX_DELETE/g" $(which ftpsync)
# fi

# sed -i '3 i set -x' $(which ftpsync)

${FTPSYNC} "$1"

# sz=$(tail -n 15 "${LOG_PATH}"/rsync-ftpsync-*.log.0|grep -Po '(?<=Total file size: )\d+')
# [ -z "$sz" ] || echo "Total file size is " "$(numfmt --to=iec "$sz")"
