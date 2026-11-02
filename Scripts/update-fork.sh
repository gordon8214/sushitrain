#!/bin/bash
# Copyright (C) 2026 Gordon
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.

# Moves the app to another commit of the Syncthing fork, which must be pushed and checked out next to this repository as
# `syncthing`. It pins the commit in go.mod, checks that the web GUI built into that commit is current, regenerates
# notices.html, raises the engine's version and rebuilds the framework. The changes are left uncommitted to review. See
# "Moving to another commit of the fork" in the README.
#
#   Scripts/update-fork.sh <fork commit or branch>
set -euo pipefail

fail() {
	echo "error: $*" >&2
	exit 1
}

[[ $# -eq 1 ]] || {
	echo "usage: $0 <fork commit or branch>" >&2
	exit 2
}
ref=$1

repository=$(cd "$(dirname "$0")/.." && pwd)
fork="$(dirname "$repository")/syncthing"
fork_module=github.com/gordon8214/syncthing
# Everything this changes, which also has to be what it can put back
changed_files=(go.mod go.sum notices.html Makefile)

git -C "$fork" rev-parse --git-dir >/dev/null 2>&1 || fail "the fork isn't checked out at $fork"
cd "$repository/SushitrainCore"

git -C "$fork" fetch --quiet origin || fail "can't fetch the fork from origin"

commit=$(git -C "$fork" rev-parse --verify --quiet "$ref^{commit}") ||
	commit=$(git -C "$fork" rev-parse --verify --quiet "origin/$ref^{commit}") ||
	fail "the fork has no commit or branch $ref"

# A branch means what is pushed, which is ahead of a local branch after a push from another computer
if pushed_tip=$(git -C "$fork" rev-parse --verify --quiet "refs/remotes/origin/$ref^{commit}") &&
	git -C "$fork" show-ref --verify --quiet "refs/heads/$ref" &&
	git -C "$fork" merge-base --is-ancestor "$commit" "$pushed_tip"; then
	commit=$pushed_tip
fi

[[ -n $(git -C "$fork" for-each-ref --count=1 --contains "$commit" refs/remotes/origin) ]] ||
	fail "${commit:0:8} ($ref) isn't pushed to the fork's origin"

version=$(go list -m -f '{{.Version}}' "$fork_module@$commit") ||
	fail "Go can't find ${commit:0:8} in $fork_module"
current_version=$(go list -m -f '{{.Replace.Version}}' github.com/syncthing/syncthing)
if [[ $version == "$current_version" ]]; then
	echo "go.mod is already at ${commit:0:8}."
	exit 0
fi

git diff --quiet HEAD -- "${changed_files[@]}" ||
	fail "commit or restore the changes to ${changed_files[*]} in SushitrainCore first, to keep the diff to this move"

scratch=$(mktemp -d)
restore_on_failure=false

finish() {
	local status=$?
	if [[ $status -ne 0 && $restore_on_failure == true ]]; then
		git checkout HEAD -- "${changed_files[@]}"
		echo "Put ${changed_files[*]} back." >&2
	fi
	rm -rf "$scratch"
}
trap finish EXIT

echo "Moving from ${current_version##*-} to ${version##*-}"
git -C "$fork" log -1 --format='  %s' "$commit"

restore_on_failure=true
go mod edit -replace "github.com/syncthing/syncthing=$fork_module@$version"
go mod tidy

echo "Checking the web GUI built into that commit"
git -C "$fork" archive "$commit" gui | tar -x -C "$scratch"
go run ./cmd/check-gui-assets "$scratch/gui" ||
	fail "lib/api/auto/gui.files.go doesn't match gui/ at ${commit:0:8}: run \`go run build.go assets\` in the fork, push, and try again"

echo "Regenerating notices.html"
if ! make -s -B notices.html 2>"$scratch/notices.log"; then
	cat "$scratch/notices.log" >&2
	fail "couldn't regenerate notices.html"
fi

engine_version=$(sed -n 's/^ENGINE_VERSION = //p' Makefile)
[[ $engine_version =~ ^(.+\.)([0-9]+)$ ]] || fail "ENGINE_VERSION ($engine_version) doesn't end in a number to raise"
new_engine_version=${BASH_REMATCH[1]}$((BASH_REMATCH[2] + 1))
sed "s/^ENGINE_VERSION = .*/ENGINE_VERSION = $new_engine_version/" Makefile >"$scratch/Makefile"
cat "$scratch/Makefile" >Makefile

# From here a failure is most likely code that needs to change for the new commit, which needs these changes to stay.
restore_on_failure=false
echo "Building the framework as $new_engine_version"
make || fail "the framework doesn't build against this commit. The changes are in place: fix the code, then run \`make -C SushitrainCore\`"

git diff --stat HEAD -- "${changed_files[@]}"
echo "Moved to ${commit:0:8} as $new_engine_version. Review the changes and commit them."
