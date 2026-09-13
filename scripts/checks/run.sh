#!/usr/bin/env bash
set -euo pipefail

# Copy current tracked and non-ignored untracked files, including unstaged edits.
# No check writes to the checkout or its Git index. Missing tracked files represent
# local deletions and are omitted. Each invocation gets its own build workspace.
workspace=$(mktemp -d)
trap 'rm -rf "$workspace"' EXIT
cd /source
while IFS= read -r -d '' file; do
    if [[ -e "$file" || -L "$file" ]]; then
        printf '%s\0' "$file"
    fi
done < <(git -c safe.directory=/source ls-files --cached --others --exclude-standard -z) > "$workspace/files"
tar --null -T "$workspace/files" -cf - | tar -C "$workspace" -xf -
rm "$workspace/files"
cd "$workspace"
git init -q
git add --force --all
git -c user.name=Falcon -c user.email=checks@localhost -c commit.gpgsign=false commit -qm 'Check input'

generate() {
    controller-gen object:headerFile= paths=./api/... \
        crd:allowDangerousTypes=true paths=./api/... \
        output:crd:artifacts:config=charts/falcon/crds
}

export_changes() {
    # Only explicit write services mount /output. Copy changed files without
    # touching the host index or unrelated files; run as the invoking user.
    git add -N --force .
    while IFS= read -r -d '' file; do
        cp -- "$file" "/output/$file"
    done < <(git diff --name-only -z)
}

case "${1:?check name required}" in
    go-checks)
        golangci-lint run
        go test -race -count=1 ./...
        CGO_ENABLED=0 go build -trimpath -o /tmp/falcon-controller ./cmd/controller
        CGO_ENABLED=0 go build -trimpath -o /tmp/zfs-agent ./cmd/zfs-agent
        ;;
    ui-checks)
        cd ui
        npm ci --no-audit --no-fund
        npm run build
        ;;
    chart-checks)
        helm lint charts/falcon --strict
        python scripts/validate-chart.py
        helm package charts/falcon --destination /tmp
        ;;
    hygiene)
        pre-commit run --all-files
        ;;
    verify-generated)
        generate
        git add -N --force api charts/falcon/crds
        git diff --exit-code -- api charts/falcon/crds
        ;;
    generate)
        generate
        export_changes
        ;;
    format-go)
        golangci-lint fmt
        export_changes
        ;;
    format-hygiene)
        result=0
        pre-commit run --all-files || result=$?
        export_changes
        if [[ $result != 0 ]]; then
            # Recheck after auto-fixes; unresolved errors still fail formatting.
            pre-commit run --all-files
        fi
        ;;
    e2e)
        exec bash scripts/e2e/run.sh
        ;;
    *) echo "Unknown check: $1" >&2; exit 2 ;;
esac
