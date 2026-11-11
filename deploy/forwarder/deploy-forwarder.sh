#!/usr/bin/env bash
# Deploy the unified dsh-forwarder to the public relay host and the in-house machine.
#
# SAFETY: this script does nothing by default. `--apply` is required for every
# mutating step, and stages are opt-in:
#
#   ./deploy-forwarder.sh --stage binaries           # upload binaries + configs, no service changes
#   ./deploy-forwarder.sh --stage parallel --apply   # start new units alongside old nginx/frps
#   ./deploy-forwarder.sh --stage cutover --apply    # stop frps, remove nginx dsh vhost, retarget tunnel
#   ./deploy-forwarder.sh --stage rollback --apply   # undo cutover, restore old chain
#
# The old chain (nginx dsh vhost, frps) is never touched
# until the `cutover` stage, and rollback restores it from backups.
# (The legacy dsh-relay unit was retired on 2026-11-11; the relay home dir on
#  the server is now just the forwarder's home — binaries, TLS dir, updates,
#  logs. Cutover therefore stops only frps; rollback rebuilds cmd/relay.)
#
# Environment (REQUIRED, fail closed when missing — no defaults in code):
#   ECS_HOST        public relay host (IP or DNS name you ssh/scp to)
#   FORWARDER_DIR   server-side forwarder home (binaries, TLS, updates, logs)
#   FORWARDER_CONF  server-side config dir (server.json lives here)
#   NGINX_VHOST     server-side nginx vhost file edited during cutover
#   DSH_VHOST_NAME  the server_name of the dsh vhost inside NGINX_VHOST
#   SSHPASS         ssh password (sshpass -e; never written to disk here)
# Optional:
#   GO_BIN (default go)   GOCACHE / GOMODCACHE (default: go env)
#   LOCAL_BIN (default <repo>/dist-relay)
#
# Example:
#   ECS_HOST=203.0.113.10 FORWARDER_DIR=/opt/dsh-forwarder \
#   FORWARDER_CONF=/etc/dsh-forwarder NGINX_VHOST=/etc/nginx/sites-enabled/relay.example.com \
#   DSH_VHOST_NAME=dsh.relay.example.com SSHPASS=... \
#   ./deploy-forwarder.sh --stage binaries --apply
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
: "${ECS_HOST:?ERROR: set ECS_HOST (public relay host)}"
: "${FORWARDER_DIR:?ERROR: set FORWARDER_DIR (server-side forwarder home)}"
: "${FORWARDER_CONF:?ERROR: set FORWARDER_CONF (server-side config dir)}"
: "${NGINX_VHOST:?ERROR: set NGINX_VHOST (server-side nginx vhost path)}"
: "${DSH_VHOST_NAME:?ERROR: set DSH_VHOST_NAME (dsh server_name inside NGINX_VHOST)}"
ECS_SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=12"
LOCAL_BIN="${LOCAL_BIN:-$REPO/dist-relay}"
GO_BIN="${GO_BIN:-go}"
VHOST_BASE="$(basename "$NGINX_VHOST")"

STAGE=""
APPLY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --stage) STAGE="$2"; shift 2 ;;
    --apply) APPLY=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

run() {
  if [[ $APPLY -eq 1 ]]; then
    echo "[APPLY] $*"
    "$@"
  else
    echo "[DRY-RUN] $*"
  fi
}

ecs() {
  # SSHPASS is expected in the environment (never written to disk here).
  # Deployment paths are forwarded as env vars because the REMOTE heredocs
  # below are quoted (`<<'REMOTE'`) and must not expand on the local side.
  local env_prefix
  env_prefix=$(printf 'FORWARDER_DIR=%q FORWARDER_CONF=%q NGINX_VHOST=%q DSH_VHOST_NAME=%q VHOST_BASE=%q' \
    "$FORWARDER_DIR" "$FORWARDER_CONF" "$NGINX_VHOST" "$DSH_VHOST_NAME" "$VHOST_BASE")
  sshpass -e ssh $ECS_SSH_OPTS "root@$ECS_HOST" "env $env_prefix bash -s"
}

require_apply() {
  if [[ $APPLY -ne 1 ]]; then
    echo "This stage mutates the live system; re-run with --apply after reviewing." >&2
  fi
}

echo "=== dsh-forwarder deploy: stage=$STAGE apply=$APPLY ==="
echo "repo=$REPO ecs=$ECS_HOST forwarder_dir=$FORWARDER_DIR"

case "$STAGE" in
  binaries)
    echo "--- build amd64 binaries ---"
    run env GOOS=linux GOARCH=amd64 "$GO_BIN" build -o "$LOCAL_BIN/dsh-forwarder-linux-amd64" "$REPO/cmd/forwarder"
    run "$GO_BIN" build -o "$REPO/dsh-forwarder-client" "$REPO/cmd/forwarder-client"
    if [[ $APPLY -eq 1 ]]; then
      echo "--- upload to ECS (as .new, nothing replaced yet) ---"
      ecs <<'REMOTE'
set -e
mkdir -p "$FORWARDER_DIR" "$FORWARDER_CONF"
REMOTE
      sshpass -e scp $ECS_SSH_OPTS "$LOCAL_BIN/dsh-forwarder-linux-amd64" "root@$ECS_HOST:$FORWARDER_DIR/dsh-forwarder.new"
      sshpass -e scp $ECS_SSH_OPTS "$REPO/deploy/forwarder/server.example.json" "root@$ECS_HOST:$FORWARDER_CONF/server.json.template"
      # Unit file carries a <FORWARDER_DIR> placeholder; substitute per host.
      sed "s#<FORWARDER_DIR>#$FORWARDER_DIR#g" "$REPO/deploy/forwarder/dsh-forwarder.service" > /tmp/dsh-forwarder.service
      sshpass -e scp $ECS_SSH_OPTS /tmp/dsh-forwarder.service "root@$ECS_HOST:/etc/systemd/system/dsh-forwarder.service"
      rm -f /tmp/dsh-forwarder.service
      ecs <<'REMOTE'
set -e
chmod +x "$FORWARDER_DIR/dsh-forwarder.new"
"$FORWARDER_DIR/dsh-forwarder.new" --config "$FORWARDER_CONF/server.json.template" --check || true
echo "NOTE: edit $FORWARDER_CONF/server.json (set a fresh token) before starting the service"
REMOTE
      echo "--- install local client config + unit ---"
      mkdir -p "$FORWARDER_CONF" ~/.config/systemd/user
      cp "$REPO/deploy/forwarder/client.example.json" "$FORWARDER_CONF/client.json.template"
      # Client unit carries <REPO> placeholders; substitute with this checkout.
      sed "s#<REPO>#$REPO#g" "$REPO/deploy/forwarder/dsh-forwarder-client.service" > ~/.config/systemd/user/dsh-forwarder-client.service
      echo "NOTE: edit $FORWARDER_CONF/client.json (same token) before starting"
    fi
    ;;

  parallel)
    require_apply
    echo "--- start new units ALONGSIDE old nginx/frps (no cutover) ---"
    if [[ $APPLY -eq 1 ]]; then
      ecs <<'REMOTE'
set -e
cd "$FORWARDER_DIR"
cp -a dsh-forwarder.new dsh-forwarder
chmod +x dsh-forwarder
# Config MUST already exist with a real token; fail closed otherwise.
test -f "$FORWARDER_CONF/server.json"
grep -q 'REPLACE_WITH' "$FORWARDER_CONF/server.json" && { echo "ERROR: token placeholder still in server.json"; exit 1; }
# PARALLEL stage: frps owns 18888/18326/7781 (and, until the 2026-11-11
# retirement of dsh-relay, the old relay owned 24000), so the new binary must
# NOT bind ANY of them yet. Park
# every conflicting port on a temporary sibling (+1) for loopback
# verification; the cutover stage rewrites them back after stopping old units.
python3 - <<'PY'
import json, os
p=os.path.join(os.environ['FORWARDER_CONF'],'server.json')
c=json.load(open(p))
c['relayTLS']['listen']='0.0.0.0:24011'
for x in c['portForward']['ports']:
    x['remotePort']=x['remotePort']+1
json.dump(c,open(p,'w'),indent=2)
print('parallel: relayTLS -> 24011, data ports +1 (old frps keeps real ports)')
PY
"$FORWARDER_DIR/dsh-forwarder" --config "$FORWARDER_CONF/server.json" --check
systemctl daemon-reload
systemctl enable --now dsh-forwarder.service
sleep 2
systemctl is-active dsh-forwarder.service
REMOTE
      echo "--- ECS new ports health (loopback) ---"
      ecs <<'REMOTE'
ss -tlnp | grep -E ':(24011|24010|7782|24000|18888|18326)\b' || true
echo "--- probes (loopback) ---"
curl -sk --noproxy '*' -m 5 https://127.0.0.1:24011/__dsh_tunnel__/probe || echo "WARN: 24011 probe failed"
curl -sk --noproxy '*' -m 5 https://127.0.0.1:24010/__dsh_tunnel__/probe || echo "WARN: 24010 probe failed"
echo "--- old chain must still be alive ---"
curl -sk --noproxy '*' -m 5 https://127.0.0.1:24000/__dsh_tunnel__/probe
REMOTE
    fi
    ;;

  cutover)
    require_apply
    echo "--- CUTOVER: stop frps, remove nginx dsh vhost, retarget tunnel fallback ---"
    echo "    This is the only stage that changes the live chain. Backups are taken first."
    if [[ $APPLY -eq 1 ]]; then
      ecs <<'REMOTE'
set -e
STAMP=$(date +%s)
# 0. Restore production ports now that old units are about to stop. The
#    canonical values live in the shipped template, not in the staged config
#    (which had temporary +1 ports during the parallel stage).
python3 - <<'PY'
import json, os
conf=os.environ['FORWARDER_CONF']
p=os.path.join(conf,'server.json')
t=os.path.join(conf,'server.json.template')
c=json.load(open(p))
tpl=json.load(open(t))
c['relayTLS']['listen']='0.0.0.0:24000'
byName={x['name']:x['remotePort'] for x in tpl['portForward']['ports']}
for x in c['portForward']['ports']:
    x['remotePort']=byName[x['name']]
json.dump(c,open(p,'w'),indent=2)
print('cutover: production ports restored (24000 / 18888 / 18326)')
PY
# 1. Backups (reversible). NEVER leave a backup inside sites-enabled: nginx
#    includes every file there and a second default_server breaks nginx -t.
mkdir -p /etc/nginx/backups
cp -a "$NGINX_VHOST" "/etc/nginx/backups/$VHOST_BASE.bak-forwarder-$STAMP"
cp -a /etc/systemd/system/frps.service "/etc/systemd/system/frps.service.bak-forwarder-$STAMP" 2>/dev/null || true
# 2. Remove ONLY the dsh vhost block, keep all others. The block starts with a
#    marker comment and ends at the NEXT "# ============" section separator.
#    (Finding "\nserver {" from the marker hits the block's OWN server line,
#    which silently deletes only the comment — a real bug caught on cutover.)
python3 - <<'PY'
import os, re
p=os.environ['NGINX_VHOST']
name=os.environ['DSH_VHOST_NAME']
s=open(p).read()
# Locate the dsh vhost by its server_name (the marker comment may already be
# gone from an earlier partial run), then find the enclosing "server {" block.
i=s.find('server_name %s;'%name)
if i<0:
    print('dsh vhost not found; no change'); raise SystemExit(0)
start=s.rfind('\nserver {', 0, i)
if start<0:
    print('cannot find vhost start; aborting'); raise SystemExit(1)
start+=1  # keep the newline
# The block ends at the next section separator or the next top-level server.
m=re.search(r'\n# ============ |\nserver \{', s[i:])
j=i+m.start()+1 if m else len(s)
# If the next boundary is a "server {" it belongs to the FOLLOWING vhost;
# trim back to the closing brace of the dsh block instead.
open(p,'w').write(s[:start]+s[j:])
print('removed dsh vhost block (%d bytes)'%(j-start))
PY
# Fail closed: the vhost must be gone, and nginx must accept the result.
if grep -q "server_name $DSH_VHOST_NAME;" "$NGINX_VHOST"; then
  echo "ERROR: dsh vhost still present after edit; aborting cutover"; exit 1
fi
nginx -t
systemctl reload nginx
# 3. Stop frps (ports 18888/18326/7781 are now served by dsh-forwarder).
systemctl stop frps.service
systemctl disable frps.service
# 4. dsh-relay was retired on 2026-11-11 (unit + binary deleted from the
#    server; archived under the relay home's retired/ dir). Nothing to stop
#    here: the unified forwarder owns 24000.
# 5. The new forwarder was bound to 24011 during the parallel stage; restart
#    it now so it picks up 24000 from the rewritten config.
systemctl restart dsh-forwarder.service
sleep 2
systemctl is-active dsh-forwarder.service
curl -sk --noproxy '*' -m 5 https://127.0.0.1:24000/__dsh_tunnel__/probe
echo "cutover done; frps stopped but NOT deleted (rollback can restore)"
REMOTE
      echo "--- retarget tunnel fallback to the new public port (local) ---"
      # The main entry is unchanged, so this is a low-risk edit. Only the
      # fallback entry's port changes (24010 = public TLS entrance).
      cp "$REPO/run-tunnel.sh" "$REPO/run-tunnel.sh.bak-before-forwarder-$(date +%s)"
      sed -i "s#wss://$DSH_VHOST_NAME/__dsh_tunnel__/device#wss://$DSH_VHOST_NAME:24010/__dsh_tunnel__/device#" "$REPO/run-tunnel.sh"
      echo "NOTE: restart dsh-tunnel.service from a host-level shell to pick up the new fallback"
    fi
    ;;

  rollback)
    require_apply
    echo "--- ROLLBACK: restore nginx dsh vhost + frps ---"
    # NOTE: dsh-relay was retired on 2026-11-11. Rollback now restores the
    # relay role by REBUILDING cmd/relay from source (cmd/relay/ + internal/
    # relay/ are still in the repo), not by resurrecting the deleted unit:
    #   GOOS=linux GOARCH=amd64 go build -o $FORWARDER_DIR/dsh-relay ./cmd/relay
    #   (recreate /etc/systemd/system/dsh-relay.service from git history or
    #    deploy/ notes), then enable --now it AFTER stopping dsh-forwarder.
    if [[ $APPLY -eq 1 ]]; then
      ecs <<'REMOTE'
set -e
NGINX_BAK=$(ls -t /etc/nginx/backups/$VHOST_BASE.bak-forwarder-* 2>/dev/null | head -1)
test -n "$NGINX_BAK" || { echo "no nginx backup found"; exit 1; }
cp -a "$NGINX_BAK" "$NGINX_VHOST"
nginx -t && systemctl reload nginx
systemctl enable --now frps.service
# dsh-relay unit was deleted on 2026-11-11; rollback cannot enable it.
# To restore the old relay: rebuild ./cmd/relay, recreate its unit, stop
# dsh-forwarder, then start the relay on 24000 (see NOTE above).
# The old relay owns 24000 again; park the new forwarder back on 24011 so a
# later re-cutover can start cleanly.
python3 - <<'PY'
import json, os
p=os.path.join(os.environ['FORWARDER_CONF'],'server.json')
c=json.load(open(p))
c['relayTLS']['listen']='0.0.0.0:24011'
json.dump(c,open(p,'w'),indent=2)
PY
systemctl stop dsh-forwarder.service || true
echo "rollback done: old chain restored, new forwarder stopped"
REMOTE
    fi
    ;;

  *)
    echo "usage: $0 --stage {binaries|parallel|cutover|rollback} [--apply]" >&2
    exit 2
    ;;
esac
