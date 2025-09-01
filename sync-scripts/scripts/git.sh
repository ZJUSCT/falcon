#!/bin/sh

#########################
# Git Repo Sync Scripts
#########################
# Comments:
# dash compatible git repo sync script modified from https://github.com/tuna/tunasync-scripts/blob/master/git.sh
#########################
# Environment Variables
# GIT_UPSTREAM_URL: upstream url of git repo (e.g. https://mirrors.zju.edu.cn/git/glibc.git)
# GIT_WORKING_DIR:  target directory (e.g. /data/git/glibc.git)
#########################

set -ex

export https_proxy=http://TZJAFoFyZt:mLyrAnmTFPoOTKIkXUBArgLQWBWpUlHt@127.0.0.1:17890

UPSTREAM=${GIT_UPSTREAM_URL}
if [ -z "$UPSTREAM" ];then
	echo "Please set the GIT_UPSTREAM_URL"
	exit 1
fi

repo_init() {
	git clone --mirror "$UPSTREAM" "$GIT_WORKING_DIR"
}

update_linux_git() {
	cd "$GIT_WORKING_DIR"
	echo "==== SYNC $UPSTREAM START ===="
	git remote set-url origin "$UPSTREAM"
	/usr/bin/timeout -s INT 3600 git remote -v update -p
	ret=$?
	[ $ret -ne 0 ] && echo "git update failed with rc=$ret"
	head="$(git remote show origin | awk '/HEAD branch:/ {print $NF}')"
	[ -n "$head" ] && echo "ref: refs/heads/$head" > HEAD
	objs=$(find objects -type f | wc -l)
	[ "$objs" -gt 8 ] && git repack -a -b -d
	sz=$(git count-objects -v|grep -Po '(?<=size-pack: )\d+')
	sz=$((sz*1024))
	echo "Total size is $(numfmt --to=iec $sz)"
	echo "==== SYNC $UPSTREAM DONE ===="
	return $ret
}

if [ ! -f "$GIT_WORKING_DIR/HEAD" ]; then
	echo "Initializing $UPSTREAM mirror"
	repo_init
fi

update_linux_git
