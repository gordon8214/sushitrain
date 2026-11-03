#!/bin/bash
# Execute a PowerShell script as the signed-in user of the local development VM.
set -euo pipefail
if [[ $# -ne 1 ]]; then
  echo "Usage: $0 script.ps1" >&2
  exit 2
fi
# Parallels rejects long command lines. Transfer the script through the shared
# source tree instead of expanding its contents into an EncodedCommand argument.
windows_root=$(cd "$(dirname "$0")/.." && pwd)
case "$windows_root" in
  "$HOME"/*) ;;
  *) echo "The development checkout must be under the shared home directory." >&2; exit 1 ;;
esac
mkdir -p "$windows_root/build/vm-scripts"
task_directory=$(mktemp -d "$windows_root/build/vm-scripts/task.XXXXXXXX")
task_script="$task_directory/script.ps1"
trap 'rm -f "$task_script"; rmdir "$task_directory"' EXIT
cp "$1" "$task_script"
shared_path="\\\\Mac\\Home\\${task_script#"$HOME"/}"
shared_path=${shared_path//\//\\}
prlctl exec 'Windows 11' --current-user powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$shared_path"
