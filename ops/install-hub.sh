#!/usr/bin/env bash
# 在一台乾淨的機器上安裝 clawctl-hub。
#
# 目標：從零到「瀏覽器打得開」≤ 30 分鐘（docs/PHASES.md Phase 1 的完成判準之一）。
#
# 用法：
#   ./install-hub.sh                          # 僅既有 hub.env 已有兩個 auth 設定時可用
#   ./install-hub.sh --binary /path/to/hub --agent-bundles /path/to/build
#   ./install-hub.sh --listen 100.x.y.z:8787 --operator-capability-prefix example.com/cap/clawctl
#   # Hub 沒有正式環境預設埠。8787 只是慣例範例。grant dst、CLAWCTL_PUBLIC_URL、agent --hub、tunnel origin 必須與 CLAWCTL_LISTEN 同一個埠。省略 --listen 且 tailscale ip -4 成功時，安裝器才填 $TS_IPV4:8787。
#
# ---------------------------------------------------------------------------
# ⚠ 這支腳本存在的理由，是因為那份文件從來沒有被執行過。
#
# docs/PHASE1.md §2.2 原本是四行手打指令。2026-09-04 第一次把它們丟到一台
# 真的乾淨的 Ubuntu 24.04 上逐字跑，**四行全部失敗**，總耗時 0 秒 ——
# 它不是「超過 30 分鐘」，它是連第一步都跨不過去。細節見 PHASE1.md §5.22。
#
# 那四行每一行都曾經在某個時間點是對的。它們過期，是因為**沒有任何東西
# 會執行它們** —— unit 檔改了、Makefile 的輸出目錄改了，文件不會跟著紅。
# 一段沒有人跑的安裝步驟，跟一支沒有人跑的測試是同一種東西。
#
# 所以現在安裝步驟是一支腳本，而不是一段文字：腳本會壞，文字只會過期。
# ---------------------------------------------------------------------------

set -euo pipefail

BIN_SRC="" ; BUNDLE_SRC="" ; LISTEN="" ; CAP_PREFIX="${CLAWCTL_OPERATOR_CAPABILITY_PREFIX:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BIN_SRC="$2"; shift 2 ;;
    --agent-bundles) BUNDLE_SRC="$2"; shift 2 ;;
    --listen) LISTEN="$2"; shift 2 ;;
    --operator-capability-prefix) CAP_PREFIX="$2"; shift 2 ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

HERE="$(cd "$(dirname "$0")" && pwd)"

# --- 0. 不准用 root，也不信 caller 自己塞的 HOME
#
# Hub 是 systemd user unit；所有安裝路徑都必須先綁定到當前 EUID 在
# passwd DB 裡的單一 home。否則 fake HOME 可以讓這支腳本 chmod 或安裝到
# caller 指定的另一棵樹。
if [[ $EUID -eq 0 ]]; then
  echo "⚠ Do not run this as root. Hub is a systemd --user unit — running as root will" >&2
  echo "  cause the database to be placed under /root, and you will see an empty inventory with your own user" >&2
  exit 2
fi

verify_canonical_home() {
  local rows account_name passwd_marker account_uid account_gid gecos account_home account_shell resolved
  mapfile -t rows < <(/usr/bin/getent passwd "$EUID")
  if [[ ${#rows[@]} -ne 1 ]]; then
    echo "✗ Cannot obtain unique passwd home for EUID=$EUID; refusing to install" >&2
    return 1
  fi
  IFS=: read -r account_name passwd_marker account_uid account_gid gecos account_home account_shell <<<"${rows[0]}"
  if [[ "$account_uid" != "$EUID" || -z "$account_home" || "$account_home" != /* ||
        -L "$account_home" || ! -d "$account_home" || ! -O "$account_home" ]]; then
    echo "✗ passwd home is not a non-symlink absolute directory owned by current user" >&2
    return 1
  fi
  if [[ "$HOME" != "$account_home" ]]; then
    echo "✗ caller HOME=$HOME does not match canonical home for EUID=$EUID; no installation paths created or modified" >&2
    return 1
  fi
  if ! resolved="$(/usr/bin/realpath -e -- "$HOME")" || [[ "$resolved" != "$account_home" ]]; then
    echo "✗ HOME realpath does not match passwd home; no installation paths created or modified" >&2
    return 1
  fi
}

if ! verify_canonical_home; then
  exit 1
fi

BIN_DIR="$HOME/.local/bin"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT="$UNIT_DIR/clawctl-hub.service"
CONFIG_DIR="$HOME/.config/clawctl"
ENV_FILE="$CONFIG_DIR/hub.env"
OPERATOR_CONFIG_HOME=""
OPERATOR_CONFIG_DIR=""
OPERATOR_CONFIG=""
STATE_DIR="$HOME/.local/share/clawctl"

resolve_operator_config_paths() {
  local configured resolved
  configured="${XDG_CONFIG_HOME:-$HOME/.config}"
  if [[ "$configured" != /* ]] ||
     ! resolved="$(/usr/bin/realpath -m -- "$configured")" ||
     [[ "$resolved" != "$configured" ]]; then
    echo "✗ XDG operator config root must be a canonical absolute path: $configured" >&2
    return 1
  fi
  OPERATOR_CONFIG_HOME="$configured"
  OPERATOR_CONFIG_DIR="$OPERATOR_CONFIG_HOME/clawctl"
  OPERATOR_CONFIG="$OPERATOR_CONFIG_DIR/operator.json"
}

reject_unsafe_existing_directory() {
  local path="$1" label="$2" resolved
  if [[ -L "$path" ]]; then
    echo "✗ $label must not be a symlink: $path" >&2
    return 1
  fi
  if [[ -e "$path" && ( ! -d "$path" || ! -O "$path" ) ]]; then
    echo "✗ $label must be a directory owned by current user: $path" >&2
    return 1
  fi
  if [[ -e "$path" ]] &&
     { ! resolved="$(/usr/bin/realpath -e -- "$path")" || [[ "$resolved" != "$path" ]]; }; then
    echo "✗ $label must be a canonical non-symlink directory: $path" >&2
    return 1
  fi
}

ensure_owned_canonical_directory() {
  local path="$1" label="$2" permissions
  reject_unsafe_existing_directory "$path" "$label" || return 1
  if [[ ! -e "$path" ]] && ! mkdir -m 0700 -- "$path"; then
    echo "✗ Cannot create $label: $path" >&2
    return 1
  fi
  reject_unsafe_existing_directory "$path" "$label" || return 1
  permissions="$(stat -c '%a' -- "$path")" || return 1
  if (( (8#$permissions & 8#022) != 0 )); then
    echo "✗ $label is writable by group/other: $path ($permissions)" >&2
    return 1
  fi
}

ensure_private_owned_directory() {
  local path="$1" label="$2" resolved
  reject_unsafe_existing_directory "$path" "$label" || return 1
  if [[ ! -e "$path" ]] && ! mkdir -m 0700 -- "$path"; then
    echo "✗ Cannot create $label: $path" >&2
    return 1
  fi
  # mkdir 之後再 lstat/realpath：即使 final component 在檢查後被換成
  # symlink，也會在 chmod 之前關閉，不會改到目標 inode 的 mode。
  if [[ ! -d "$path" || -L "$path" || ! -O "$path" ]] ||
     ! resolved="$(/usr/bin/realpath -e -- "$path")" ||
     [[ "$resolved" != "$path" ]]; then
    echo "✗ $label is not a canonical non-symlink directory owned by current user: $path" >&2
    return 1
  fi
  chmod 0700 -- "$path"
}

ensure_operator_config_directory() {
  local parent
  reject_unsafe_existing_directory "$OPERATOR_CONFIG_HOME" "operator config root" || return 1
  if [[ ! -e "$OPERATOR_CONFIG_HOME" ]]; then
    parent="$(dirname -- "$OPERATOR_CONFIG_HOME")"
    ensure_owned_canonical_directory "$parent" "operator config root parent" || return 1
  fi
  ensure_owned_canonical_directory "$OPERATOR_CONFIG_HOME" "operator config root" || return 1
  ensure_private_owned_directory "$OPERATOR_CONFIG_DIR" "operator config directory"
}

if ! resolve_operator_config_paths; then
  exit 2
fi

# Operator auth 的 destination 必須是 literal tailnet IP；prefix 必須跟先存進
# Tailscale grants 的 app capability namespace 完全相同。重跑安裝時沿用既有值。
if [[ -z "$LISTEN" && -f "$ENV_FILE" ]]; then
  LISTEN="$(sed -n 's/^CLAWCTL_LISTEN=//p' "$ENV_FILE" | tail -1 | tr -d '"')"
fi
if [[ -z "$CAP_PREFIX" && -f "$ENV_FILE" ]]; then
  CAP_PREFIX="$(sed -n 's/^CLAWCTL_OPERATOR_CAPABILITY_PREFIX=//p' "$ENV_FILE" | tail -1 | tr -d '"')"
fi
# 8787 is the conventional example port, not a Hub default. Grants,
# CLAWCTL_PUBLIC_URL, agent --hub, and a tunnel origin must use this same port.
if [[ -z "$LISTEN" ]] && command -v tailscale >/dev/null 2>&1; then
  TS_IPV4="$(tailscale ip -4 2>/dev/null | sed -n '1p')"
  [[ -z "$TS_IPV4" ]] || LISTEN="$TS_IPV4:8787"
fi
if [[ -z "$LISTEN" || -z "$CAP_PREFIX" ]]; then
  cat >&2 <<EOF
Missing operator auth installation configuration. Hub now only accepts explicit Tailscale listener IP,
and all UI/API require Tailscale grants app capability:

  ./ops/install-hub.sh \\
    --listen 100.x.y.z:8787 \\
    --operator-capability-prefix example.com/cap/clawctl

8787 is only a conventional example port, not Hub default. grant dst, CLAWCTL_PUBLIC_URL, agent --hub, tunnel origin must use the same port as --listen.
Please add the three capability grants to Tailscale per docs/OPERATOR-AUTH.md first;
the installation script will not modify tailnet policy for you, nor fall back to unauthenticated loopback console.
EOF
  exit 2
fi

# --- 1. binary 從哪來
if [[ -z "$BIN_SRC" ]]; then
  BIN_SRC="$HERE/../build/clawctl-hub"
fi
if [[ -z "$BUNDLE_SRC" ]]; then
  BUNDLE_SRC="$HERE/../build"
fi
if [[ ! -f "$BIN_SRC" ]]; then
  cat >&2 <<EOF
Cannot find clawctl-hub binary: $BIN_SRC

To build yourself, this machine requires Go $(grep -m1 '^go ' "$HERE/../go.mod" 2>/dev/null | awk '{print $2}') (specified in go.mod).
Ubuntu apt version is too old, do not use apt install golang-go:

  sudo apt-get update && sudo apt-get install -y make ca-certificates curl
  curl -fsSL https://go.dev/dl/go1.27.1.linux-amd64.tar.gz | tar -C "\$HOME/.local" -xzf -
  make            # Makefile will look for ~/.local/go/bin/go

⚠ Or do not build on this machine at all. Artifact is a CGO_ENABLED=0 static executable —
   build on another machine, scp over, then pass with --binary.
EOF
  exit 1
fi
BUNDLE_VERSION="$(timeout 5 "$BIN_SRC" version 2>/dev/null)" || {
  echo "clawctl-hub binary cannot report version" >&2
  exit 1
}
if [[ ! "$BUNDLE_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]]; then
  echo "clawctl-hub binary version format invalid" >&2
  exit 1
fi
for arch in amd64 arm64; do
  bundle="$BUNDLE_SRC/clawctl-agent-bootstrap-linux-$arch.tar.gz"
  [[ -f "$bundle" && ! -L "$bundle" ]] || { echo "Cannot find Agent bootstrap: $bundle" >&2; exit 1; }
done
if [[ -e "$BUNDLE_SRC/clawctl-agent-bootstrap-darwin-amd64.tar.gz" || -L "$BUNDLE_SRC/clawctl-agent-bootstrap-darwin-amd64.tar.gz" ||
      -e "$BUNDLE_SRC/clawctl-agent-bootstrap-darwin-arm64.tar.gz" || -L "$BUNDLE_SRC/clawctl-agent-bootstrap-darwin-arm64.tar.gz" ]]; then
  for arch in amd64 arm64; do
    bundle="$BUNDLE_SRC/clawctl-agent-bootstrap-darwin-$arch.tar.gz"
    [[ -f "$bundle" && ! -L "$bundle" ]] || { echo "Cannot find Agent bootstrap: $bundle" >&2; exit 1; }
  done
fi
if [[ -e "$BUNDLE_SRC/clawctl-agent-bootstrap-windows-amd64.tar.gz" || -L "$BUNDLE_SRC/clawctl-agent-bootstrap-windows-amd64.tar.gz" ||
      -e "$BUNDLE_SRC/clawctl-agent-bootstrap-windows-arm64.tar.gz" || -L "$BUNDLE_SRC/clawctl-agent-bootstrap-windows-arm64.tar.gz" ]]; then
  for arch in amd64 arm64; do
    bundle="$BUNDLE_SRC/clawctl-agent-bootstrap-windows-$arch.tar.gz"
    [[ -f "$bundle" && ! -L "$bundle" ]] || { echo "Cannot find Agent bootstrap: $bundle" >&2; exit 1; }
  done
fi

# --- 2. 已經有一個 Hub 了嗎
#
# ⚠⚠ 這一條擋的是這個 repo 裡最貴的那個失效：兩個 Hub 開同一個 SQLite。
#
# 它不是假想的。docs/PHASE1.md §2.4 原本教人在服務已經跑著的時候執行
# `clawctl-hub --notify-cmd ...` —— 當時 Hub 的 serve 模式是一組沒有
# `serve` 子指令的 flags。於是那行在當時的 binary **一定會成功地**開起
# 第二個 Hub：unit 用 8787、CLI 預設 8770，兩個數字剛好不一樣，所以
# 「address already in use」這個唯一會救你的錯誤永遠不會發生。兩個 Hub、
# 同一個資料庫、都是綠燈。（§5.22）目前 binary 已在開 DB 前做 listener 與
# operator auth preflight；這裡仍保留 process-level 的第二道防線。
#
# ⚠ 順序：這一段跟上面的 binary 檢查都是**唯讀**的，所以放在 linger 前面。
#   一支會在第 4 步失敗、卻已經在第 2 步改了機器設定的安裝腳本，
#   會讓「它失敗了所以什麼都沒發生」變成一句假話。
#
# ⚠ 用 `ps -eo comm` + `grep -x`，不用 `pgrep -f` —— 理由見 install-agent.sh
#   第 100 行：pgrep -f 會把發出查詢的那條指令自己算進去。
HUBS="$(ps -eo comm | grep -cx clawctl-hub || true)"
if [[ "$HUBS" -gt 0 ]]; then
  cat >&2 <<EOF
⚠ Already $HUBS clawctl-hub processes running on this machine.

Two Hubs opening the same SQLite will both show green, but judgements will flap back and forth. First check what they are:

  ps -eo pid,etime,args | awk '\$3 ~ /clawctl-hub/'

If upgrading an old version, use ops/upgrade-hub.sh (it stops service, backs up, verifies, rollbacks if needed),
do not use this script. This script is for clean machines.
EOF
  exit 1
fi

# --- 3. linger 要在 systemctl --user 之前
#
# ⚠⚠ 順序是這支腳本裡唯一一個「看起來可以換、其實不行」的東西。
#
# 原本文件把 enable-linger 放在 `systemctl --user enable --now` 的**後面**。
# 在一台有圖形登入的機器上那沒差；在一台只有 ssh 進去的 server 上，
# linger 正是「user manager 存不存在」的那個開關 —— 沒有它，
# 前一行會拿到 `Failed to connect to bus: No medium found`，
# 而那個訊息不會有任何一個字提到 linger。
#
# ops/install-agent.sh 早就是先 linger 再 unit（它第 53 行）。
# 兩支安裝腳本對同一件事有兩種順序，就是其中一支是錯的。
LINGER=0
if loginctl show-user "$USER" --property=Linger 2>/dev/null | grep -q 'Linger=yes'; then
  LINGER=1; echo "✓ linger enabled"
elif loginctl enable-linger "$USER" 2>/dev/null; then
  LINGER=1; echo "✓ linger enabled"
else
  # 實測（乾淨 Ubuntu 24.04）：一般使用者跑會拿到 `Could not enable linger: Access denied`。
  # ⚠ 這一步失敗**不中止安裝**，但它的後果是延遲發作的：Hub 會在你登出的
  #    那一刻死掉，而那時候沒有人在看畫面。所以它要講得比其他步驟大聲。
  cat >&2 <<EOF

⚠⚠ Cannot enable linger (tested message: Could not enable linger: Access denied). Please run:

    sudo loginctl enable-linger $USER

Without linger, Hub will stop the moment you log out — and quietly.
The entire fleet will lose connection simultaneously, daily reports will not arrive, deadman switch will trigger tomorrow.
EOF
fi

# --- 4. user bus 通不通
#
# ⚠ 這一段是先問「等一下那些 systemctl --user 有沒有機會成功」，
# 而不是等它們失敗之後叫人去猜。實測的兩種訊息都很難從字面連回原因：
#   Failed to connect to bus: No medium found        （XDG_RUNTIME_DIR 沒設）
#   Failed to connect to bus: No such file or directory（設了但 user manager 沒起來）
if ! systemctl --user is-system-running >/dev/null 2>&1; then
  case "$(systemctl --user is-system-running 2>&1)" in
    *"connect to bus"*)
      cat >&2 <<EOF
⚠ systemctl --user cannot connect to user bus (XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-<empty>}).

This almost always means "this session was not established through pam_systemd". First verify:

    dpkg -l libpam-systemd            # if not installed user manager cannot start
    loginctl list-users               # your account must be listed

Then log in again. Do not manually export XDG_RUNTIME_DIR — that creates
an environment variable pointing to a non-existent bus, causing systemctl --user to fail in a different way.
EOF
      exit 1 ;;
  esac
fi

# --- 5. 目錄（乾淨機器上這些都不存在）
# writer lock 必須在 Store.Open 前取得，所以 state parent 不能再靠 SQLite
# 開檔時順手建立。hub.env 仍固定在 ~/.config；operator.json 則跟
# os.UserConfigDir 一樣尊重 XDG_CONFIG_HOME。先檢查所有 final target，
# 再逐層建 parent；不對未驗證的 symlink 做 mkdir -p 或 chmod。
for directory in \
  "$HOME/.config" "$HOME/.config/systemd" "$UNIT_DIR" "$CONFIG_DIR" \
  "$HOME/.local" "$BIN_DIR" "$HOME/.local/share" "$STATE_DIR"
do
  reject_unsafe_existing_directory "$directory" "install directory" || exit 1
done
reject_unsafe_existing_directory "$CONFIG_DIR" "Hub config directory" || exit 1
reject_unsafe_existing_directory "$STATE_DIR" "Hub state directory" || exit 1
reject_unsafe_existing_directory "$OPERATOR_CONFIG_HOME" "operator config root" || exit 1
if [[ ! -e "$OPERATOR_CONFIG_HOME" ]]; then
  reject_unsafe_existing_directory "$(dirname -- "$OPERATOR_CONFIG_HOME")" "operator config root parent" || exit 1
fi
if [[ "$OPERATOR_CONFIG_DIR" != "$CONFIG_DIR" ]]; then
  reject_unsafe_existing_directory "$OPERATOR_CONFIG_DIR" "operator config directory" || exit 1
fi
ensure_owned_canonical_directory "$HOME/.config" "config root" || exit 1
ensure_owned_canonical_directory "$HOME/.config/systemd" "systemd config directory" || exit 1
ensure_owned_canonical_directory "$UNIT_DIR" "systemd user unit directory" || exit 1
ensure_private_owned_directory "$CONFIG_DIR" "Hub config directory" || exit 1
ensure_owned_canonical_directory "$HOME/.local" "local data root" || exit 1
ensure_owned_canonical_directory "$BIN_DIR" "local binary directory" || exit 1
ensure_owned_canonical_directory "$HOME/.local/share" "local state root" || exit 1
ensure_private_owned_directory "$STATE_DIR" "Hub state directory" || exit 1
ensure_operator_config_directory || exit 1

"$HERE/publish-agent-bundles.sh" --version "$BUNDLE_VERSION" \
  --source-dir "$BUNDLE_SRC" --state-dir "$STATE_DIR"

# --- 6. 裝 binary
#
# ⚠ 一定是 $HOME/.local/bin，因為 unit 的 ExecStart 寫的是 %h/.local/bin。
# 文件原本叫人裝到 /usr/local/bin —— 兩邊各自看都對，放在一起才是錯的，
## 而這個 repo 裡沒有任何東西會同時讀那兩個檔案。現在有了：
# cmd/clawctl-hub/install_doc_test.go。
install -m 0755 "$BIN_SRC" "$BIN_DIR/clawctl-hub"
echo "✓ binary → $BIN_DIR/clawctl-hub ($("$BIN_DIR/clawctl-hub" version 2>/dev/null || echo 'cannot read version'))"

# --- 7. unit
install -m 0644 "$HERE/clawctl-hub.service" "$UNIT"
# ⚠ 寫進 hub.env 而不是改 unit：換版時 unit 會被覆蓋，hub.env 不會。
#   （而且 hub.env 是 0600，之後要放 bot token 的也是它。）
touch "$ENV_FILE"; chmod 600 "$ENV_FILE"
if grep -q '^CLAWCTL_LISTEN=' "$ENV_FILE" 2>/dev/null; then
  sed -i "s|^CLAWCTL_LISTEN=.*|CLAWCTL_LISTEN=$LISTEN|" "$ENV_FILE"
else
  printf 'CLAWCTL_LISTEN=%s\n' "$LISTEN" >> "$ENV_FILE"
fi
if grep -q '^CLAWCTL_OPERATOR_CAPABILITY_PREFIX=' "$ENV_FILE" 2>/dev/null; then
  sed -i "s|^CLAWCTL_OPERATOR_CAPABILITY_PREFIX=.*|CLAWCTL_OPERATOR_CAPABILITY_PREFIX=$CAP_PREFIX|" "$ENV_FILE"
else
  printf 'CLAWCTL_OPERATOR_CAPABILITY_PREFIX=%s\n' "$CAP_PREFIX" >> "$ENV_FILE"
fi
echo "✓ CLAWCTL_LISTEN=$LISTEN → $ENV_FILE"
echo "✓ CLAWCTL_OPERATOR_CAPABILITY_PREFIX=$CAP_PREFIX → $ENV_FILE"

# ⚠ daemon-reload 不能省。剛複製進來的 unit 對 user manager 來說還不存在，
#   直接 enable 會拿到 `Failed to enable unit: Unit file clawctl-hub.service
#   does not exist.` —— 一個聽起來像「檔案沒複製成功」的訊息。
systemctl --user daemon-reload
systemctl --user enable --now clawctl-hub.service
echo "✓ unit enabled and started"

# --- 8. 驗收
#
# ⚠⚠ 刻意**不**拿 `systemctl is-active` 當成功判準。
#
# 這支腳本走到這裡為止，證明的全部是「我把檔案放對了位置」——
# 那是自證。真正的問題是「它有沒有在回答 HTTP」，而那要去問它。
#
# ⚠ 用 bash 的 /dev/tcp，不用 curl：一台乾淨的 Ubuntu 沒有 curl
#   （實測 24.04 base：curl / wget 都沒有）。一個要求你先裝工具才能驗證的
#   驗證步驟，會在最該跑的那次被跳過。
# systemctl show 不會可靠展開 EnvironmentFile 的執行期值；本輪剛把 LISTEN
# 寫入 hub.env，所以驗收直接使用同一個已明示輸入。
ADDR="$LISTEN"
HOST="${ADDR%:*}" ; PORT="${ADDR##*:}"
# net.SplitHostPort 要求 IPv6 有中括號，但 bash /dev/tcp 要的 host
# 本體不含中括號。IPv4 這兩行不會改到任何東西。
HOST="${HOST#[}" ; HOST="${HOST%]}"

# ⚠ 那個 ( ) 子殼層是防禦性的，而它的來歷值得寫下來，因為我一開始把原因搞錯了。
#
# 觀察到的是：這支腳本有一次在「✓ unit 已啟用並啟動」之後**安靜地 exit 1**，
# 連下面那句「✗ 沒有回 alive」都沒印。我的第一個診斷是
# 「`exec 3<>/dev/tcp/...` 失敗會讓非互動 shell 直接結束」。
#
# 那個機制**確實存在**，但它不適用於原本那段程式碼。實測（bash 5.2）：
#
#   exec 3<>/dev/tcp/127.0.0.1/1        # 裸的 → shell 當場結束，後面一行都不跑
#   if exec 3<>/dev/tcp/127.0.0.1/1; …  # 在 if 的條件裡 → 不致命，繼續跑
#
# 原本寫的正是第二種。所以那個診斷是錯的 —— 而我差一點就把它寫成註解留在這裡，
# 讓下一個人去修一個不存在的問題。後來查出那次 exit 1 是容器裡的腳本是舊的複本，
# 跟這段邏輯無關。
#
# ⚠⚠ 留下子殼層的理由**不是**那個診斷，是這兩件實際的好處：
#   1. fd 3 的生命週期關在子殼層裡，不必手動 `exec 3<&- 3>&-`（漏掉會留著半開的 socket）
#   2. 就算哪天有人把它從 `if` 的條件裡搬出去，那個 fatal exit 也被關住了
#
# 記在這裡是因為「我下錯了一個診斷」跟 bug 本身一樣值得留（見 §5.8）。
ok=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if ( exec 3<>"/dev/tcp/$HOST/$PORT"
       printf 'GET /healthz HTTP/1.0\r\nHost: %s\r\n\r\n' "$ADDR" >&3
       grep -q '^alive' <&3 ) 2>/dev/null; then
    ok=1; break
  fi
  sleep 1
done

if [[ "$ok" -ne 1 ]]; then
  cat >&2 <<EOF

✗ Service started, but http://$ADDR/healthz did not return alive.

  systemctl --user status clawctl-hub --no-pager
  journalctl --user -u clawctl-hub -n 50 --no-pager

⚠ Worth checking more than "startup failure": unit is active, so anyone
  who only checks systemctl will think it succeeded.
EOF
  exit 1
fi

# healthz 刻意不走 operator auth，因為 agent/monitoring 不可以被 human auth
# outage 拖垮；所以還要真的打首頁，否則「service 綠、管理者全被 403」會被誤報成功。
operator_ok=0
for _ in 1 2 3 4 5; do
  if ( exec 3<>"/dev/tcp/$HOST/$PORT"
       printf 'GET / HTTP/1.0\r\nHost: %s\r\n\r\n' "$ADDR" >&3
       IFS= read -r status <&3
       [[ "$status" == *" 200 "* ]] ) 2>/dev/null; then
    operator_ok=1; break
  fi
  sleep 1
done
if [[ "$operator_ok" -ne 1 ]]; then
  cat >&2 <<EOF

✗ /healthz ok, but operator homepage did not return 200.

This usually indicates Tailscale grant does not yet include $CAP_PREFIX-view, or current node/user is not the grant src.
Please verify the grant in docs/OPERATOR-AUTH.md using Tailscale policy editor, then check:

  journalctl --user -u clawctl-hub -n 50 --no-pager

Script will not declare installation successful with an inaccessible console just because healthz is green.
EOF
  exit 1
fi

# CLI 的 normal path 與瀏覽器使用同一個已驗收 authority。agent.json 是 machine
# bearer 的設定，不是 operator trust anchor；hub.env 則由 systemd parser 擁有。
# 因此另寫一個單欄位、無秘密的 operator.json，並以同目錄 atomic rename 取代。
OPERATOR_TMP="$(mktemp "$OPERATOR_CONFIG_DIR/.operator.json.XXXXXX")"
if ! chmod 0600 "$OPERATOR_TMP" ||
   ! printf '{"hub_url":"http://%s"}\n' "$ADDR" >"$OPERATOR_TMP" ||
   ! mv -fT -- "$OPERATOR_TMP" "$OPERATOR_CONFIG"; then
  rm -f -- "$OPERATOR_TMP"
  echo "✗ Hub started, but cannot safely write CLI discovery: $OPERATOR_CONFIG" >&2
  exit 1
fi
echo "✓ CLI operator discovery → $OPERATOR_CONFIG"

RESTARTS="$(systemctl --user show clawctl-hub.service -p NRestarts --value)"
[[ "$RESTARTS" == "0" ]] || echo "⚠ Restarted $RESTARTS times already: journalctl --user -u clawctl-hub -n 50" >&2

# ⚠⚠ 沒有 linger 的話，**不准說「裝好了」**。
#
# 這不是措辭潔癖。2026-09-04 在乾淨機器上實測，linger 關著時：
# 這支腳本印出「裝好了」、healthz 回 alive、每一項都是綠的 ——
# 然後 ssh session 一結束，`ps` 就再也找不到那個 process。
# 從頭到尾沒有任何一則錯誤訊息。
#
# 一個「安裝成功」而在你關掉終端機的那一刻就蒸發的東西，
# 它的成功訊息比失敗還糟：失敗你會回頭看，成功你會走開。
if [[ "$LINGER" -ne 1 ]]; then
  cat >&2 <<EOF

⚠⚠ Running now (http://$ADDR/healthz returned alive), but it will not survive logout.

linger is not enabled, so your user manager will be terminated when session ends,
Hub along with it — quietly, without error messages. Tested behavior: install fully green,
once session ends \`ps\` can no longer find it.

    sudo loginctl enable-linger $USER
    systemctl --user start clawctl-hub

⚠ Complete these two lines before closing this window. Without this step, every ✓ above only holds within this session.
EOF
  exit 1
fi

cat <<EOF

Installed; http://$ADDR/healthz=alive, operator homepage passed Tailscale app-cap verification.

  Open:   http://$ADDR/
  Fleet:  $BIN_DIR/clawctl-hub machines
  Logs:   journalctl --user -u clawctl-hub -f

Next steps:

  1. Currently bound to Tailscale address $ADDR only. Do not change to 0.0.0.0, LAN/public IP or
     localhost; Hub will refuse to start, operator authority must not be guessed from HTTP Host.

  2. Open http://$ADDR/ from another authorized tailnet device first,
     verify login, device and capability in top bar. Then issue a token:
       $BIN_DIR/clawctl-hub enroll-token <machine-name>

  3. Daily reports and deadman switch: write settings to $ENV_FILE (0600), then restart.
     ⚠ Do not run another serve process while service is running. Bare
       clawctl-hub --notify-cmd now fails before opening DB due to loopback/missing app-cap;
       but specifying valid tailnet listener and prefix can still produce a second writer.
       To change settings edit hub.env and restart unit.

       # Preferred: built-in Telegram/webhook channels (ops/notify.env.example).
       CLAWCTL_NOTIFY_ENV=\$HOME/.config/clawctl/notify.env
       # Legacy command (built-in channels in that file are then unused):
       # CLAWCTL_NOTIFY_CMD=\$HOME/.local/libexec/clawctl/notify-telegram.sh
       # ops/notify-telegram.sh stays for ops/deadman.sh and Alertmanager.

  4. ⚠ Hub must not be installed on machines it manages, nor monitor itself.
     External deadman switch must run on another machine: ops/deadman.sh
EOF
