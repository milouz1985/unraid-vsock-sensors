#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
mode="project"

case "$#" in
    0) ;;
    1)
        case "$1" in
            --debian) mode="debian" ;;
            --release) mode="release" ;;
            *) echo "Usage: $0 [--debian|--release]" >&2; exit 2 ;;
        esac
        ;;
    *) echo "Usage: $0 [--debian|--release]" >&2; exit 2 ;;
esac

validate_version() {
    if [[ ! "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
        echo "Version invalide : $1" >&2
        exit 1
    fi
}

validate_selected_version() {
    validate_version "$1"
    if [[ "$mode" == "release" && ! "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
        echo "Version de release invalide : $1 (format attendu : X.Y.Z)" >&2
        exit 1
    fi
}

print_selected_version() {
    local version="$1"

    validate_selected_version "$version"
    if [[ "$mode" == "debian" ]]; then
        # Debian sorts '~' before the corresponding final release and '+'
        # after it. Git snapshots replace their project '-dev.' separator
        # first so post-release builds remain newer than the final release.
        version="${version/-dev./+dev.}"
        if [[ "$version" == *-* ]]; then
            version="${version%%-*}~${version#*-}"
        fi
    fi
    printf '%s\n' "$version"
}

if [[ -n "${VERSION:-}" ]]; then
    print_selected_version "$VERSION"
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

print_selected_version "$version"
