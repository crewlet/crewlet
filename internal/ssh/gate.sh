#!/bin/sh
# crewlet-gate 1: the only program a Crewlet certificate runs on this server.
set -eu
set -f
GATE_VERSION=1
ALLOWLIST=/etc/ssh/crewlet/read-allowlist

say() { logger -t crewlet-gate -p authpriv.notice -- "$(printf '%s' "$*" | tr '\n\r' '  ')" 2>/dev/null || :; }

key_id() {
  if [ -n "${SSH_USER_AUTH:-}" ] && [ -r "$SSH_USER_AUTH" ]; then
    sed -n 's/^publickey //p' "$SSH_USER_AUTH" | head -n 1 |
      ssh-keygen -L -f /dev/stdin 2>/dev/null |
      sed -n 's/^[[:space:]]*Key ID: "\(.*\)"$/\1/p'
  fi
}

# matches RULE ARG...: RULE's first word is an absolute path, matched by the
# path or its base name; "*" is any one word not starting with "-"; a word
# ending in "=*" is that prefix and any value; a final "..." is any further words.
matches() {
  rule=$1
  shift
  first=1
  for word in $rule; do
    if [ "$word" = "..." ]; then
      return 0
    fi
    [ $# -gt 0 ] || return 1
    if [ "$first" = 1 ]; then
      [ "$1" = "$word" ] || [ "$1" = "${word##*/}" ] || return 1
      first=0
    elif [ "$word" = "*" ]; then
      case "$1" in -*) return 1 ;; esac
    elif [ "${word%=\*}" != "$word" ]; then
      case "$1" in "${word%\*}"*) ;; *) return 1 ;; esac
    elif [ "$word" != "$1" ]; then
      return 1
    fi
    shift
  done
  [ $# -eq 0 ]
}

refuse() {
  say "refused id=$(key_id) cmd=$1"
  echo "crewlet-gate: refused: $2" >&2
  exit 126
}

mode=${1:-}
case "$mode" in
  version)
    echo "crewlet-gate $GATE_VERSION"
    ;;
  read)
    cmd=${SSH_ORIGINAL_COMMAND:-}
    case "$cmd" in
      '') refuse "" "no interactive shell; name one allowlisted command" ;;
      'crewlet-gate version') echo "crewlet-gate $GATE_VERSION"; exit 0 ;;
      *[!A-Za-z0-9\ _./:=,@%+-]*) refuse "$cmd" "write the command without quotes or shell characters" ;;
    esac
    # shellcheck disable=SC2086 # word splitting on spaces is the point; globbing is off
    set -- $cmd
    while IFS= read -r rule || [ -n "$rule" ]; do
      case "$rule" in '' | '#'*) continue ;; esac
      if matches "$rule" "$@"; then
        bin=${rule%%[[:space:]]*}
        shift
        say "read id=$(key_id) cmd=$cmd"
        exec "$bin" "$@"
      fi
    done <"$ALLOWLIST"
    refuse "$cmd" "not on this server's read allowlist"
    ;;
  operate)
    [ $# -eq 2 ] || refuse "" "malformed operate command"
    case "$2" in '' | *[!A-Za-z0-9+/=]*) refuse "" "malformed operate command" ;; esac
    cmd=$(printf '%s' "$2" | base64 -d 2>/dev/null) || refuse "" "malformed operate command"
    [ -n "$cmd" ] || refuse "" "malformed operate command"
    say "operate id=$(key_id) cmd_b64=$2"
    exec /bin/sh -c "$cmd"
    ;;
  *)
    refuse "" "unknown mode"
    ;;
esac
