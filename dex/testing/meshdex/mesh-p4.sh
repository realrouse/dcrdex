#!/usr/bin/env bash
# SIMNET ONLY. A private N-node bisoncraft/mesh for the P4+ robustness tests,
# so node kills don't touch the shared tatanka-test mesh.
#   mesh-p4.sh start | stop | kill N | restart N | addr N | id N | up N
# Defaults (P4/P5): tmux session tatanka-p4 (windows node-1..3), p2p ports
# 13346-8, metrics 13356-8, admin 13366-8, data in $ROOT. Override with
# SESSION, N, PORT_BASE (p2p = base+i, metrics +10, admin +20) and ROOT; P6
# uses SESSION=tatanka-p6 N=5 PORT_BASE=13445 ROOT=run/p6mesh.
set -euo pipefail
TATANKA=${TATANKA:-/workspace/tatanka-dev/bin/tatanka}
ROOT=${ROOT:-/workspace/tatanka-dev/run/p4mesh}
SESSION=${SESSION:-tatanka-p4}
N=${N:-3}
PORT_BASE=${PORT_BASE:-13345}
port() { echo $((PORT_BASE + $1)); }
id() { cat "$ROOT/tatanka-$1/peerid"; }
addr() { echo "/ip4/127.0.0.1/tcp/$(port "$1")/p2p/$(id "$1")"; }
node_cmd() {
  local i=$1 flags="" j
  for j in $(seq 1 $N); do [ "$j" -ne "$i" ] && flags="$flags --bootstrap $(addr "$j")"; done
  echo "$TATANKA -C $ROOT/tatanka-$i/tatanka.conf$flags 2>&1 | tee -a $ROOT/tatanka-$i/console.log"
}
case "${1:-}" in
start)
  tmux has-session -t $SESSION 2>/dev/null && { echo "$SESSION already running"; exit 0; }
  # Mesh node data only (no wallet or swap state), moved aside anyway.
  [ -d "$ROOT" ] && mv "$ROOT" "$ROOT.$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$ROOT"
  ids=()
  for i in $(seq 1 $N); do
    d=$ROOT/tatanka-$i; mkdir -p "$d"
    "$TATANKA" init --appdata "$d" > "$d/peerid"
    ids+=("\"$(cat "$d/peerid")\"")
    printf 'appdata=%s\nlistenport=%d\nmetricsport=%d\nadminport=%d\n' "$d" "$(port "$i")" $((PORT_BASE + 10 + i)) $((PORT_BASE + 20 + i)) > "$d/tatanka.conf"
  done
  wl="[$(IFS=,; echo "${ids[*]}")]"
  for i in $(seq 1 $N); do echo "$wl" > "$ROOT/tatanka-$i/whitelist.json"; done
  tmux new-session -d -s $SESSION -n node-1 -x 200 -y 50
  for i in $(seq 1 $N); do
    [ "$i" -gt 1 ] && tmux new-window -t $SESSION -n node-$i
    tmux send-keys -t $SESSION:node-$i "$(node_cmd "$i")" C-m
  done
  sleep 3; echo "started $SESSION"
  ;;
stop) tmux kill-session -t $SESSION ;;
kill) tmux send-keys -t $SESSION:node-"$2" C-c ;;
restart) tmux send-keys -t $SESSION:node-"$2" "$(node_cmd "$2")" C-m ;;
addr) addr "$2" ;;
up) (exec 3<>/dev/tcp/127.0.0.1/"$(port "$2")") 2>/dev/null && echo up || echo down ;;
id) id "$2" ;;
*) echo "usage: $0 start|stop|kill N|restart N|addr N|id N"; exit 1 ;;
esac
