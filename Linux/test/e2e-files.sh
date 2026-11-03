#!/bin/bash
# End-to-end check of synctrain-files against a scratch synctrain-gateway, on one Linux machine with FUSE: pairing,
# adding a folder, reading on demand, keeping, writes, a change on the gateway, and a clean stop. Build first
# with "make -C SushitrainCore linux". Everything lives in /tmp/st-e2e: the machine's own Syncthing and ~/Synctrain are
# not touched. It prints what it sees; read it rather than trusting an exit status.
set -u
BIN=${BIN:-$(cd "$(dirname "$0")/../../SushitrainCore/build/linux-amd64" && pwd)}
ROOT=/tmp/st-e2e
cleanup() {
	[ -n "${FILES_PID:-}" ] && kill "$FILES_PID" 2>/dev/null
	[ -n "${GW_PID:-}" ] && kill "$GW_PID" 2>/dev/null
	sleep 2
	fusermount3 -uz "$ROOT/mnt" 2>/dev/null
}
trap cleanup EXIT
fusermount3 -uz "$ROOT/mnt" 2>/dev/null
rm -rf "$ROOT" && mkdir -p "$ROOT/home" "$ROOT/docs/.stfolder" "$ROOT/docs/photos" && cd "$ROOT" || exit 1

cat > home/config.xml <<'XML'
<configuration version="52">
    <folder id="docs" label="Documents" path="/tmp/st-e2e/docs" type="sendreceive" rescanIntervalS="5" fsWatcherEnabled="false">
        <versioning type="trashcan"><param key="cleanoutDays" val="0"></param></versioning>
    </folder>
    <gui enabled="true" tls="false"><address>127.0.0.1:18384</address></gui>
    <options>
        <listenAddress>tcp://127.0.0.1:22100</listenAddress>
        <globalAnnounceEnabled>false</globalAnnounceEnabled>
        <localAnnounceEnabled>false</localAnnounceEnabled>
        <relaysEnabled>false</relaysEnabled>
        <natEnabled>false</natEnabled>
        <urAccepted>-1</urAccepted>
        <crashReportingEnabled>false</crashReportingEnabled>
    </options>
</configuration>
XML
echo "hello from the gateway" > docs/readme.txt
head -c 20000000 /dev/urandom > docs/big.bin
for i in 1 2 3; do head -c 300000 /dev/urandom > "docs/photos/img$i.jpg"; done

STNODEFAULTFOLDER=1 "$BIN/synctrain-gateway" serve --home home --listen 127.0.0.1:20921 --public-host 127.0.0.1 \
	--socket gw.sock > gateway.log 2>&1 &
GW_PID=$!
for i in $(seq 1 60); do [ -S gw.sock ] && break; sleep 0.5; done
[ -S gw.sock ] || { echo "gateway did not start"; tail -20 gateway.log; exit 1; }
sleep 3
"$BIN/synctrain-gateway" exports --socket gw.sock --add docs --write || exit 1

"$BIN/synctrain-files" serve --state files-state --cache files-cache --mount mnt --socket files.sock \
	--allow-local-gateway --log-level debug > files.log 2>&1 &
FILES_PID=$!
for i in $(seq 1 40); do [ -S files.sock ] && break; sleep 0.5; done
[ -S files.sock ] || { echo "files daemon did not start"; cat files.log; exit 1; }

# The gateway shows a link and approves the first request itself with --yes
"$BIN/synctrain-gateway" pair --socket gw.sock --folder docs:write --yes > pair.log 2>&1 &
PAIR_PID=$!
for i in $(seq 1 40); do grep -q 'synctrain+pair://' pair.log && break; sleep 0.5; done
LINK=$(grep -o 'synctrain+pair://[^ ]*' pair.log | head -1)
[ -n "$LINK" ] || { echo "no pairing link"; cat pair.log; exit 1; }
echo "== pair"
timeout 60 "$BIN/synctrain-files" pair --socket files.sock "$LINK" || { cat pair.log; exit 1; }
wait $PAIR_PID
SERVICE=$("$BIN/synctrain-files" gateways --socket files.sock --json | grep -o '"serviceID": "[^"]*"' | head -1 | cut -d'"' -f4)
echo "== gateway's folders"
"$BIN/synctrain-files" folders --socket files.sock --gateway "$SERVICE"
"$BIN/synctrain-files" folders --socket files.sock --gateway "$SERVICE" --add docs || exit 1
for i in $(seq 1 40); do [ -e mnt/Documents/readme.txt ] && break; sleep 0.5; done
echo "== status"
"$BIN/synctrain-files" status --socket files.sock
echo "== mount"
grep synctrain /proc/mounts
ls -la mnt mnt/Documents mnt/Documents/photos
echo "== read"
cat mnt/Documents/readme.txt
"$BIN/synctrain-files" state --socket files.sock "$ROOT/mnt/Documents/big.bin"
dd if=mnt/Documents/big.bin bs=4096 skip=4000 count=1 status=none | cmp - <(dd if=docs/big.bin bs=4096 skip=4000 count=1 status=none) && echo "partial read matches"
getfattr -n user.synctrain.state mnt/Documents/big.bin
cmp mnt/Documents/big.bin docs/big.bin && echo "whole file matches"
sleep 1
getfattr -n user.synctrain.state mnt/Documents/big.bin
echo "== keep"
setfattr -n user.synctrain.keep -v 1 mnt/Documents/photos
for i in $(seq 1 40); do [ "$(getfattr --only-values -n user.synctrain.state mnt/Documents/photos/img3.jpg 2>/dev/null)" = kept ] && break; sleep 0.5; done
"$BIN/synctrain-files" state --socket files.sock "$ROOT"/mnt/Documents/photos/*.jpg
echo "== writes"
echo "made here" > mnt/Documents/new.txt
sed -i 's/hello/edited/' mnt/Documents/readme.txt
mkdir -p mnt/Documents/made/here && echo deep > mnt/Documents/made/here/file.txt
"$BIN/synctrain-files" pending --socket files.sock
"$BIN/synctrain-files" wait --socket files.sock "$ROOT/mnt/Documents" && echo "all changes on the gateway"
cat docs/new.txt docs/readme.txt docs/made/here/file.txt
ls -a docs | tr '\n' ' '; echo
echo "== a change on the gateway"
echo "changed" > docs/readme.txt
"$BIN/synctrain-gateway" folders --socket gw.sock >/dev/null
curl -s -X POST -H "X-API-Key: $(grep -o '<apikey>[^<]*' home/config.xml | cut -d'>' -f2)" "http://127.0.0.1:18384/rest/db/scan?folder=docs" >/dev/null
for i in $(seq 1 40); do [ "$(cat mnt/Documents/readme.txt)" = changed ] && break; sleep 0.5; done
echo "readme now: $(cat mnt/Documents/readme.txt)"
echo "== GIO"
gio info -a filesystem::remote,filesystem::type,standard::content-type mnt/Documents/readme.txt 2>&1 | tail -4
echo "== stop"
kill -TERM "$FILES_PID"; wait "$FILES_PID"; echo "daemon exit $?"; FILES_PID=
grep -c synctrain /proc/mounts
echo "== daemon log (warnings)"
grep -E 'level=(WARN|ERROR)' files.log | head -20
