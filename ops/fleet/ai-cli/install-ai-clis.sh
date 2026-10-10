#!/usr/bin/env bash
set -euo pipefail
apply=false
only=claude,codex,grok,agy
while [ "$#" -gt 0 ]; do
    case "$1" in
        --plan) apply=false; shift ;;
        --apply) apply=true; shift ;;
        --only) [ "$#" -ge 2 ] || exit 2; only=$2; shift 2 ;;
        *) printf 'usage: install-ai-clis.sh [--plan|--apply] [--only claude,codex,grok,agy]\n' >&2; exit 2 ;;
    esac
done
IFS=, read -r -a selected <<< "$only"
[ "${#selected[@]}" -gt 0 ] || exit 2
for cli in "${selected[@]}"; do
    case "$cli" in claude|codex|grok|agy) ;; *) printf 'unknown CLI\n' >&2; exit 2 ;; esac
done
root_step() {
    if ! "$apply"; then
        printf 'plan root: %s\n' "$1"
    elif [ "$(id -u)" -eq 0 ]; then
        bash -c "$1"
    else
        local quoted=${1//\'/\'\\\'\'}
        printf "sudo bash -c '%s'\n" "$quoted"
    fi
}
backup() { [ ! -e "$1" ] || cp -p -- "$1" "$1.bak-$(date -u +%Y%m%dT%H%M%S%NZ)"; }
if ! command -v node >/dev/null 2>&1; then
    root_step 'set -euo pipefail; export NEEDRESTART_MODE=l; curl -fsSL https://deb.nodesource.com/setup_22.x | bash; apt-get install -y nodejs'
fi
for cli in "${selected[@]}"; do
    case "$cli" in
        claude|codex)
            if ! command -v "$cli" >/dev/null 2>&1; then
                package=@anthropic-ai/claude-code
                [ "$cli" != codex ] || package=@openai/codex
                root_step "set -euo pipefail; export NEEDRESTART_MODE=l; npm install -g $package"
            fi ;;
        grok|agy)
            url=https://x.ai/cli/install.sh
            [ "$cli" != agy ] || url=https://antigravity.google/cli/install.sh
            if ! command -v "$cli" >/dev/null 2>&1; then
                if "$apply"; then curl -fsSL "$url" | bash; else printf 'plan user: install %s from %s\n' "$cli" "$url"; fi
            fi ;;
    esac
done
if "$apply"; then
    mkdir -p "$HOME/.local/bin"
    if [[ ,$only, == *,claude,* ]]; then
        mkdir -p "$HOME/.config/claude-automation"
        chmod 700 "$HOME/.config/claude-automation"
        source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
        target=$HOME/.local/bin/claude-automation
        if ! cmp -s "$source_dir/claude-automation" "$target"; then
            backup "$target"
            install -m 755 "$source_dir/claude-automation" "$target"
        fi
    fi
    if [[ ,$only, == *,grok,* ]] && [ -x "$HOME/.grok/bin/grok" ]; then
        target=$HOME/.local/bin/grok
        if [ "$(readlink "$target" 2>/dev/null || true)" != "$HOME/.grok/bin/grok" ]; then
            backup "$target"
            ln -sfn "$HOME/.grok/bin/grok" "$target"
        fi
    fi
    # The agy installer owns its binary location. Link known user locations.
    if [[ ,$only, == *,agy,* ]]; then
        for candidate in "$HOME/.antigravity/bin/agy" "$HOME/.agy/bin/agy"; do
            if [ -x "$candidate" ] && [ "$(readlink "$HOME/.local/bin/agy" 2>/dev/null || true)" != "$candidate" ]; then
                backup "$HOME/.local/bin/agy"
                ln -sfn "$candidate" "$HOME/.local/bin/agy"
                break
            fi
        done
    fi
    if ! grep -q '^# fleet-ai-cli PATH begin$' "$HOME/.bashrc" 2>/dev/null; then
        backup "$HOME/.bashrc"
        tmp=$(mktemp)
        printf '%s\n' '# fleet-ai-cli PATH begin' 'export PATH="$HOME/.local/bin:$HOME/.grok/bin:$PATH"' '# fleet-ai-cli PATH end' > "$tmp"
        [ ! -f "$HOME/.bashrc" ] || cat "$HOME/.bashrc" >> "$tmp"
        cat "$tmp" > "$HOME/.bashrc"
        rm -f "$tmp"
    fi
else
    printf 'plan user: install wrapper, create private token directory; symlink user CLIs; prepend marked PATH block to ~/.bashrc (backup first)\n'
fi
# Pass HOME as a quoted literal; never source /etc/environment.
export FLEET_AI_HOME=$HOME
environment_step=$(python3 - <<'PY'
import os, shlex
home = os.environ['FLEET_AI_HOME']
if '\n' in home or '\r' in home: raise SystemExit('invalid HOME')
code = '''import pathlib,re,shutil,datetime
p=pathlib.Path('/etc/environment')
old=p.read_text() if p.exists() else ''
m=re.search(r'^PATH=(.*)$',old,re.M)
path=m[1].strip(chr(34)+chr(39)) if m else '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
dirs=HOME_DIRS
newpath=':'.join(dirs+[x for x in path.split(':') if x not in dirs])
line='PATH='+chr(34)+newpath+chr(34)
new=old[:m.start()]+line+old[m.end():] if m else old+('' if not old or old.endswith(chr(10)) else chr(10))+line+chr(10)
if new!=old:
 if p.exists(): shutil.copy2(p,str(p)+'.bak-'+datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ'))
 p.write_text(new)
'''.replace('HOME_DIRS', repr([home+'/.local/bin', home+'/.grok/bin']))
# One physical line, including the Python program.
print('python3 -c '+shlex.quote('exec('+repr(code)+')'))
PY
)
root_step "$environment_step"
printf 'Human next steps (selected CLIs only):\n'
for cli in "${selected[@]}"; do
    case "$cli" in
        claude) printf 'claude setup-token; save privately to ~/.config/claude-automation/oauth-token; chmod 600 that file. Never put tokens in rc files.\n' ;;
        codex) printf 'codex login --device-auth; codex login status\n' ;;
        grok) printf 'grok: complete interactive login; chmod 600 ~/.grok/auth.json\n' ;;
        agy) printf 'agy: complete interactive login; check with agy -p after login\n' ;;
    esac
done
