#!/bin/sh
# Build the fork and (re)install the model proxy as a launchd service for the current user.
# Safe to re-run: it rebuilds, keeps an existing config.yaml, and restarts the services.
#   deploy/studio/install.sh            build, install, restart
#   deploy/studio/install.sh --render   also re-render config.yaml from the template + keys.env
set -eu
PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin; export PATH
REPO=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DEST="$HOME/model-proxy"
umask 077
mkdir -p "$DEST/bin" "$DEST/auths" "$DEST/static" "$DEST/logs"
chmod 700 "$DEST" "$DEST/auths"

cd "$REPO"
VERSION=$(git describe --tags --always 2>/dev/null || echo dev)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)
go build -buildvcs=false -ldflags="-s -w -X 'main.Version=$VERSION' -X 'main.Commit=$COMMIT' -X 'main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)'" -o "$DEST/bin/cli-proxy-api.new" ./cmd/server
mv "$DEST/bin/cli-proxy-api.new" "$DEST/bin/cli-proxy-api"
echo "built $VERSION ($COMMIT)"

if [ ! -f "$DEST/config.yaml" ] || [ "${1:-}" = "--render" ]; then
  [ -f "$DEST/keys.env" ] || { echo "missing $DEST/keys.env (see README)"; exit 1; }
  python3 - "$REPO/deploy/studio/config.template.yaml" "$DEST/keys.env" "$DEST/config.yaml" <<'PY'
import sys,re,os
tpl,keys,out=sys.argv[1:]
vals=dict(l.strip().split("=",1) for l in open(keys) if "=" in l and not l.startswith("#"))
text=open(tpl).read()
missing=sorted(set(re.findall(r"__([A-Z0-9_]+)__",text))-set(vals))
if missing: sys.exit("keys.env lacks: "+", ".join(missing))
text=re.sub(r"__([A-Z0-9_]+)__",lambda m:vals[m.group(1)],text)
fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600); os.write(fd,text.encode()); os.close(fd)
PY
  echo "rendered $DEST/config.yaml"
fi
chmod 600 "$DEST/config.yaml"

for name in systems.aeron.model-proxy systems.aeron.model-proxy-tunnel; do
  plist="$HOME/Library/LaunchAgents/$name.plist"
  if [ "$name" = systems.aeron.model-proxy-tunnel ] && [ ! -f "$HOME/.cloudflared/model-proxy.token" ]; then
    echo "skip $name: no tunnel token"; continue
  fi
  sed "s#__HOME__#$HOME#g" "$REPO/deploy/studio/$name.plist" > "$plist"
  launchctl bootout "gui/$(id -u)/$name" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  echo "started $name"
done
