#!/usr/bin/env bash
# Install Prometheus + node_exporter from the distro packages and apply this
# repository's config and alert rules. Stage 1 of ops/prometheus/install.sh.
#
#   sudo CLAWCTL_FLEET_JSON=/path/to/fleet.json ./ops/prometheus/install-prometheus.sh
#
# Site values stay outside the shared tree:
#   CLAWCTL_FLEET_JSON       node_exporter scrape list (required; format:
#                            ops/prometheus/fleet.example.json)
#   CLAWCTL_PROMETHEUS_YML   Prometheus config (default: ops/prometheus/prometheus.yml,
#                            whose Hub target is a sample address — point this
#                            at your own copy)
#
# Idempotent: a re-run re-applies the config and does not reinstall. Nothing is
# deleted; an existing /etc/prometheus/prometheus.yml is backed up first.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RULES="$HERE/rules"
PROM_YML="${CLAWCTL_PROMETHEUS_YML:-$HERE/prometheus.yml}"
FLEET_JSON="${CLAWCTL_FLEET_JSON:-}"
STAMP="$(date +%Y%m%d-%H%M%S)"

if [ -z "$FLEET_JSON" ] || [ ! -f "$FLEET_JSON" ]; then
	echo "CLAWCTL_FLEET_JSON must name your fleet.json (see $HERE/fleet.example.json)" >&2
	exit 1
fi
if [ ! -f "$PROM_YML" ]; then
	echo "Prometheus config not found: $PROM_YML" >&2
	exit 1
fi
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert isinstance(d,list) and d, "fleet.json must be a non-empty list"' "$FLEET_JSON"

if [ "$(id -u)" -ne 0 ]; then
	echo "must run as root: sudo $0" >&2
	exit 1
fi

echo "== 1/5 install (apt, so the distro maintains the binaries)"
DEBIAN_FRONTEND=noninteractive apt-get install -y prometheus prometheus-node-exporter

echo "== 2/5 config"
install -d -m 0755 /etc/prometheus/targets /etc/prometheus/rules
if [ -f /etc/prometheus/prometheus.yml ] && ! cmp -s "$PROM_YML" /etc/prometheus/prometheus.yml; then
	cp -a /etc/prometheus/prometheus.yml "/etc/prometheus/prometheus.yml.bak-$STAMP"
	echo "   previous config backed up to prometheus.yml.bak-$STAMP"
fi
install -m 0644 "$PROM_YML" /etc/prometheus/prometheus.yml
install -m 0644 "$FLEET_JSON" /etc/prometheus/targets/fleet.json
install -m 0644 "$RULES"/*.yml /etc/prometheus/rules/

# rule_files is a glob. A glob that matches nothing is not an error: Prometheus
# starts, loads zero rules, and never alerts. Count explicitly.
want=$(find "$RULES" -name '*.yml' | wc -l)
got=$(find /etc/prometheus/rules -name '*.yml' | wc -l)
if [ "$want" -eq 0 ] || [ "$got" -ne "$want" ]; then
	echo "rule files incomplete: repo has $want, /etc/prometheus/rules has $got" >&2
	exit 1
fi
echo "   $got rule files"

echo "== 3/5 node_exporter textfile collector"
# clawctl-agent writes .prom files here; node_exporter picks them up.
TEXTFILE_DIR=/var/lib/prometheus/node-exporter
install -d -m 0755 "$TEXTFILE_DIR"
DEFAULTS=/etc/default/prometheus-node-exporter
touch "$DEFAULTS"
if ! grep -q 'collector\.textfile\.directory' "$DEFAULTS"; then
	cp -a "$DEFAULTS" "$DEFAULTS.bak-$STAMP"
	# Edit only the ARGS line; never source this file.
	if grep -q '^ARGS=' "$DEFAULTS"; then
		sed -i "s|^ARGS=\"\\(.*\\)\"|ARGS=\"\\1 --collector.textfile.directory=$TEXTFILE_DIR\"|" "$DEFAULTS"
	else
		printf 'ARGS="--collector.textfile.directory=%s"\n' "$TEXTFILE_DIR" >>"$DEFAULTS"
	fi
	echo "   added --collector.textfile.directory=$TEXTFILE_DIR"
else
	echo "   already set"
fi

echo "== 4/5 config check (no restart on a bad config)"
promtool check config /etc/prometheus/prometheus.yml

echo "== 5/5 restart and check"
systemctl enable --now prometheus prometheus-node-exporter
systemctl restart prometheus prometheus-node-exporter
sleep 3
for s in prometheus prometheus-node-exporter; do
	printf '   %-26s %s\n' "$s" "$(systemctl is-active "$s")"
done
printf '   :9090/-/healthy            %s\n' "$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://localhost:9090/-/healthy)"
printf '   :9100/metrics              %s\n' "$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://localhost:9100/metrics)"

echo
echo "up per fleet machine (0 is not always broken: a machine that never reports stays at 0):"
sleep 5
curl -s 'http://localhost:9090/api/v1/query?query=up{job="node"}' |
	python3 -c 'import json,sys
d=json.load(sys.stdin)
for r in sorted(d["data"]["result"], key=lambda x: x["metric"].get("machine","")):
    print("   %-14s up=%s" % (r["metric"].get("machine","?"), r["value"][1]))' 2>/dev/null ||
	echo "   (first scrape not finished yet; check again in 60 seconds)"
