#!/bin/sh
# crewlet-gate 2: the only program a Crewlet certificate runs on this server.
set -eu
set -f
GATE_VERSION=2
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
# path or its base name; where the rule runs a program through "sudo -n", the
# program's absolute path is matched by the path or its bare name too, a name
# with no "/", which sudo resolves through its own secure_path and checks
# against the sudoers line naming that path; "*" is any one word not starting
# with "-"; a word ending in "=*" is that prefix and any value; a final "..."
# is any further words. Every other word matches exactly.
matches() {
  rule=$1
  shift
  pos=0
  sudo=0
  for word in $rule; do
    if [ "$word" = "..." ]; then
      return 0
    fi
    [ $# -gt 0 ] || return 1
    if [ "$pos" = 0 ]; then
      [ "$1" = "$word" ] || [ "$1" = "${word##*/}" ] || return 1
      [ "${word##*/}" = sudo ] && sudo=1
    elif [ "$pos" = 1 ] && [ "$sudo" = 1 ] && [ "$word" = "-n" ]; then
      [ "$1" = "-n" ] || return 1
      sudo=2
    elif [ "$pos" = 2 ] && [ "$sudo" = 2 ] && [ "${word#/}" != "$word" ]; then
      case "$1" in
        */*) [ "$1" = "$word" ] || return 1 ;;
        *) [ "$1" = "${word##*/}" ] || return 1 ;;
      esac
    elif [ "$word" = "*" ]; then
      case "$1" in -*) return 1 ;; esac
    elif [ "${word%=\*}" != "$word" ]; then
      case "$1" in "${word%\*}"*) ;; *) return 1 ;; esac
    elif [ "$word" != "$1" ]; then
      return 1
    fi
    pos=$((pos + 1))
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
