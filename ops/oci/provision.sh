#!/bin/bash
# Idempotent Always Free-minded OCI provisioner for one Autopilot Hub VM.
# Creates only resources named clawctl-* and never prints API keys.
set -euo pipefail

name="clawctl-hub"
region=""
compartment=""
ocpus=2
memory=12
os_version="22.04"
boot_gb=50
ssh_cidr="0.0.0.0/0"
pubkey=""
domain=""
git_ref="main"
repo="https://github.com/teddashh/AI-Intune.git"
vcn_cidr="10.50.0.0/16"
interval=60
max_wait=1800
allow_paid=0
dry_run=0
destroy=0
render_path=""
no_wait=0
ssh_user="ubuntu"
oci_bin="${OCI_BIN:-oci}"
FREE_OCPU=4
FREE_MEM=24
FREE_BOOT=200

fail() { echo "provision: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name|--region|--compartment-id|--ocpus|--memory-gb|--os-version|--boot-volume-gb|--ssh-cidr|--ssh-public-key|--domain|--git-ref|--repo|--vcn-cidr|--capacity-interval|--capacity-max-wait|--ssh-user|--render-user-data)
      [[ $# -ge 2 ]] || fail "missing flag value"
      case "$1" in
        --name) name=$2 ;;
        --region) region=$2 ;;
        --compartment-id) compartment=$2 ;;
        --ocpus) ocpus=$2 ;;
        --memory-gb) memory=$2 ;;
        --os-version) os_version=$2 ;;
        --boot-volume-gb) boot_gb=$2 ;;
        --ssh-cidr) ssh_cidr=$2 ;;
        --ssh-public-key) pubkey=$2 ;;
        --domain) domain=$2 ;;
        --git-ref) git_ref=$2 ;;
        --repo) repo=$2 ;;
        --vcn-cidr) vcn_cidr=$2 ;;
        --capacity-interval) interval=$2 ;;
        --capacity-max-wait) max_wait=$2 ;;
        --ssh-user) ssh_user=$2 ;;
        --render-user-data) render_path=$2 ;;
      esac
      shift 2 ;;
    --free-max) ocpus=4; memory=24; shift ;;
    --allow-paid) allow_paid=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    --destroy) destroy=1; shift ;;
    --no-wait) no_wait=1; shift ;;
    -h|--help)
      echo "Usage: ops/oci/provision.sh [--dry-run] [--destroy] [--name clawctl-hub] [--os-version 22.04|24.04] [--free-max] [--domain host]"
      exit 0 ;;
    *) fail "unknown flag" ;;
  esac
done

[[ "$name" =~ ^clawctl-[a-z0-9-]{1,32}$ ]] || fail "--name must match clawctl-[a-z0-9-]{1,32} so unrelated instances cannot be selected"
[[ "$name" != *- ]] || fail "--name must not end with a hyphen"
[[ "$name" != *--* ]] || fail "--name must not contain a double hyphen"
[[ "$os_version" == "22.04" || "$os_version" == "24.04" ]] || fail "--os-version must be 22.04 or 24.04"
[[ "$ocpus" =~ ^[0-9]+$ && "$ocpus" -ge 1 ]] || fail "--ocpus must be a positive integer"
[[ "$memory" =~ ^[0-9]+$ && "$memory" -ge 1 ]] || fail "--memory-gb must be a positive integer"
[[ "$boot_gb" =~ ^[0-9]+$ && "$boot_gb" -ge 50 ]] || fail "--boot-volume-gb must be an integer >= 50"
[[ "$interval" =~ ^[0-9]+$ ]] || fail "--capacity-interval must be a non-negative integer"
[[ "$max_wait" =~ ^[0-9]+$ ]] || fail "--capacity-max-wait must be a non-negative integer"
[[ "$git_ref" =~ ^[A-Za-z0-9._/-]+$ && "$git_ref" != *..* ]] || fail "invalid --git-ref"
[[ "$repo" =~ ^https://[A-Za-z0-9._/-]+$ ]] || fail "--repo must be an https URL without credentials"
[[ "$ssh_user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || fail "invalid --ssh-user"
if [[ -n "$domain" ]]; then
  [[ "$domain" =~ ^[A-Za-z0-9.-]+$ && "$domain" != *..* ]] || fail "invalid --domain"
fi
if [[ -n "$region" ]]; then
  [[ "$region" =~ ^[a-z0-9-]+$ ]] || fail "invalid --region"
fi
if [[ -n "$compartment" ]]; then
  [[ "$compartment" =~ ^ocid1\.[A-Za-z0-9._-]+$ ]] || fail "invalid --compartment-id"
fi
python3 - "$ssh_cidr" "$vcn_cidr" <<'PY' || fail "invalid --ssh-cidr or --vcn-cidr (VCN must be an IPv4 /16)"
import ipaddress, sys
ssh, vcn = sys.argv[1:]
ipaddress.ip_network(ssh, strict=False)
net = ipaddress.ip_network(vcn, strict=True)
if net.version != 4 or net.prefixlen != 16:
    sys.exit(1)
PY

root=$(cd "$(dirname "$0")/../.." && pwd)
setup="$root/ops/oci/host-setup.sh"
[[ -f "$setup" ]] || fail "missing $setup"

render_user_data() {
  local dest=$1 public_host=$2
  python3 - "$setup" "$dest" "$public_host" "$git_ref" "$repo" <<'PY'
import base64, json, pathlib, sys
script, dest, public_host, git_ref, repo = sys.argv[1:]
payload = base64.b64encode(pathlib.Path(script).read_bytes()).decode("ascii")
text = """#cloud-config
# Install Docker, open 80/443 in front of the Ubuntu INPUT REJECT, start the Hub.
package_update: true
packages:
  - ca-certificates
  - curl
  - git
  - python3
debconf_selections: |
  iptables-persistent iptables-persistent/autosave_v4 boolean true
  iptables-persistent iptables-persistent/autosave_v6 boolean true
write_files:
  - path: /var/lib/clawctl/host-setup.sh
    permissions: "0755"
    encoding: b64
    content: |
      %s
runcmd:
  - %s
""" % (payload, json.dumps(["bash", "/var/lib/clawctl/host-setup.sh", "--public-host", public_host, "--git-ref", git_ref, "--repo", repo]))
pathlib.Path(dest).write_text(text)
PY
}

cloud_public_host="auto"
if [[ -n "$domain" ]]; then
  cloud_public_host=$domain
fi

if [[ -n "$render_path" ]]; then
  [[ "$destroy" == 0 ]] || fail "--render-user-data cannot be combined with --destroy"
  render_user_data "$render_path" "$cloud_public_host"
  echo "Wrote $render_path"
  exit 0
fi

command -v python3 >/dev/null || fail "python3 not found on PATH"
[[ -x "$oci_bin" || -n "$(command -v "$oci_bin" || true)" ]] || fail "oci CLI not found on PATH"

if [[ "$ssh_cidr" == "0.0.0.0/0" ]]; then
  echo "WARNING: SSH port 22 is open to 0.0.0.0/0. Authentication is key-only. Prefer --ssh-cidr <your-ip>/32." >&2
fi
if [[ -z "$domain" ]]; then
  echo "WARNING: no --domain was given. The VM will use <public-ipv4-dashed>.sslip.io. Choose a real domain before enrolling machines." >&2
fi

shape_note="VM.Standard.A1.Flex ${ocpus} OCPU / ${memory} GB, boot ${boot_gb} GB, Ubuntu ${os_version} aarch64"
if (( ocpus > FREE_OCPU || memory > FREE_MEM || boot_gb > FREE_BOOT )); then
  over_shape=1
else
  over_shape=0
fi
if [[ "$over_shape" == 1 && "$allow_paid" == 0 && "$dry_run" == 0 && "$destroy" == 0 ]]; then
  fail "requested shape or boot volume exceeds Always Free (A1 total ${FREE_OCPU} OCPU and ${FREE_MEM} GB, ${FREE_BOOT} GB block storage). Refusing. Re-run with --allow-paid only after confirming charges."
fi

work=$(mktemp -d)
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT
OCI_LAST_ERR=""

show_err() {
  [[ -n "$OCI_LAST_ERR" && -f "$OCI_LAST_ERR" ]] || return 0
  python3 - "$OCI_LAST_ERR" <<'PY'
import sys
for line in open(sys.argv[1], errors="replace"):
    low = line.lower()
    if "private key" in low or "begin openssh" in low or "begin rsa" in low or "key_file" in low:
        print("[redacted]", file=sys.stderr)
        continue
    if len(line) > 400:
        print("[redacted long line]", file=sys.stderr)
        continue
    sys.stderr.write(line)
PY
}

run_oci() {
  local dest=$1
  shift
  local err rc
  err=$(mktemp)
  set +e
  "$oci_bin" --region "$region" "$@" >"$dest" 2>"$err"
  rc=$?
  set -e
  if [[ "$rc" -eq 0 ]]; then
    if [[ ! -s "$dest" ]]; then
      printf '%s\n' '{"data":[]}' >"$dest"
    fi
    rm -f "$err"
    return 0
  fi
  OCI_LAST_ERR=$err
  return "$rc"
}

load_config() {
  python3 - <<'PY'
import configparser, os, sys
path = os.environ.get("OCI_CLI_CONFIG_FILE", os.path.expanduser("~/.oci/config"))
if not os.path.isfile(path):
    sys.exit(2)
cfg = configparser.ConfigParser()
cfg.read(path)
profile = os.environ.get("OCI_CLI_PROFILE", "DEFAULT")
sec = cfg[profile] if profile in cfg else cfg["DEFAULT"]
tenancy = (sec.get("tenancy") or "").strip()
region = (sec.get("region") or "").strip()
key = (sec.get("key_file") or "").strip()
if not tenancy or not region or not key:
    sys.exit(3)
if not os.path.isfile(os.path.expanduser(key)):
    sys.exit(4)
sys.stdout.write(tenancy + "\n" + region + "\n")
PY
}

if [[ -z "$region" || -z "$compartment" ]]; then
  config_out=$(load_config) || {
    rc=$?
    if [[ "$dry_run" == 1 ]]; then
      echo "WARNING: OCI config is missing or incomplete (status $rc). See docs/DEPLOY-OCI.md. No resources were created." >&2
      echo "Plan: $shape_note"
      echo "Plan: VCN $vcn_cidr, IGW, route 0.0.0.0/0, security list 22 from $ssh_cidr plus 80 and 443, public subnet, one instance."
      if [[ "$destroy" == 1 ]]; then
        echo "Plan: require the typed name $name, then delete only that stack."
      fi
      echo "No resources were created."
      exit 0
    fi
    fail "OCI config is missing or incomplete. See docs/DEPLOY-OCI.md. Generate a key with the CLI and paste only the public half into the console."
  }
  cfg_tenancy=${config_out%%$'\n'*}
  cfg_region=${config_out#*$'\n'}
  [[ -n "$compartment" ]] || compartment=$cfg_tenancy
  [[ -n "$region" ]] || region=$cfg_region
fi

echo "Region: $region"
echo "Shape: $shape_note"

if ! run_oci "$work/ads.json" iam availability-domain list -c "$compartment" --all; then
  show_err
  if [[ "$dry_run" == 1 ]]; then
    echo "WARNING: OCI preflight failed. No resources were created." >&2
    echo "No resources were created."
    exit 0
  fi
  fail "OCI preflight failed"
fi

python3 - "$work/ads.json" > "$work/ads" <<'PY'
import json, sys
doc = json.loads(open(sys.argv[1]).read())
data = doc.get("data") or []
names = []
for item in data:
    name = item.get("name") or ""
    if name:
        names.append(name)
def rank(name):
    if name.endswith("-1"):
        return (0, name)
    if name.endswith("-3"):
        return (1, name)
    if name.endswith("-2"):
        return (2, name)
    return (9, name)
for name in sorted(names, key=rank):
    print(name)
PY
mapfile -t ads < "$work/ads"
[[ ${#ads[@]} -gt 0 ]] || fail "no availability domains returned"
echo "Availability domains: ${ads[*]}"

if ! run_oci "$work/images.json" compute image list -c "$compartment" \
  --operating-system "Canonical Ubuntu" --operating-system-version "$os_version" \
  --shape "VM.Standard.A1.Flex" --sort-by TIMECREATED --sort-order DESC --all; then
  show_err
  fail "image lookup failed"
fi
image_line=$(python3 - "$work/images.json" <<'PY'
import json, sys
doc = json.loads(open(sys.argv[1]).read())
for item in doc.get("data") or []:
    display = item.get("display-name") or ""
    low = display.lower()
    if "aarch64" in low and "amd64" not in low:
        print(item.get("id", ""))
        print(display)
        break
PY
)
image_id=$(printf '%s\n' "$image_line" | head -n 1)
image_name=$(printf '%s\n' "$image_line" | sed -n '2p')
[[ -n "$image_id" && -n "$image_name" ]] || fail "no Ubuntu $os_version aarch64 image for VM.Standard.A1.Flex"
echo "Image: $image_name"

if ! run_oci "$work/instances.json" compute instance list -c "$compartment" --all; then
  show_err
  fail "instance list failed"
fi
boot_index=0
: > "$work/boots.ndjson"
for ad in "${ads[@]}"; do
  boot_index=$((boot_index + 1))
  if ! run_oci "$work/boot-$boot_index.json" bv boot-volume list -c "$compartment" --availability-domain "$ad" --all; then
    show_err
    if [[ "$allow_paid" == 0 && "$dry_run" == 0 && "$destroy" == 0 ]]; then
      fail "could not list boot volumes; refusing to guess Always Free usage"
    fi
    echo "WARNING: boot volume list failed for one availability domain." >&2
    continue
  fi
  cat "$work/boot-$boot_index.json" >> "$work/boots.ndjson"
  printf '\n' >> "$work/boots.ndjson"
done
if ! run_oci "$work/volumes.json" bv volume list -c "$compartment" --all; then
  show_err
  if [[ "$allow_paid" == 0 && "$dry_run" == 0 && "$destroy" == 0 ]]; then
    fail "could not list block volumes; refusing to guess Always Free usage"
  fi
  echo "WARNING: block volume list failed." >&2
  printf '%s\n' '{"data":[]}' > "$work/volumes.json"
fi

quota=$(python3 - "$work/instances.json" "$work/boots.ndjson" "$work/volumes.json" "$name" "$ocpus" "$memory" "$boot_gb" <<'PY'
import json, sys
inst_path, boot_path, vol_path, name, ocpus, memory, boot = sys.argv[1:]
ocpus, memory, boot = int(ocpus), int(memory), int(boot)
instances = json.loads(open(inst_path).read()).get("data") or []
ours = None
used_o = 0.0
used_m = 0.0
for item in instances:
    state = item.get("lifecycle-state") or ""
    if state in ("TERMINATED", "TERMINATING"):
        continue
    if item.get("display-name") == name:
        ours = state
        continue
    if item.get("shape") != "VM.Standard.A1.Flex":
        continue
    cfg = item.get("shape-config") or {}
    used_o += float(cfg.get("ocpus") or 0)
    used_m += float(cfg.get("memory-in-gbs") or 0)
used_b = 0.0
def add_sizes(raw):
    global used_b
    raw = raw.strip()
    if not raw:
        return
    # One or more concatenated JSON documents.
    decoder = json.JSONDecoder()
    idx = 0
    while idx < len(raw):
        while idx < len(raw) and raw[idx].isspace():
            idx += 1
        if idx >= len(raw):
            break
        doc, end = decoder.raw_decode(raw, idx)
        idx = end
        for item in doc.get("data") or []:
            state = item.get("lifecycle-state") or ""
            if state in ("TERMINATED", "TERMINATING"):
                continue
            used_b += float(item.get("size-in-gbs") or 0)
add_sizes(open(boot_path).read())
add_sizes(open(vol_path).read())
launch = ours is None
delta_o = ocpus if launch else 0
delta_m = memory if launch else 0
delta_b = boot if launch else 0
free_o, free_m, free_b = 4, 24, 200
exceeds = 0
reason = "fits Always Free"
if ocpus > free_o or memory > free_m or boot > free_b:
    exceeds = 1
    reason = "request itself exceeds Always Free limits"
elif launch and (used_o + delta_o > free_o or used_m + delta_m > free_m or used_b + delta_b > free_b):
    exceeds = 1
    reason = "existing A1 usage plus this VM would exceed Always Free"
print(f"{int(launch)} {exceeds} {used_o:g} {used_m:g} {used_b:g}")
print(reason)
if ours:
    print(ours)
PY
)
quota_launch=$(printf '%s\n' "$quota" | awk 'NR==1 {print $1}')
quota_exceeds=$(printf '%s\n' "$quota" | awk 'NR==1 {print $2}')
quota_used=$(printf '%s\n' "$quota" | awk 'NR==1 {print "A1 in use " $3 " OCPU / " $4 " GB, block " $5 " GB"}')
quota_reason=$(printf '%s\n' "$quota" | sed -n '2p')
our_state=$(printf '%s\n' "$quota" | sed -n '3p')
echo "Quota: $quota_used; $quota_reason"
if [[ "$quota_exceeds" == 1 ]]; then
  echo "WARNING: this shape or boot volume would exceed Always Free (A1 total ${FREE_OCPU} OCPU and ${FREE_MEM} GB, ${FREE_BOOT} GB block storage). A real run refuses unless --allow-paid is set after you confirm charges." >&2
fi
if [[ "$over_shape" == 1 ]]; then
  echo "WARNING: the requested size itself is above the Always Free ceiling." >&2
fi

if [[ "$dry_run" == 1 ]]; then
  if [[ "$destroy" == 1 ]]; then
    echo "Plan: require the typed name $name, then terminate its instance, delete its subnet, security list, internet gateway, and VCN. Nothing else is selected."
  else
    echo "Plan: create only missing ${name}-vcn ${name}-igw ${name}-seclist ${name}-subnet and one ${name} instance."
    echo "Plan: security list allows TCP 22 from $ssh_cidr, TCP 80 and 443 from 0.0.0.0/0, and ICMP type 3 code 4."
    echo "Plan: on Out of host capacity, try the next availability domain, then sleep ${interval}s until ${max_wait}s."
    echo "Plan: cloud-init installs Docker and the Hub pack with CLAWCTL_AUTH_MODE=local."
  fi
  echo "No resources were created."
  exit 0
fi

if [[ "$destroy" == 0 && "$quota_exceeds" == 1 && "$allow_paid" == 0 ]]; then
  fail "refusing to create resources that would exceed Always Free. Re-run with --allow-paid only after confirming charges."
fi
if [[ "$destroy" == 0 && "$allow_paid" == 1 && ( "$quota_exceeds" == 1 || "$over_shape" == 1 ) ]]; then
  echo "WARNING: --allow-paid is set. This can spend money outside Always Free." >&2
fi

list_id() {
  local file=$1 want=$2
  python3 - "$file" "$want" <<'PY'
import json, sys
want = sys.argv[2]
raw = open(sys.argv[1]).read().strip()
if not raw:
    sys.exit(0)
doc = json.loads(raw)
data = doc.get("data", doc)
if isinstance(data, dict):
    data = [data]
matches = [item.get("id", "") for item in data if item.get("display-name") == want and item.get("id")]
if len(matches) > 1:
    sys.exit(2)
if matches:
    print(matches[0])
PY
}

resource_name() {
  python3 - "$1" <<'PY'
import json, sys
doc = json.loads(open(sys.argv[1]).read())
data = doc.get("data", doc)
print(data.get("display-name", ""))
PY
}

lifecycle() {
  python3 - "$1" <<'PY'
import json, sys
doc = json.loads(open(sys.argv[1]).read())
data = doc.get("data", doc)
print(data.get("lifecycle-state", ""))
PY
}

created_id() {
  python3 - "$1" <<'PY'
import json, sys
doc = json.loads(open(sys.argv[1]).read())
print(doc["data"]["id"])
PY
}

wait_gone() {
  local tries=0 state
  while (( tries < 60 )); do
    if ! run_oci "$work/gone.json" "$@"; then
      rm -f "${OCI_LAST_ERR:-}"
      OCI_LAST_ERR=""
      return 0
    fi
    state=$(lifecycle "$work/gone.json")
    if [[ "$state" == "TERMINATED" ]]; then
      return 0
    fi
    tries=$((tries + 1))
    sleep 5
  done
  fail "timed out waiting for a delete to finish"
}

confirm_destroy() {
  local confirmation=""
  printf 'Type the name to destroy %s: ' "$name" >&2
  IFS= read -r confirmation || [[ -n "$confirmation" ]] || fail "confirmation required"
  [[ "$confirmation" == "$name" ]] || fail "confirmation did not match; nothing deleted"
}

do_destroy() {
  confirm_destroy
  run_oci "$work/instances.json" compute instance list -c "$compartment" --all || { show_err; fail "instance list failed"; }
  mapfile -t instance_ids < <(python3 - "$work/instances.json" "$name" <<'PY'
import json, sys
name = sys.argv[2]
doc = json.loads(open(sys.argv[1]).read())
for item in doc.get("data") or []:
    state = item.get("lifecycle-state") or ""
    if item.get("display-name") == name and state not in ("TERMINATED",):
        print(item["id"])
PY
)
  local id
  for id in "${instance_ids[@]}"; do
    [[ -n "$id" ]] || continue
    run_oci "$work/iget.json" compute instance get --instance-id "$id" || { show_err; fail "instance get failed"; }
    [[ "$(resource_name "$work/iget.json")" == "$name" ]] || fail "refusing to terminate an instance whose display name does not match"
    run_oci "$work/term.json" compute instance terminate --instance-id "$id" --preserve-boot-volume false --force || { show_err; fail "instance terminate failed"; }
    wait_gone compute instance get --instance-id "$id"
  done
  run_oci "$work/vcns.json" network vcn list -c "$compartment" --all || { show_err; fail "vcn list failed"; }
  vcn_id=$(list_id "$work/vcns.json" "${name}-vcn") || fail "ambiguous VCN name"
  if [[ -n "$vcn_id" ]]; then
    run_oci "$work/subnets.json" network subnet list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "subnet list failed"; }
    subnet_id=$(list_id "$work/subnets.json" "${name}-subnet") || fail "ambiguous subnet name"
    if [[ -n "$subnet_id" ]]; then
      run_oci "$work/sget.json" network subnet get --subnet-id "$subnet_id" || { show_err; fail "subnet get failed"; }
      [[ "$(resource_name "$work/sget.json")" == "${name}-subnet" ]] || fail "refusing to delete a subnet whose display name does not match"
      run_oci "$work/sdel.json" network subnet delete --subnet-id "$subnet_id" --force || { show_err; fail "subnet delete failed"; }
      wait_gone network subnet get --subnet-id "$subnet_id"
    fi
    run_oci "$work/seclists.json" network security-list list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "security list list failed"; }
    seclist_id=$(list_id "$work/seclists.json" "${name}-seclist") || fail "ambiguous security list name"
    if [[ -n "$seclist_id" ]]; then
      run_oci "$work/slget.json" network security-list get --security-list-id "$seclist_id" || { show_err; fail "security list get failed"; }
      [[ "$(resource_name "$work/slget.json")" == "${name}-seclist" ]] || fail "refusing to delete a security list whose display name does not match"
      run_oci "$work/sldel.json" network security-list delete --security-list-id "$seclist_id" --force || { show_err; fail "security list delete failed"; }
      wait_gone network security-list get --security-list-id "$seclist_id"
    fi
    run_oci "$work/vcnget.json" network vcn get --vcn-id "$vcn_id" || { show_err; fail "vcn get failed"; }
    [[ "$(resource_name "$work/vcnget.json")" == "${name}-vcn" ]] || fail "refusing to delete a VCN whose display name does not match"
    rt_id=$(python3 - "$work/vcnget.json" <<'PY'
import json, sys
print(json.loads(open(sys.argv[1]).read())["data"]["default-route-table-id"])
PY
)
    run_oci "$work/rtupd.json" network route-table update --rt-id "$rt_id" --route-rules '[]' --force || { show_err; fail "route table update failed"; }
    run_oci "$work/igws.json" network internet-gateway list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "igw list failed"; }
    igw_id=$(list_id "$work/igws.json" "${name}-igw") || fail "ambiguous internet gateway name"
    if [[ -n "$igw_id" ]]; then
      run_oci "$work/igdel.json" network internet-gateway delete --ig-id "$igw_id" --force || { show_err; fail "internet gateway delete failed"; }
      wait_gone network internet-gateway get --ig-id "$igw_id"
    fi
    run_oci "$work/vdel.json" network vcn delete --vcn-id "$vcn_id" --force || { show_err; fail "vcn delete failed"; }
    wait_gone network vcn get --vcn-id "$vcn_id"
  fi
  echo "Destroyed $name and its VCN resources. Unrelated resources were not selected."
}

if [[ "$destroy" == 1 ]]; then
  do_destroy
  exit 0
fi

if [[ -z "$pubkey" ]]; then
  for candidate in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_rsa.pub"; do
    if [[ -f "$candidate" ]]; then
      pubkey=$candidate
      break
    fi
  done
fi
[[ -n "$pubkey" && -f "$pubkey" ]] || fail "no SSH public key; pass --ssh-public-key or create one with ssh-keygen -t ed25519"
echo "SSH public key file: $pubkey"

security_json() {
  python3 - "$ssh_cidr" <<'PY'
import json, sys
cidr = sys.argv[1]
def tcp(source, port):
    return {
        "source": source,
        "protocol": "6",
        "isStateless": False,
        "tcpOptions": {"destinationPortRange": {"min": port, "max": port}},
    }
ingress = [
    tcp(cidr, 22),
    tcp("0.0.0.0/0", 80),
    tcp("0.0.0.0/0", 443),
    {"source": "0.0.0.0/0", "protocol": "1", "isStateless": False, "icmpOptions": {"type": 3, "code": 4}},
]
egress = [{"destination": "0.0.0.0/0", "protocol": "all", "isStateless": False}]
print(json.dumps(ingress))
print(json.dumps(egress))
PY
}
mapfile -t sec_lines < <(security_json)
ingress_json=${sec_lines[0]}
egress_json=${sec_lines[1]}

rules_ok() {
  python3 - "$1" "$ssh_cidr" <<'PY'
import json, sys
path, cidr = sys.argv[1:]
doc = json.loads(open(path).read())
data = doc.get("data", doc)
rules = data.get("ingress-security-rules") or data.get("ingressSecurityRules") or []
found = set()
for rule in rules:
    proto = str(rule.get("protocol"))
    tcp = rule.get("tcp-options") or rule.get("tcpOptions") or {}
    dest = tcp.get("destination-port-range") or tcp.get("destinationPortRange") or {}
    if proto == "6" and dest.get("min") == dest.get("max") and dest.get("min") is not None:
        found.add((rule.get("source"), int(dest["min"])))
need = {(cidr, 22), ("0.0.0.0/0", 80), ("0.0.0.0/0", 443)}
sys.exit(0 if need <= found else 1)
PY
}

route_ok() {
  python3 - "$1" "$2" <<'PY'
import json, sys
path, igw = sys.argv[1:]
doc = json.loads(open(path).read())
data = doc.get("data", doc)
for rule in data.get("route-rules") or []:
    dest = rule.get("destination") or rule.get("cidrBlock") or rule.get("cidr-block")
    entity = rule.get("network-entity-id") or rule.get("networkEntityId")
    if dest == "0.0.0.0/0" and entity == igw:
        sys.exit(0)
sys.exit(1)
PY
}

cleanup_capacity() {
  run_oci "$work/cap-instances.json" compute instance list -c "$compartment" --all || return 0
  mapfile -t stale < <(python3 - "$work/cap-instances.json" "$name" <<'PY'
import json, sys
name = sys.argv[2]
doc = json.loads(open(sys.argv[1]).read())
for item in doc.get("data") or []:
    if item.get("display-name") == name and item.get("lifecycle-state") in ("PROVISIONING", "STARTING"):
        print(item["id"])
PY
)
  local id
  for id in "${stale[@]}"; do
    [[ -n "$id" ]] || continue
    run_oci "$work/cap-get.json" compute instance get --instance-id "$id" || continue
    [[ "$(resource_name "$work/cap-get.json")" == "$name" ]] || fail "refusing to terminate an instance whose display name does not match"
    run_oci "$work/cap-term.json" compute instance terminate --instance-id "$id" --preserve-boot-volume false --force || true
  done
}

ensure_network() {
  run_oci "$work/vcns.json" network vcn list -c "$compartment" --all || { show_err; fail "vcn list failed"; }
  local conflict=""
  conflict=$(python3 - "$vcn_cidr" "${name}-vcn" "$work/vcns.json" <<'PY'
import ipaddress, json, sys
ours = ipaddress.ip_network(sys.argv[1])
our_name, path = sys.argv[2:]
doc = json.loads(open(path).read())
for item in doc.get("data") or []:
    if item.get("display-name") == our_name:
        continue
    cidrs = item.get("cidr-blocks") or ([] if not item.get("cidr-block") else [item["cidr-block"]])
    for cidr in cidrs:
        if ours.overlaps(ipaddress.ip_network(cidr)):
            sys.stdout.write((item.get("display-name") or "unnamed-vcn") + "\n")
            sys.exit(0)
sys.exit(0)
PY
)
  if [[ -n "$conflict" ]]; then
    fail "VCN CIDR $vcn_cidr overlaps $conflict. Pass a different --vcn-cidr. Nothing was changed."
  fi
  vcn_id=$(list_id "$work/vcns.json" "${name}-vcn") || fail "ambiguous VCN name"
  if [[ -z "$vcn_id" ]]; then
    dns_label=$(printf '%s' "$name" | tr -cd 'a-z0-9' | cut -c1-15)
    run_oci "$work/vcn-create.json" network vcn create -c "$compartment" \
      --cidr-blocks "[\"$vcn_cidr\"]" --display-name "${name}-vcn" --dns-label "$dns_label" \
      || { show_err; fail "vcn create failed"; }
    vcn_id=$(created_id "$work/vcn-create.json")
  fi
  run_oci "$work/vcnget.json" network vcn get --vcn-id "$vcn_id" || { show_err; fail "vcn get failed"; }
  [[ "$(resource_name "$work/vcnget.json")" == "${name}-vcn" ]] || fail "VCN display name does not match"
  rt_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["data"]["default-route-table-id"])' "$work/vcnget.json")
  run_oci "$work/igws.json" network internet-gateway list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "igw list failed"; }
  igw_id=$(list_id "$work/igws.json" "${name}-igw") || fail "ambiguous internet gateway name"
  if [[ -z "$igw_id" ]]; then
    run_oci "$work/igw-create.json" network internet-gateway create -c "$compartment" --vcn-id "$vcn_id" \
      --is-enabled true --display-name "${name}-igw" || { show_err; fail "internet gateway create failed"; }
    igw_id=$(created_id "$work/igw-create.json")
  fi
  run_oci "$work/rt.json" network route-table get --rt-id "$rt_id" || { show_err; fail "route table get failed"; }
  if ! route_ok "$work/rt.json" "$igw_id"; then
    route_json=$(python3 - "$igw_id" <<'PY'
import json, sys
print(json.dumps([{
    "destination": "0.0.0.0/0",
    "destinationType": "CIDR_BLOCK",
    "networkEntityId": sys.argv[1],
    "description": "default via IGW",
}]))
PY
)
    run_oci "$work/rt-upd.json" network route-table update --rt-id "$rt_id" --route-rules "$route_json" --force \
      || { show_err; fail "route table update failed"; }
  fi
  run_oci "$work/seclists.json" network security-list list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "security list list failed"; }
  seclist_id=$(list_id "$work/seclists.json" "${name}-seclist") || fail "ambiguous security list name"
  if [[ -z "$seclist_id" ]]; then
    run_oci "$work/sl-create.json" network security-list create -c "$compartment" --vcn-id "$vcn_id" \
      --display-name "${name}-seclist" --ingress-security-rules "$ingress_json" --egress-security-rules "$egress_json" \
      || { show_err; fail "security list create failed"; }
    seclist_id=$(created_id "$work/sl-create.json")
  else
    run_oci "$work/sl-get.json" network security-list get --security-list-id "$seclist_id" || { show_err; fail "security list get failed"; }
    if ! rules_ok "$work/sl-get.json" ; then
      run_oci "$work/sl-upd.json" network security-list update --security-list-id "$seclist_id" \
        --ingress-security-rules "$ingress_json" --egress-security-rules "$egress_json" --force \
        || { show_err; fail "security list update failed"; }
    fi
  fi
  subnet_cidr=$(python3 - "$vcn_cidr" <<'PY'
import ipaddress, sys
net = ipaddress.ip_network(sys.argv[1])
print(next(net.subnets(new_prefix=24)))
PY
)
  run_oci "$work/subnets.json" network subnet list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "subnet list failed"; }
  subnet_id=$(list_id "$work/subnets.json" "${name}-subnet") || fail "ambiguous subnet name"
  if [[ -z "$subnet_id" ]]; then
    run_oci "$work/sub-create.json" network subnet create -c "$compartment" --vcn-id "$vcn_id" \
      --cidr-block "$subnet_cidr" --display-name "${name}-subnet" --dns-label hub \
      --prohibit-public-ip-on-vnic false --route-table-id "$rt_id" \
      --security-list-ids "[\"$seclist_id\"]" || { show_err; fail "subnet create failed"; }
    subnet_id=$(created_id "$work/sub-create.json")
  fi
  printf '%s\n' "$subnet_id"
}

if [[ -n "$our_state" && "$our_state" != "RUNNING" ]]; then
  fail "instance $name is $our_state. Refusing to launch another or to terminate it. Start it in the console or destroy this stack with --destroy."
fi

subnet_id=""
if [[ "$quota_launch" == 1 || -z "$our_state" ]]; then
  subnet_id=$(ensure_network)
fi
if [[ -z "$subnet_id" ]]; then
  run_oci "$work/vcns.json" network vcn list -c "$compartment" --all || { show_err; fail "vcn list failed"; }
  vcn_id=$(list_id "$work/vcns.json" "${name}-vcn") || fail "ambiguous VCN name"
  [[ -n "$vcn_id" ]] || fail "instance exists but its VCN ${name}-vcn was not found"
  run_oci "$work/subnets.json" network subnet list -c "$compartment" --vcn-id "$vcn_id" --all || { show_err; fail "subnet list failed"; }
  subnet_id=$(list_id "$work/subnets.json" "${name}-subnet") || fail "ambiguous subnet name"
fi

launch_instance() {
  local ad=$1
  if ! run_oci "$work/launch.json" compute instance launch -c "$compartment" \
    --availability-domain "$ad" --shape "VM.Standard.A1.Flex" \
    --shape-config "{\"ocpus\":${ocpus},\"memoryInGBs\":${memory}}" \
    --image-id "$image_id" --boot-volume-size-in-gbs "$boot_gb" \
    --subnet-id "$subnet_id" --assign-public-ip true \
    --display-name "$name" --hostname-label "$name" \
    --ssh-authorized-keys-file "$pubkey" --user-data-file "$work/user-data" ; then
    if grep -qi 'out of host capacity' "${OCI_LAST_ERR:-/dev/null}"; then
      echo "capacity: $ad is out of host capacity" >&2
      cleanup_capacity
      return 10
    fi
    show_err
    fail "instance launch failed"
  fi
  instance_id=$(created_id "$work/launch.json")
  local state
  state=$(lifecycle "$work/launch.json" || true)
  local tries=0
  while [[ "$state" != "RUNNING" ]]; do
    tries=$((tries + 1))
    if (( tries > 90 )); then
      fail "instance did not reach RUNNING"
    fi
    sleep 2
    run_oci "$work/iget.json" compute instance get --instance-id "$instance_id" || { show_err; fail "instance get failed"; }
    state=$(lifecycle "$work/iget.json")
    case "$state" in
      RUNNING) break ;;
      PROVISIONING|STARTING) ;;
      *) fail "instance entered $state" ;;
    esac
  done
  printf '%s\n' "$instance_id"
}

render_user_data "$work/user-data" "$cloud_public_host"

instance_id=""
if [[ "$our_state" == "RUNNING" ]]; then
  instance_id=$(python3 - "$work/instances.json" "$name" <<'PY'
import json, sys
name = sys.argv[2]
doc = json.loads(open(sys.argv[1]).read())
for item in doc.get("data") or []:
    if item.get("display-name") == name and item.get("lifecycle-state") == "RUNNING":
        print(item["id"])
        break
PY
)
  echo "Reusing running instance $name"
else
  deadline=$((SECONDS + max_wait))
  while :; do
    for ad in "${ads[@]}"; do
      set +e
      instance_id=$(launch_instance "$ad")
      rc=$?
      set -e
      if [[ "$rc" -eq 0 && -n "$instance_id" ]]; then
        break 2
      fi
      if [[ "$rc" -eq 10 ]]; then
        continue
      fi
      fail "instance launch failed"
    done
    if (( SECONDS >= deadline )); then
      fail "out of host capacity in every availability domain after ${max_wait}s"
    fi
    if (( interval > 0 )); then
      sleep "$interval"
    fi
  done
fi

public_ip=""
tries=0
while [[ -z "$public_ip" ]]; do
  tries=$((tries + 1))
  if (( tries > 30 )); then
    fail "instance has no public IPv4"
  fi
  run_oci "$work/vnics.json" compute instance list-vnics --instance-id "$instance_id" || { show_err; fail "vnic list failed"; }
  public_ip=$(python3 - "$work/vnics.json" <<'PY'
import json, sys
doc = json.loads(open(sys.argv[1]).read())
for nic in doc.get("data") or []:
    ip = nic.get("public-ip") or nic.get("publicIp") or ""
    if ip:
        print(ip)
        break
PY
)
  if [[ -z "$public_ip" ]]; then
    sleep 2
  fi
done
[[ "$public_ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail "refusing a non-IPv4 public address"

if [[ -n "$domain" ]]; then
  url_host=$domain
else
  url_host=${public_ip//./-}.sslip.io
  echo "WARNING: public URL https://$url_host uses sslip.io. Choose a real domain before enrolling machines." >&2
fi
echo "public-ip=$public_ip"
echo "https://$url_host"
if [[ "$no_wait" == 1 ]]; then
  echo "Cloud-init is installing Docker and the Hub. Not waiting for HTTPS."
  echo "Next: ops/oci/install-hub.sh --host $public_ip --user $ssh_user --public-host $url_host --git-ref $git_ref"
  exit 0
fi

command -v curl >/dev/null || fail "curl not found on PATH"
deadline=$((SECONDS + 1800))
while :; do
  status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 10 "https://$url_host/healthz") || status=000
  if [[ "$status" == 200 ]]; then
    break
  fi
  if (( SECONDS >= deadline )); then
    echo "provision: HTTPS did not become ready. The instance is up; cloud-init may still be installing." >&2
    echo "Next: ops/oci/install-hub.sh --host $public_ip --user $ssh_user --public-host $url_host --git-ref $git_ref" >&2
    exit 1
  fi
  sleep 10
done
echo "HTTPS is up. Next: ops/oci/install-hub.sh --host $public_ip --user $ssh_user --public-host $url_host --git-ref $git_ref"
echo "That command is idempotent, waits for cloud-init, and can enroll the first admin with TOTP via ops/fly/setup-admin.sh."
