#!/bin/bash
# Stubbed tests for the OCI Autopilot route. No cloud calls.
# Tests intentionally isolate PATH and sourced globals in subshells.
# shellcheck disable=SC2030,SC2031
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE/.."

run_test() {
  local name=$1
  shift
  echo "--- RUN   $name"
  "$@"
  echo "--- PASS  $name"
}

write_stub() {
  local dir=$1
  mkdir -p "$dir/bin"
  cat > "$dir/bin/oci" <<'PY'
#!/usr/bin/env python3
import json, os, shutil, sys

log_path = os.environ["OCI_STUB_LOG"]
state_path = os.environ["OCI_STUB_STATE"]
stub_dir = os.environ["OCI_STUB_DIR"]

def load():
    if not os.path.exists(state_path) or os.path.getsize(state_path) == 0:
        return {}
    state = json.load(open(state_path))
    for key in ("vcns", "igws", "route_tables", "seclists", "subnets", "instances", "boots", "volumes"):
        state.setdefault(key, [])
    state.setdefault("n", 10)
    return state

def save(state):
    tmp = state_path + ".tmp"
    with open(tmp, "w") as handle:
        json.dump(state, handle)
    os.replace(tmp, state_path)

def parse(argv):
    flags = {}
    pos = []
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg in ("--all", "--force"):
            flags[arg] = "true"
            i += 1
            continue
        if arg.startswith("-"):
            flags[arg] = argv[i + 1] if i + 1 < len(argv) else ""
            i += 2
            continue
        pos.append(arg)
        i += 1
    return pos, flags

def emit(payload):
    json.dump(payload, sys.stdout)
    sys.stdout.write("\n")

def nid(state, kind):
    state["n"] += 1
    return "ocid1.%s.stub.%s" % (kind, state["n"])

def find(items, ident):
    for item in items:
        if item.get("id") == ident:
            return item
    return None

args = sys.argv[1:]
pos, flags = parse(args)
cmd = " ".join(pos)
with open(log_path, "a") as handle:
    handle.write(json.dumps({"cmd": cmd, "flags": flags}) + "\n")
state = load()

def ok_get(collection, flag):
    item = find(state[collection], flags.get(flag, ""))
    if item is None:
        sys.stderr.write("NotAuthorizedOrNotFound\n")
        sys.exit(1)
    emit({"data": item})

if cmd == "iam availability-domain list":
    emit({"data": [{"name": "stub:AD-1"}, {"name": "stub:AD-2"}, {"name": "stub:AD-3"}]})
elif cmd == "compute image list":
    version = flags.get("--operating-system-version", "22.04")
    emit({"data": [{"id": "ocid1.image.stub.1", "display-name": "Canonical-Ubuntu-%s-aarch64-test" % version}]})
elif cmd == "compute instance list":
    emit({"data": state["instances"]})
elif cmd == "bv boot-volume list":
    ad = flags.get("--availability-domain", "")
    emit({"data": [item for item in state["boots"] if item.get("availability-domain") == ad]})
elif cmd == "bv volume list":
    emit({"data": state["volumes"]})
elif cmd == "network vcn list":
    emit({"data": state["vcns"]})
elif cmd == "network vcn create":
    ident = nid(state, "vcn")
    rt = nid(state, "routetable")
    cidrs = json.loads(flags["--cidr-blocks"])
    state["vcns"].append({
        "id": ident, "display-name": flags["--display-name"], "cidr-blocks": cidrs,
        "cidr-block": cidrs[0], "default-route-table-id": rt, "lifecycle-state": "AVAILABLE",
    })
    state["route_tables"].append({"id": rt, "display-name": "default", "route-rules": [], "lifecycle-state": "AVAILABLE"})
    save(state)
    emit({"data": {"id": ident, "display-name": flags["--display-name"], "lifecycle-state": "AVAILABLE"}})
elif cmd == "network vcn get":
    ok_get("vcns", "--vcn-id")
elif cmd == "network vcn delete":
    state["vcns"] = [item for item in state["vcns"] if item["id"] != flags["--vcn-id"]]
    save(state)
    emit({"data": {}})
elif cmd == "network internet-gateway list":
    emit({"data": [item for item in state["igws"] if item.get("vcn-id") == flags.get("--vcn-id")]})
elif cmd == "network internet-gateway create":
    ident = nid(state, "internetgateway")
    item = {"id": ident, "display-name": flags["--display-name"], "vcn-id": flags["--vcn-id"], "lifecycle-state": "AVAILABLE"}
    state["igws"].append(item)
    save(state)
    emit({"data": {"id": ident, "lifecycle-state": "AVAILABLE"}})
elif cmd == "network internet-gateway get":
    ok_get("igws", "--ig-id")
elif cmd == "network internet-gateway delete":
    state["igws"] = [item for item in state["igws"] if item["id"] != flags["--ig-id"]]
    save(state)
    emit({"data": {}})
elif cmd == "network route-table get":
    ok_get("route_tables", "--rt-id")
elif cmd == "network route-table update":
    item = find(state["route_tables"], flags["--rt-id"])
    if item is None:
        sys.exit(1)
    item["route-rules"] = json.loads(flags["--route-rules"])
    save(state)
    emit({"data": item})
elif cmd == "network security-list list":
    emit({"data": [item for item in state["seclists"] if item.get("vcn-id") == flags.get("--vcn-id")]})
elif cmd == "network security-list create":
    ident = nid(state, "securitylist")
    item = {
        "id": ident, "display-name": flags["--display-name"], "vcn-id": flags["--vcn-id"],
        "lifecycle-state": "AVAILABLE",
        "ingress-security-rules": json.loads(flags["--ingress-security-rules"]),
        "egress-security-rules": json.loads(flags["--egress-security-rules"]),
    }
    state["seclists"].append(item)
    save(state)
    emit({"data": {"id": ident, "lifecycle-state": "AVAILABLE"}})
elif cmd == "network security-list get":
    ok_get("seclists", "--security-list-id")
elif cmd == "network security-list update":
    item = find(state["seclists"], flags["--security-list-id"])
    if item is None:
        sys.exit(1)
    item["ingress-security-rules"] = json.loads(flags["--ingress-security-rules"])
    item["egress-security-rules"] = json.loads(flags["--egress-security-rules"])
    save(state)
    emit({"data": item})
elif cmd == "network security-list delete":
    state["seclists"] = [item for item in state["seclists"] if item["id"] != flags["--security-list-id"]]
    save(state)
    emit({"data": {}})
elif cmd == "network subnet list":
    emit({"data": [item for item in state["subnets"] if item.get("vcn-id") == flags.get("--vcn-id")]})
elif cmd == "network subnet create":
    ident = nid(state, "subnet")
    item = {"id": ident, "display-name": flags["--display-name"], "vcn-id": flags["--vcn-id"], "lifecycle-state": "AVAILABLE"}
    state["subnets"].append(item)
    save(state)
    emit({"data": {"id": ident, "lifecycle-state": "AVAILABLE"}})
elif cmd == "network subnet get":
    ok_get("subnets", "--subnet-id")
elif cmd == "network subnet delete":
    state["subnets"] = [item for item in state["subnets"] if item["id"] != flags["--subnet-id"]]
    save(state)
    emit({"data": {}})
elif cmd == "compute instance launch":
    left = state.get("launch_failures_left")
    if left is None:
        left = int(os.environ.get("OCI_STUB_FAILS", "0"))
    if left > 0:
        state["launch_failures_left"] = left - 1
        if os.environ.get("OCI_STUB_CAPACITY_LEAVES") == "1":
            state["instances"].append({
                "id": nid(state, "instance"),
                "display-name": flags.get("--display-name", ""),
                "lifecycle-state": "PROVISIONING",
                "shape": "VM.Standard.A1.Flex",
                "shape-config": {"ocpus": 2, "memory-in-gbs": 12},
            })
        save(state)
        sys.stderr.write("Out of host capacity.\n")
        sys.exit(1)
    ident = nid(state, "instance")
    state["instances"].append({
        "id": ident,
        "display-name": flags["--display-name"],
        "lifecycle-state": "RUNNING",
        "shape": flags.get("--shape", ""),
        "shape-config": json.loads(flags["--shape-config"]),
    })
    state["boots"].append({
        "id": nid(state, "bootvolume"),
        "display-name": flags["--display-name"] + " boot",
        "lifecycle-state": "AVAILABLE",
        "size-in-gbs": int(flags["--boot-volume-size-in-gbs"]),
        "availability-domain": flags["--availability-domain"],
    })
    save(state)
    source = flags.get("--user-data-file", "")
    if source and os.path.isfile(source):
        shutil.copy(source, os.path.join(stub_dir, "user-data"))
    emit({"data": {"id": ident, "lifecycle-state": "RUNNING", "display-name": flags["--display-name"]}})
elif cmd == "compute instance get":
    ok_get("instances", "--instance-id")
elif cmd == "compute instance terminate":
    item = find(state["instances"], flags["--instance-id"])
    if item is None:
        sys.exit(1)
    item["lifecycle-state"] = "TERMINATED"
    for boot in state["boots"]:
        if boot.get("display-name", "").startswith(item.get("display-name", "nomatch")):
            boot["lifecycle-state"] = "TERMINATED"
    save(state)
    emit({"data": item})
elif cmd == "compute instance list-vnics":
    emit({"data": [{"public-ip": "203.0.113.10"}]})
else:
    sys.stderr.write("stub missing %s\n" % cmd)
    sys.exit(3)
PY
  chmod 755 "$dir/bin/oci"
}

prep() {
  local dir=$1
  write_stub "$dir"
  : > "$dir/calls.jsonl"
  if [[ ! -f "$dir/state.json" ]]; then
    printf '%s\n' '{"vcns":[],"igws":[],"route_tables":[],"seclists":[],"subnets":[],"instances":[],"boots":[],"volumes":[]}' > "$dir/state.json"
  fi
  export PATH="$dir/bin:/usr/bin:/bin"
  export OCI_STUB_LOG="$dir/calls.jsonl"
  export OCI_STUB_STATE="$dir/state.json"
  export OCI_STUB_DIR="$dir"
  export OCI_STUB_FAILS="${OCI_STUB_FAILS:-0}"
  unset OCI_STUB_CAPACITY_LEAVES || true
}

mutating() {
  python3 - "$1" <<'PY'
import json, sys
bad = {"create", "launch", "delete", "terminate", "update"}
for line in open(sys.argv[1]):
    if not line.strip():
        continue
    last = json.loads(line)["cmd"].split()[-1]
    if last in bad:
        print(json.loads(line)["cmd"])
PY
}

test_render_user_data() (
  local tmp
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  bash ops/oci/provision.sh --render-user-data "$tmp/user-data" --domain hub.example.com --git-ref main \
    --repo https://github.com/teddashh/AI-Intune.git >"$tmp/out"
  python3 - "$tmp/user-data" ops/oci/host-setup.sh <<'PY'
import base64, pathlib, re, sys
text = pathlib.Path(sys.argv[1]).read_text()
script = pathlib.Path(sys.argv[2]).read_bytes()
match = re.search(r"encoding: b64\n    content: \|\n      (\S+)", text)
if not match:
    raise SystemExit("user-data has no base64 payload")
decoded = base64.b64decode(match.group(1))
if decoded != script:
    raise SystemExit("cloud-init payload is not host-setup.sh")
for needle in (b"REJECT", b"--dport", b"CLAWCTL_AUTH_MODE=local", b"CLAWCTL_PUBLIC_URL", b"netfilter-persistent", b"iptables-save", b"rules.v4"):
    if needle not in decoded:
        raise SystemExit("missing %s" % needle)
if "--public-host" not in text or "hub.example.com" not in text:
    raise SystemExit("domain was not passed to cloud-init")
if "0.0.0.0/0" in text and "ssh" in text.lower():
    pass
PY
  grep -q 'iptables-persistent' "$tmp/user-data"
)

test_name_guard_and_warnings() (
  local tmp
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  prep "$tmp"
  if bash ops/oci/provision.sh --name other-a1 --dry-run >"$tmp/out" 2>"$tmp/err"; then
    echo "fail: other-a1 name accepted"
    exit 1
  fi
  grep -q 'clawctl-' "$tmp/err"
  if grep -q 'ocid1.instance' "$tmp/out" "$tmp/err"; then
    echo "fail: printed an instance id while rejecting a name"
    exit 1
  fi
  bash ops/oci/provision.sh --dry-run --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    --capacity-interval 0 --capacity-max-wait 0 >"$tmp/dry" 2>"$tmp/dry.err"
  grep -q '0.0.0.0/0' "$tmp/dry.err"
  grep -q 'sslip.io' "$tmp/dry.err"
  grep -q 'Canonical-Ubuntu-22.04-aarch64-test' "$tmp/dry"
  grep -q 'stub:AD-1 stub:AD-3 stub:AD-2' "$tmp/dry"
  [[ -z "$(mutating "$tmp/calls.jsonl")" ]]
  bash ops/oci/provision.sh --dry-run --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    --ssh-cidr 203.0.113.10/32 --domain hub.example.com --free-max --os-version 24.04 \
    >"$tmp/tight" 2>"$tmp/tight.err"
  if grep -q 'open to 0.0.0.0/0' "$tmp/tight.err"; then
    echo "fail: world-SSH warning shown for a restricted CIDR"
    exit 1
  fi
  if grep -q 'sslip.io' "$tmp/tight.err"; then
    echo "fail: sslip warning shown when a domain was given"
    exit 1
  fi
  grep -q '4 OCPU / 24 GB' "$tmp/tight"
  grep -q 'Canonical-Ubuntu-24.04-aarch64-test' "$tmp/tight"
)

test_quota_refuses_without_creating() (
  local tmp
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  prep "$tmp"
  python3 - "$tmp/state.json" <<'PY'
import json, sys
json.dump({
  "instances": [{
    "id": "ocid1.instance.stub.other",
    "display-name": "other-a1",
    "lifecycle-state": "RUNNING",
    "shape": "VM.Standard.A1.Flex",
    "shape-config": {"ocpus": 4, "memory-in-gbs": 24},
  }],
  "boots": [{
    "id": "ocid1.bootvolume.stub.other",
    "display-name": "other boot",
    "lifecycle-state": "AVAILABLE",
    "size-in-gbs": 200,
    "availability-domain": "stub:AD-1",
  }],
}, open(sys.argv[1], "w"))
PY
  if bash ops/oci/provision.sh --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    --capacity-interval 0 --capacity-max-wait 0 --no-wait >"$tmp/out" 2>"$tmp/err"; then
    echo "fail: exceeded Always Free and still exited 0"
    exit 1
  fi
  grep -q 'Always Free' "$tmp/err"
  [[ -z "$(mutating "$tmp/calls.jsonl")" ]]
  : > "$tmp/calls.jsonl"
  bash ops/oci/provision.sh --dry-run --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    >"$tmp/dry" 2>"$tmp/dry.err"
  grep -q 'Always Free' "$tmp/dry.err"
  grep -q 'No resources were created.' "$tmp/dry"
  [[ -z "$(mutating "$tmp/calls.jsonl")" ]]
)

test_launch_idempotent_and_capacity() (
  local tmp pub
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  prep "$tmp"
  pub=$tmp/key.pub
  printf '%s\n' 'ssh-ed25519 AAAAC3TESTKEYONLY stub' > "$pub"
  OCI_STUB_FAILS=2 OCI_STUB_CAPACITY_LEAVES=1 \
    bash ops/oci/provision.sh --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
      --capacity-interval 0 --capacity-max-wait 30 --no-wait --ssh-public-key "$pub" \
      >"$tmp/out" 2>"$tmp/err"
  grep -q 'public-ip=203.0.113.10' "$tmp/out"
  grep -q 'https://203-0-113-10.sslip.io' "$tmp/out"
  grep -q 'out of host capacity' "$tmp/err"
  if grep -q 'AAAAC3TESTKEYONLY' "$tmp/out" "$tmp/err"; then
    echo "fail: SSH public key material was printed"
    exit 1
  fi
  python3 - "$tmp/calls.jsonl" "$tmp/user-data" ops/oci/host-setup.sh "$tmp/state.json" <<'PY'
import base64, json, pathlib, re, sys
log, user_data, script, state_path = sys.argv[1:]
launches = []
terminates = 0
for line in open(log):
    item = json.loads(line)
    if item["cmd"] == "compute instance launch":
        launches.append(item["flags"]["--availability-domain"])
    if item["cmd"] == "compute instance terminate":
        terminates += 1
if launches != ["stub:AD-1", "stub:AD-3", "stub:AD-2"]:
    raise SystemExit("capacity retry order was %s" % launches)
if terminates < 1:
    raise SystemExit("capacity leftover was not terminated")
text = pathlib.Path(user_data).read_text()
decoded = base64.b64decode(re.search(r"content: \|\n      (\S+)", text).group(1))
if decoded != pathlib.Path(script).read_bytes():
    raise SystemExit("launched user-data is not host-setup.sh")
if b"REJECT" not in decoded or b"CLAWCTL_AUTH_MODE=local" not in decoded:
    raise SystemExit("user-data missing iptables or auth mode")
state = json.load(open(state_path))
running = [item for item in state["instances"] if item["lifecycle-state"] == "RUNNING"]
provisioning = [item for item in state["instances"] if item["lifecycle-state"] == "PROVISIONING"]
if len(running) != 1 or provisioning:
    raise SystemExit("expected one RUNNING instance and no PROVISIONING leftovers")
PY
  : > "$tmp/calls.jsonl"
  bash ops/oci/provision.sh --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    --capacity-interval 0 --capacity-max-wait 0 --no-wait --ssh-public-key "$pub" \
    >"$tmp/again" 2>"$tmp/again.err"
  grep -q 'Reusing running instance' "$tmp/again"
  [[ -z "$(mutating "$tmp/calls.jsonl")" ]]
)

test_destroy_confirmation() (
  local tmp pub
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  prep "$tmp"
  pub=$tmp/key.pub
  printf '%s\n' 'ssh-ed25519 AAAAC3TESTKEYONLY stub' > "$pub"
  bash ops/oci/provision.sh --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    --capacity-interval 0 --capacity-max-wait 0 --no-wait --ssh-public-key "$pub" >/dev/null
  cp "$tmp/state.json" "$tmp/before.json"
  if printf '%s\n' 'nope' | bash ops/oci/provision.sh --destroy --region us-ashburn-1 \
    --compartment-id ocid1.tenancy.oc1..stub >"$tmp/bad" 2>"$tmp/bad.err"; then
    echo "fail: mismatched confirmation was accepted"
    exit 1
  fi
  grep -q 'nothing deleted' "$tmp/bad.err"
  cmp -s "$tmp/before.json" "$tmp/state.json"
  : > "$tmp/calls.jsonl"
  bash ops/oci/provision.sh --destroy --dry-run --region us-ashburn-1 \
    --compartment-id ocid1.tenancy.oc1..stub >"$tmp/plan" 2>"$tmp/plan.err"
  grep -q 'require the typed name' "$tmp/plan"
  [[ -z "$(mutating "$tmp/calls.jsonl")" ]]
  cmp -s "$tmp/before.json" "$tmp/state.json"
  printf '%s\n' 'clawctl-hub' | bash ops/oci/provision.sh --destroy --region us-ashburn-1 \
    --compartment-id ocid1.tenancy.oc1..stub >"$tmp/gone" 2>"$tmp/gone.err"
  grep -q 'Destroyed clawctl-hub' "$tmp/gone"
  python3 - "$tmp/state.json" <<'PY'
import json, sys
state = json.load(open(sys.argv[1]))
if state.get("vcns") or state.get("subnets") or state.get("igws") or state.get("seclists"):
    raise SystemExit("network resources remained after destroy")
live = [item for item in state.get("instances", []) if item.get("lifecycle-state") != "TERMINATED"]
if live:
    raise SystemExit("instance was not terminated")
PY
)

test_cidr_overlap_refuses() (
  local tmp pub
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  prep "$tmp"
  pub=$tmp/key.pub
  printf '%s\n' 'ssh-ed25519 AAAAC3TESTKEYONLY stub' > "$pub"
  python3 - "$tmp/state.json" <<'PY'
import json, sys
json.dump({"vcns": [{
  "id": "ocid1.vcn.stub.other",
  "display-name": "other-vcn",
  "cidr-blocks": ["10.50.0.0/16"],
  "cidr-block": "10.50.0.0/16",
  "default-route-table-id": "ocid1.routetable.stub.other",
  "lifecycle-state": "AVAILABLE",
}]}, open(sys.argv[1], "w"))
PY
  if bash ops/oci/provision.sh --region us-ashburn-1 --compartment-id ocid1.tenancy.oc1..stub \
    --no-wait --ssh-public-key "$pub" --capacity-interval 0 --capacity-max-wait 0 \
    >"$tmp/out" 2>"$tmp/err"; then
    echo "fail: overlapping CIDR was accepted"
    exit 1
  fi
  grep -q 'other-vcn' "$tmp/err"
  [[ -z "$(mutating "$tmp/calls.jsonl")" ]]
)

test_host_setup_firewall_and_dry_run() (
  local tmp
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/iptables" <<'PY'
#!/usr/bin/env python3
import os, sys
path = os.environ["IPT_RULES"]
rules = open(path).read().splitlines()
args = sys.argv[1:]
if args[:2] == ["-S", "INPUT"]:
    sys.stdout.write("\n".join(rules) + "\n")
    sys.exit(0)
if args[0] == "-C":
    port = args[args.index("--dport") + 1]
    for line in rules:
        if "--dport %s " % port in line and line.endswith("-j ACCEPT"):
            sys.exit(0)
    sys.exit(1)
if args[0] == "-I":
    number = int(args[2])
    rules.insert(number, "-A INPUT " + " ".join(args[3:]))
    open(path, "w").write("\n".join(rules) + "\n")
    sys.exit(0)
if args[0] == "-A":
    rules.append(" ".join(args))
    open(path, "w").write("\n".join(rules) + "\n")
    sys.exit(0)
sys.exit(2)
PY
  chmod 755 "$tmp/iptables"
  printf '%s\n' '-P INPUT ACCEPT' \
    '-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT' \
    '-A INPUT -j REJECT --reject-with icmp-host-prohibited' > "$tmp/rules"
  (
    # shellcheck disable=SC1091
    source ops/oci/host-setup.sh
    export PATH="$tmp:$PATH" IPT_RULES="$tmp/rules"
    # The stub is named iptables via PATH directory...
    ln -sf "$tmp/iptables" "$tmp/bin-ipt" 2>/dev/null || true
  )
  mkdir -p "$tmp/path"
  ln -s "$tmp/iptables" "$tmp/path/iptables"
  (
    set -euo pipefail
    # shellcheck disable=SC1091
    source ops/oci/host-setup.sh
    export PATH="$tmp/path:/usr/bin:/bin" IPT_RULES="$tmp/rules"
    [[ "$(sslip_from_ip 203.0.113.10)" == "203-0-113-10.sslip.io" ]]
    open_port iptables 80
    open_port iptables 443
    open_port iptables 80
    open_port iptables 443
  )
  python3 - "$tmp/rules" <<'PY'
import sys
rules = open(sys.argv[1]).read().splitlines()
if "REJECT" not in rules[-1]:
    raise SystemExit("REJECT is no longer last: %s" % rules)
for port in ("80", "443"):
    hits = [line for line in rules if "--dport %s " % port in line]
    if len(hits) != 1:
        raise SystemExit("port %s count %s" % (port, len(hits)))
    if rules.index(hits[0]) > rules.index(rules[-1]):
        raise SystemExit("port inserted after REJECT")
PY
  bash ops/oci/host-setup.sh --dry-run --public-host 203-0-113-10.sslip.io >"$tmp/dry" 2>"$tmp/dry.err"
  grep -q 'CLAWCTL_AUTH_MODE=local' "$tmp/dry"
  grep -q 'CLAWCTL_PUBLIC_URL=https://203-0-113-10.sslip.io' "$tmp/dry"
  grep -q 'sslip.io' "$tmp/dry.err"
  if bash ops/oci/host-setup.sh --dry-run --public-host auto --repo 'http://example.invalid/repo' >"$tmp/bad" 2>"$tmp/bad.err"; then
    echo "fail: non-https repo accepted"
    exit 1
  fi
)

test_install_hub_ssh() (
  local tmp mode
  local -a args
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  mkdir -p "$tmp/bin"
  cat > "$tmp/bin/ssh" <<'SH'
#!/bin/bash
printf '%s\n' "$*" >> "$SSH_LOG"
if [[ " $* " == *" -G "* ]]; then
  printf 'hostname %s\nuser configured-user\n' "${SSH_HOSTNAME:-203.0.113.10}"
  exit 0
fi
if [[ "${SSH_FAIL:-0}" == 1 ]]; then exit 1; fi
if [[ "$*" == *"sudo -n rm -f /var/lib/clawctl/setup-code"* && "${SSH_CLEANUP_FAIL:-0}" == 1 ]]; then exit 1; fi
if [[ "$*" == *'sudo -n bash "$f"'* ]]; then
  cat >/dev/null
  if [[ "$*" == *"--dry-run"* ]]; then
    echo 'Plan: install Docker Engine and the compose plugin if missing.'
    echo 'WARNING: public URL https://203-0-113-10.sslip.io uses sslip.io.' >&2
  else
    printf '%s\n' "remote ready" "clawctl-oci-url=https://203-0-113-10.sslip.io" "clawctl-setup-code=${STUB_CODE}"
  fi
  if [[ "${SSH_NO_MARKER:-0}" != 1 ]]; then echo 'clawctl-oci-done'; fi
fi
exit 0
SH
  cat > "$tmp/bin/curl" <<'SH'
#!/bin/bash
url=""
for arg in "$@"; do url=$arg; done
printf '%s\n' "$url" >> "$CURL_LOG"
case "$url" in
  */healthz) printf '200' ;;
  */setup) printf '%s' "${CURL_SETUP_STATUS:-200}" ;;
  *) printf '000' ;;
esac
SH
  cat > "$tmp/bin/setup-admin" <<'SH'
#!/bin/bash
codefile=""
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --setup-code-file) codefile=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    *) shift ;;
  esac
done
code=$(cat "$codefile")
if [[ "$code" != "$STUB_CODE" ]]; then
  echo "setup-admin stub: unexpected code file" >&2
  exit 1
fi
umask 077
printf '%s\n' '{"stored":true}' > "$out"
chmod 600 "$out"
printf '%s\n' 'admin enrolled' >> "$SSH_LOG"
echo "MFA enrolled. Credentials saved to $out"
SH
  chmod 755 "$tmp/bin/ssh" "$tmp/bin/curl" "$tmp/bin/setup-admin"
  export PATH="$tmp/bin:/usr/bin:/bin" SSH_LOG="$tmp/ssh.log" CURL_LOG="$tmp/curl.log" STUB_CODE="stub-setup-code-do-not-print" CLAWCTL_SETUP_ADMIN="$tmp/bin/setup-admin"
  : > "$SSH_LOG"
  bash ops/oci/install-hub.sh --dry-run --host 203.0.113.10 >"$tmp/dry" 2>"$tmp/dry.err"
  grep -q '203-0-113-10.sslip.io' "$tmp/dry.err"
  grep -q ' true$' "$SSH_LOG"
  # The remote shell expands $f, so match its literal reference.
  # shellcheck disable=SC2016
  grep -Fq 'sudo -n bash "$f" --dry-run' "$SSH_LOG"
  # shellcheck disable=SC2016
  [[ "$(grep -Fc 'sudo -n bash "$f"' "$SSH_LOG")" == 1 ]]
  grep -q 'SSH reachable' "$tmp/dry"
  grep -q '^Plan: install Docker' "$tmp/dry"
  grep -q 'No changes were made.' "$tmp/dry"
  : > "$SSH_LOG"
  : > "$tmp/config"
  bash ops/oci/install-hub.sh --dry-run --host my_vm --ssh-config "$tmp/config" \
    --public-host 203-0-113-10.sslip.io >"$tmp/alias" 2>"$tmp/alias.err"
  grep -q -- "-F $tmp/config" "$SSH_LOG"
  grep -q ' my_vm true$' "$SSH_LOG"
  if grep -q 'ubuntu@' "$SSH_LOG"; then exit 1; fi
  : > "$SSH_LOG"
  bash ops/oci/install-hub.sh --dry-run --host myvm --user operator \
    --public-host 203-0-113-10.sslip.io >"$tmp/user" 2>"$tmp/user.err"
  grep -q ' operator@myvm true$' "$SSH_LOG"
  if SSH_HOSTNAME=100.64.0.10 bash ops/oci/install-hub.sh --dry-run --host myvm >"$tmp/private" 2>"$tmp/private.err"; then
    echo 'fail: CGNAT alias accepted without --public-host'; exit 1
  fi
  grep -q -- '--public-host' "$tmp/private.err"
  if SSH_FAIL=1 bash ops/oci/install-hub.sh --dry-run --host myvm \
    --public-host hub.example.com >"$tmp/unreachable" 2>"$tmp/unreachable.err"; then
    echo 'fail: unreachable SSH accepted'; exit 1
  fi
  grep -q 'reachability probe failed' "$tmp/unreachable.err"
  bash ops/oci/install-hub.sh --host 203.0.113.10 --public-host 203-0-113-10.sslip.io \
    >"$tmp/out" 2>"$tmp/err"
  [[ "$(grep -c '^Setup code: stub-setup-code-do-not-print$' "$tmp/out")" == 1 ]]
  if grep -qx 'clawctl-oci-done' "$tmp/out" "$tmp/dry"; then exit 1; fi
  for mode in real dry; do
    args=()
    [[ "$mode" != dry ]] || args+=(--dry-run)
    if SSH_NO_MARKER=1 bash ops/oci/install-hub.sh --host 203.0.113.10 "${args[@]}" \
      >"$tmp/incomplete" 2>"$tmp/incomplete.err"; then
      echo 'fail: incomplete remote setup accepted'; exit 1
    fi
    grep -q 'remote setup did not finish (no completion marker)' "$tmp/incomplete.err"
  done
  : > "$tmp/out"
  : > "$SSH_LOG"
  CURL_SETUP_STATUS=404 bash ops/oci/install-hub.sh --host 203.0.113.10 \
    --public-host 203-0-113-10.sslip.io >"$tmp/closed" 2>"$tmp/closed.err"
  if grep -q 'stub-setup-code-do-not-print' "$tmp/closed" "$tmp/closed.err"; then
    echo "fail: setup code printed after setup was closed"
    exit 1
  fi
  grep -q 'already closed' "$tmp/closed"
  grep -q '203.0.113.10 sudo -n rm -f /var/lib/clawctl/setup-code$' "$SSH_LOG"
  : > "$SSH_LOG"
  CURL_SETUP_STATUS=200 bash ops/oci/install-hub.sh --host 203.0.113.10 \
    --public-host 203-0-113-10.sslip.io --admin-user admin --admin-out "$tmp/admin.json" \
    >"$tmp/admin.out" 2>"$tmp/admin.err"
  if grep -q 'stub-setup-code-do-not-print' "$tmp/admin.out" "$tmp/admin.err"; then
    echo "fail: setup code printed during admin enrollment"
    exit 1
  fi
  grep -q 'MFA enrolled' "$tmp/admin.out"
  [[ "$(tail -n 2 "$SSH_LOG" | head -n 1)" == 'admin enrolled' ]]
  tail -n 1 "$SSH_LOG" | grep -q '203.0.113.10 sudo -n rm -f /var/lib/clawctl/setup-code$'
  [[ "$(stat -c '%a' "$tmp/admin.json")" == 600 ]]
  # Idempotent: closed setup does not write the requested admin output file.
  : > "$SSH_LOG"
  CURL_SETUP_STATUS=404 bash ops/oci/install-hub.sh --host 203.0.113.10 \
    --public-host 203-0-113-10.sslip.io --admin-user admin --admin-out "$tmp/second-admin.json" >"$tmp/second" 2>"$tmp/second.err"
  grep -q 'already closed' "$tmp/second"
  grep -q 'no admin was created and --admin-out was not written' "$tmp/second.err"
  [[ ! -e "$tmp/second-admin.json" ]]
  grep -q '203.0.113.10 sudo -n rm -f /var/lib/clawctl/setup-code$' "$SSH_LOG"
  if grep -q "$STUB_CODE" "$tmp/second" "$tmp/second.err"; then exit 1; fi
  SSH_CLEANUP_FAIL=1 CURL_SETUP_STATUS=404 bash ops/oci/install-hub.sh --host 203.0.113.10 \
    --public-host hub.example.com >"$tmp/cleanup" 2>"$tmp/cleanup.err"
  grep -q 'warning: could not remove remote setup-code file' "$tmp/cleanup.err"
  if grep -q "$STUB_CODE" "$tmp/cleanup" "$tmp/cleanup.err"; then exit 1; fi
)

test_persist_port_rules() (
  local tmp port
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  # shellcheck disable=SC1091
  source ops/oci/host-setup.sh
  cat > "$tmp/rules.v4" <<'EOF'
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT
-A INPUT -p tcp -m state --state NEW -m tcp --dport 80 -j ACCEPT
-A INPUT -j REJECT --reject-with icmp-host-prohibited
-A INPUT -p tcp -m tcp --dport 80 -j ACCEPT
COMMIT
*nat
:PREROUTING ACCEPT [0:0]
COMMIT
EOF
  cp "$tmp/rules.v4" "$tmp/original"
  persist_port_rules "$tmp/rules.v4" 80 443
  cmp "$tmp/original" "$tmp/rules.v4.clawctl-bak"
  for port in 80 443; do
    [[ "$(grep -c -- "--dport $port " "$tmp/rules.v4")" == 1 ]]
    [[ "$(grep -n -- "--dport $port " "$tmp/rules.v4" | cut -d: -f1)" -lt \
       "$(grep -n -- '-j REJECT' "$tmp/rules.v4" | cut -d: -f1)" ]]
  done
  if grep -Eq 'ts-|DOCKER' "$tmp/rules.v4"; then exit 1; fi
  cp "$tmp/rules.v4" "$tmp/first"
  persist_port_rules "$tmp/rules.v4" 80 443
  cmp "$tmp/first" "$tmp/rules.v4"
  cmp "$tmp/original" "$tmp/rules.v4.clawctl-bak"
  [[ "$(stat -c '%a' "$tmp/rules.v4")" == 644 ]]
  cp "$tmp/original" "$tmp/rules.v6"
  persist_port_rules "$tmp/rules.v6" 80 443
  cmp "$tmp/first" "$tmp/rules.v6"
  mkdir "$tmp/bin"
  cat > "$tmp/bin/iptables-save" <<'SH'
#!/bin/bash
[[ "$*" == '-t filter' ]] || exit 1
cat <<'EOF'
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:ts-input - [0:0]
:ts-forward - [0:0]
:DOCKER - [0:0]
:DOCKER-USER - [0:0]
-A INPUT -j ts-input
-A ts-input -j ACCEPT
-A FORWARD -j DOCKER-USER
-A DOCKER -j ACCEPT
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT
COMMIT
EOF
SH
  chmod 755 "$tmp/bin/iptables-save"
  export PATH="$tmp/bin:$PATH"
  persist_port_rules "$tmp/new.v4" 80 443
  if grep -Eq 'ts-|DOCKER' "$tmp/new.v4"; then exit 1; fi
  grep -q -- '--dport 22' "$tmp/new.v4"
  for port in 80 443; do
    [[ "$(grep -c -- "--dport $port " "$tmp/new.v4")" == 1 ]]
  done
  [[ "$(tail -n 1 "$tmp/new.v4")" == COMMIT ]]
)

test_refresh_caddy_if_stale() (
  local tmp mode line
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  # shellcheck disable=SC1091
  source ops/oci/host-setup.sh
  cat > "$tmp/docker" <<'SH'
#!/bin/bash
set -euo pipefail
printf '%s\n' "$*" >> "$DOCKER_LOG"
case "$*" in
  'compose -p test exec -T caddy cat /etc/caddy/Caddyfile')
    cat >/dev/null
    [[ "$CADDY_MODE" != failed ]] || exit 1
    cat "$CADDY_CONTENT"
    ;;
  'compose -p test up -d --force-recreate --no-deps caddy') cat >/dev/null ;;
  *) exit 2 ;;
esac
SH
  chmod 755 "$tmp/docker"
  export PATH="$tmp:$PATH" DOCKER_LOG="$tmp/log" CADDY_CONTENT="$tmp/container"
  printf 'new config\n' > "$tmp/Caddyfile"
  for mode in matching changed failed; do
    export CADDY_MODE=$mode
    : > "$DOCKER_LOG"
    if [[ "$mode" == matching ]]; then
      cp "$tmp/Caddyfile" "$CADDY_CONTENT"
    else
      printf 'old config\n' > "$CADDY_CONTENT"
    fi
    {
      refresh_caddy_if_stale "$tmp/Caddyfile" docker compose -p test > "$tmp/output"
      read -r line
    } <<< "keep"
    [[ "$line" == keep ]]
    grep -qx 'compose -p test exec -T caddy cat /etc/caddy/Caddyfile' "$DOCKER_LOG"
    if [[ "$mode" == matching ]]; then
      [[ "$(wc -l < "$DOCKER_LOG")" == 1 ]]
      [[ ! -s "$tmp/output" ]]
    else
      [[ "$(wc -l < "$DOCKER_LOG")" == 2 ]]
      grep -qx 'compose -p test up -d --force-recreate --no-deps caddy' "$DOCKER_LOG"
      grep -qx 'Caddy config changed; recreated caddy.' "$tmp/output"
    fi
  done
)

test_shellcheck() {
  if command -v shellcheck >/dev/null 2>&1; then
    shellcheck ops/oci/*.sh ops/test-oci.sh
  else
    echo "shellcheck not installed; skipping automation lint"
  fi
}

run_test test_render_user_data test_render_user_data
run_test test_name_guard_and_warnings test_name_guard_and_warnings
run_test test_quota_refuses_without_creating test_quota_refuses_without_creating
run_test test_launch_idempotent_and_capacity test_launch_idempotent_and_capacity
run_test test_destroy_confirmation test_destroy_confirmation
run_test test_cidr_overlap_refuses test_cidr_overlap_refuses
run_test test_host_setup_firewall_and_dry_run test_host_setup_firewall_and_dry_run
run_test test_install_hub_ssh test_install_hub_ssh
run_test test_persist_port_rules test_persist_port_rules
run_test test_refresh_caddy_if_stale test_refresh_caddy_if_stale
run_test test_shellcheck test_shellcheck
