#!/bin/bash
# Build the engine-free stdio helper and sign it before the containing app.
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
root=$(cd "$(dirname "$0")/.." && pwd)
stage="$root/SushitrainCore/build/mcp-macos"
mkdir -p "$stage" "$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH"
cd "$root/SushitrainCore"
for architecture in arm64 amd64; do
    GOOS=darwin GOARCH="$architecture" CGO_ENABLED=0 go build -mod=readonly -trimpath \
        -ldflags "-s -w -X main.version=$MARKETING_VERSION.$CURRENT_PROJECT_VERSION" \
        -o "$stage/$architecture" ./cmd/synctrain-mcp
done
output="$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH/synctrain-mcp"
xcrun lipo -create "$stage/arm64" "$stage/amd64" -output "$output"
identity="${EXPANDED_CODE_SIGN_IDENTITY:--}"
if [[ "${CODE_SIGNING_ALLOWED:-YES}" == NO || -z "$identity" ]]; then identity=-; fi
codesign --force --sign "$identity" --identifier gordonbyrnes.Synctrain.MCP --options runtime "$output"
