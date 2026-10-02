# laravel-dev-mcp dev tasks.

# List all recipes with their descriptions.
default:
    @just --list

# Run every release gate in order: vet, lint, test, build, then keep package.nix's vendorHash in sync.
check: vet lint test build sync-flake

# Static analysis of every package with go vet.
vet:
    go vet ./...

# Lint every package; settings live in .golangci.yml.
lint:
    golangci-lint run

# Run the test suite for every package.
test:
    go test ./...

# Compile-check every package; the output is discarded.
build:
    go build -o /dev/null ./...

# Format every Go source in place with the formatters from .golangci.yml (gofumpt, gci).
fmt:
    golangci-lint fmt

# Build the flake package and run its checks.
nix-check:
    nix flake check --print-build-logs

# A sha256 of go.sum is cached as a `# go-sum:` line in package.nix; when it
# matches, this returns immediately. `--force` recomputes regardless.
# Align package.nix's vendorHash with go.sum; pass a version to also set the package version.
sync-flake version="":
    #!/usr/bin/env bash
    set -euo pipefail
    ARG="{{version}}"
    FORCE=0
    VERSION=""
    case "$ARG" in
        "")          ;;
        "--force")   FORCE=1 ;;
        *)           VERSION="${ARG#v}" ;;
    esac

    GO_SUM_HASH=$(sha256sum go.sum | awk '{print $1}')
    CACHED_HASH=$(awk -F': ' '/^[[:space:]]*#[[:space:]]*go-sum:/ {print $2; exit}' package.nix | tr -d ' ')
    CURRENT_VERSION=$(awk -F'"' '/^[[:space:]]*version = "/ {print $2; exit}' package.nix)

    NEED_HASH=0
    NEED_VERSION=0
    if [ "$FORCE" = "1" ] || [ "$GO_SUM_HASH" != "$CACHED_HASH" ]; then NEED_HASH=1; fi
    if [ -n "$VERSION" ] && [ "$VERSION" != "$CURRENT_VERSION" ]; then NEED_VERSION=1; fi

    if [ "$NEED_HASH" = "0" ] && [ "$NEED_VERSION" = "0" ]; then
        echo "sync-flake: up-to-date (go.sum=$GO_SUM_HASH version=$CURRENT_VERSION)"
        exit 0
    fi

    echo "sync-flake: refreshing (need_hash=$NEED_HASH need_version=$NEED_VERSION)"

    if [ "$NEED_HASH" = "1" ]; then
        SENTINEL="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$SENTINEL"'";|' package.nix
        set +e
        OUT=$(nix build .#laravel-dev-mcp --no-link 2>&1)
        BUILD_STATUS=$?
        set -e
        NEW_HASH=$(printf '%s\n' "$OUT" | awk '/got:[[:space:]]*sha256-/ {print $2; exit}')
        if [ -z "$NEW_HASH" ]; then
            if [ "$BUILD_STATUS" = "0" ]; then
                echo "sync-flake: unexpected nix build success with sentinel hash" >&2
                echo "$OUT" >&2
                exit 1
            fi
            echo "$OUT" >&2
            echo "sync-flake: nix build failed without printing 'got: sha256-…'" >&2
            exit 1
        fi
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$NEW_HASH"'";|' package.nix
        if grep -q '^[[:space:]]*# go-sum:' package.nix; then
            sed -i -E 's|^(\s*# go-sum:).*|\1 '"$GO_SUM_HASH"'|' package.nix
        else
            sed -i -E 's|^(\s*vendorHash = )|  # go-sum: '"$GO_SUM_HASH"'\n\1|' package.nix
        fi
        echo "sync-flake: vendorHash=$NEW_HASH go-sum=$GO_SUM_HASH"
    fi

    if grep -q '^\s*vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="' package.nix; then
        echo "sync-flake: refusing to leave sentinel vendorHash in package.nix" >&2
        exit 1
    fi

    if [ "$NEED_VERSION" = "1" ]; then
        sed -i -E 's|^(\s*version = )"[^"]*";|\1"'"$VERSION"'";|' package.nix
        echo "sync-flake: version=$VERSION"
    fi

    nix build .#laravel-dev-mcp --no-link

# ─────────────────────────── Release ───────────────────────────

# Show the next major/minor/patch tags.
release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CURRENT_TAG=$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)
    CURRENT_TAG=${CURRENT_TAG:-v0.0.0}
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    echo "Current tag: $CURRENT_TAG"
    echo "  release-major: v$((MAJOR + 1)).0.0"
    echo "  release-minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  release-patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"

_release-checks:
    #!/usr/bin/env bash
    set -euo pipefail
    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    DEFAULT_BRANCH=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||' || true)
    if [ -z "${DEFAULT_BRANCH:-}" ]; then
        DEFAULT_BRANCH=$(git remote show origin 2>/dev/null | awk '/HEAD branch/ {print $NF}' || echo master)
    fi
    if [ "$BRANCH" != "$DEFAULT_BRANCH" ]; then
        echo "Error: not on default branch '$DEFAULT_BRANCH' (currently '$BRANCH')." >&2
        exit 1
    fi
    just check

_release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    just _release-checks
    CURRENT_TAG=$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)
    CURRENT_TAG=${CURRENT_TAG:-v0.0.0}
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    case "{{bump}}" in
        major) NEW="$((MAJOR + 1)).0.0" ;;
        minor) NEW="${MAJOR}.$((MINOR + 1)).0" ;;
        patch) NEW="${MAJOR}.${MINOR}.$((PATCH + 1))" ;;
        *) echo "unknown bump kind: {{bump}}"; exit 1 ;;
    esac
    just sync-flake "${NEW}"
    # Keep the npm wrapper's version in lockstep — the release workflow's npm
    # job refuses to publish if package.json doesn't match the tag.
    sed -i -E 's|^(\s*"version": )"[^"]*",|\1"'"${NEW}"'",|' package.json
    if [ -n "$(git status --porcelain package.nix package.json)" ]; then
        git add package.nix package.json
        git commit -m "chore: bump to v${NEW}"
    fi
    git tag -a "v${NEW}" -m "v${NEW}"
    git push origin HEAD
    git push origin "v${NEW}"
    echo
    echo "Tagged v${NEW}. Watch: gh run watch || open https://github.com/stubbedev/laravel-dev-mcp/actions"

# Tag and push the next patch release.
release-patch: (_release "patch")
# Tag and push the next minor release.
release-minor: (_release "minor")
# Tag and push the next major release.
release-major: (_release "major")
