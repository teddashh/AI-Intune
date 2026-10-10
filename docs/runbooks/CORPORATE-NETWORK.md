# Corporate network

System services do not read `/etc/environment` through PAM. A login shell can
enroll successfully while `clawctl-agent` cannot reach the Hub. The installer
creates a system proxy drop-in; check that it still matches the host settings.

| Symptom | Check | Fix |
|---|---|---|
| Login enrollment works; system agent times out | Run proxy-check. Compare direct and proxy health results. | Review its printed root command. It rebuilds the drop-in from `/etc/environment`, reloads systemd, and restarts only the agent. |
| Tailnet requests go through the proxy | Check `no_proxy_missing`. | Keep `localhost,127.0.0.1,::1,.ts.net,100.64.0.0/10` plus site ranges in NO_PROXY. |
| Clock drifts; chrony has Reach 0 everywhere | Run ntp-check. Check offset and synchronized status. | Use reachable Cloudflare NTS and optionally the site NTP source. |
| Distro NTS sources fail behind the perimeter | Check permission for NTS-KE TCP 4460 and NTP UDP 123. | Allow a working source. Cloudflare uses NTS; a site source can use UDP 123 without NTS. |
| timesyncd is unsynchronized | Run ntp-check and timedatectl. | Review the suggested timesyncd NTP drop-in. |

```bash
bash ops/fleet/net/proxy-check.sh --hub https://hub.example.com
bash ops/fleet/net/proxy-check.sh --hub https://hub.example.com --json
bash ops/fleet/net/ntp-check.sh --site-ntp ntp.corp.example
sudo bash ops/fleet/net/ntp-check.sh --apply --site-ntp ntp.corp.example
chronyc -n sources
timedatectl show -p NTPSynchronized
```

Proxy output shows host and port only. Credentials are never printed or passed
in curl argv. Proxy probes bypass NO_PROXY to test the proxy explicitly. Direct
probes use `curl --noproxy '*'`. A `fix-needed` verdict is diagnostic; proxy-check
exits successfully when it completes its checks. JSON includes the fix hint.
The hint reads proxy values from `/etc/environment` at execution time. It uses
printf, backs up the existing file, and writes the drop-in mode 0600. It preserves
site NO_PROXY entries and adds local and tailnet bypasses. Review any other drop-ins
that override this file. An unreadable drop-in requires a privileged check.

NTP apply needs root; otherwise it only prints a single-line root step. Chrony
sources go in `/etc/chrony/sources.d/10-local.sources`. Apply adds `sourcedir /etc/chrony/sources.d` to the main chrony config if
needed, with a backup. Confirm the directory is loaded after the restart.
On Oracle Linux the unit can be named `chronyd`; the check detects it. Only the
detected time daemon is restarted. Timesyncd uses
`/etc/systemd/timesyncd.conf.d/10-local.conf` with a `[Time]` header.
Existing files receive UTC-stamped backups. A re-run with identical content does
not restart the daemon. Read Reach and synchronization again after sources settle.
The checks do not diagnose firewall rules or change the perimeter.
