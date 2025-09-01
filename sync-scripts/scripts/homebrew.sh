#!/bin/sh

set -ex

export GIT_UPSTREAM_URL=${BREW_URL}
export GIT_WORKING_DIR=/data/brew.git
dash /scripts/git.sh

export GIT_UPSTREAM_URL=${BREW_CORE_URL}
export GIT_WORKING_DIR=/data/homebrew-core.git
dash /scripts/git.sh

unset https_proxy

export RSYNC_UPSTREAM=${BREW_BOTTLES_URL}
bash /scripts/rsync.sh
