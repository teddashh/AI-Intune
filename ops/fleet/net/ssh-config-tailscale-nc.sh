#!/usr/bin/env bash
set -euo pipefail
name='' ip='' user='' identity=$HOME/.ssh/id_ed25519 socket=/var/run/tailscale/tailscaled.sock
jump='' config=$HOME/.ssh/config write=0 check='' rename=''
while (($#)); do
    case $1 in
        --name) name=${2:?}; shift 2;; --ip) ip=${2:?}; shift 2;; --user) user=${2:?}; shift 2;;
        --identity) identity=${2:?}; shift 2;; --socket) socket=${2:?}; shift 2;;
        --jump-fallback) jump=${2:?}; shift 2;; --config) config=${2:?}; shift 2;;
        --rename-existing) rename=${2:?}; shift 2;; --check) check=${2:?}; shift 2;;
        --write) write=1; shift;;
        *) echo 'Unknown option' >&2; exit 2;;
    esac
done
if [[ -n $check ]]; then
    [[ $check =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] || exit 2
    if ssh -F "$config" -o BatchMode=yes -o ConnectTimeout=10 "$check" true; then echo 'SSH: ok'; else echo 'SSH: failed' >&2; exit 1; fi
    exit
fi
python3 - "$name" "$ip" "$user" "$identity" "$socket" "$jump" "$config" "$write" "$rename" <<'PY'
import datetime,ipaddress,os,re,shlex,shutil,sys,tempfile
name,ip,user,identity,socket,jump,config,write,rename=sys.argv[1:]
for label,value in [('alias',name),('user',user),('jump',jump),('rename',rename)]:
    if (not value and label in ('alias','user')) or (value and not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9._-]*',value)):
        sys.exit('Invalid '+label)
try:
    if ipaddress.ip_address(ip) not in ipaddress.ip_network('100.64.0.0/10'): raise ValueError()
except ValueError: sys.exit('Use a Tailscale IPv4 address')
# ProxyCommand goes through a shell. Restrict paths rather than allow shell expansion.
for path in (identity,socket):
    if not re.fullmatch(r'/[a-zA-Z0-9_./-]+',path): sys.exit('Use absolute paths with simple characters')
start='# >>> fleet:'+name
end='# <<< fleet:'+name
block='\n'.join([start,'Host '+name,'    HostName '+ip,'    User '+user,
                  '    IdentityFile '+identity,'    IdentitiesOnly yes',
                  '    StrictHostKeyChecking accept-new',
                  '    ProxyCommand /usr/bin/tailscale --socket='+socket+' nc %h %p',end])+'\n'
if jump: print('# Fallback only: use ProxyJump '+jump+' in a separate Host block without ProxyCommand.')
if write!='1': print(block,end=''); sys.exit()
if os.path.islink(config): sys.exit('Refusing symlink config')
old=open(config).read() if os.path.exists(config) else ''
lines=old.splitlines(keepends=True)
begin=[i for i,l in enumerate(lines) if l.rstrip('\r\n')==start]
finish=[i for i,l in enumerate(lines) if l.rstrip('\r\n')==end]
if len(begin)!=len(finish) or len(begin)>1 or (begin and begin[0]>=finish[0]): sys.exit('Invalid fleet markers')
lo,hi=(begin[0],finish[0]) if begin else (-1,-1)
for i,line in enumerate(lines):
    if lo<=i<=hi: continue
    parts=shlex.split(line,comments=True)
    if parts and parts[0].lower()=='host' and name in parts[1:]:
        if not rename: sys.exit('Alias exists outside fleet markers; use --rename-existing OLD')
        if any(rename in shlex.split(l,comments=True)[1:] for l in lines if l.strip().lower().startswith('host ')):
            sys.exit('Rename target already exists')
        lines[i]='Host '+' '.join(rename if p==name else p for p in parts[1:])+'\n'
if begin: lines[lo:hi+1]=[block]
else:
    # Specific blocks precede Host * defaults: OpenSSH keeps the first value.
    lines.insert(0,block+'\n')
new=''.join(lines)
parent=os.path.dirname(os.path.abspath(config))
os.makedirs(parent,mode=0o700,exist_ok=True)
if new!=old:
    if os.path.exists(config):
        stamp=datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
        shutil.copy2(config,config+'.bak-'+stamp)
        os.chmod(config+'.bak-'+stamp,0o600)
    fd,tmp=tempfile.mkstemp(dir=parent)
    with os.fdopen(fd,'w') as f: f.write(new)
    os.replace(tmp,config)
os.chmod(config,0o600)
print('Updated SSH config: '+config)
PY
