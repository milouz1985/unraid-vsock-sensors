#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
release_only=0

case "$#" in
    0) ;;
    1)
        if [[ "$1" == "--release" ]]; then
            release_only=1
        else
            echo "Usage: $0 [--release]" >&2
            exit 2
        fi
        ;;
    *) echo "Usage: $0 [--release]" >&2; exit 2 ;;
esac

validate_version() {
    if [[ ! "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
        echo "Version invalide : $1" >&2
        exit 1
    fi
}

validate_selected_version() {
    validate_version "$1"
    if (( release_only )) && [[ ! "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
        echo "Version de release invalide : $1 (format attendu : X.Y.Z)" >&2
        exit 1
    fi
}

if [[ -n "${VERSION:-}" ]]; then
    validate_selected_version "$VERSION"
    printf '%s\n' "$VERSION"
    exit 0
fi

if ! git -C "$repo_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "Impossible de déterminer la version Git ; définir VERSION explicitement" >&2
    exit 1
fi

tag="$(git -C "$repo_dir" describe --tags --match 'v[0-9]*' --exact-match 2>/dev/null || true)"
dirty="$(git -C "$repo_dir" status --porcelain --untracked-files=normal)"
if [[ -n "$tag" && -z "$dirty" ]]; then
    version="${tag#v}"
else
    # Development versions retain the nearest release, distance and commit so
    # packages remain traceable even when built outside a tagged release.
    base_tag="$(git -C "$repo_dir" describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || true)"
    if [[ -n "$base_tag" ]]; then
        base_version="${base_tag#v}"
        commit_count="$(git -C "$repo_dir" rev-list "$base_tag"..HEAD --count)"
    else
        base_version="0.0.0"
        commit_count="$(git -C "$repo_dir" rev-list HEAD --count)"
    fi
    revision="$(git -C "$repo_dir" rev-parse --short=12 HEAD)"
    version="${base_version}-dev.${commit_count}.g${revision}"
    if [[ -n "$dirty" ]]; then
        version+=".dirty"
    fi
fi

validate_selected_version "$version"
printf '%s\n' "$version"
