#!/usr/bin/env bash
# Ubuntu 24.04 amd64 source installation. Never removes existing runtime data.
set -euo pipefail
umask 077
fail() { printf '安装失败：%s\n' "$*" >&2; exit 1; }
[[ "$(id -u)" == 0 ]] || fail '请使用 sudo bash install.sh [安装目录]'
[[ -r /etc/os-release ]] || fail '无法识别操作系统'
source /etc/os-release
[[ "${ID:-}" == ubuntu && "${VERSION_ID:-}" == 24.04 ]] || fail '首期支持 Ubuntu 24.04 LTS'
[[ "$(uname -m)" == x86_64 ]] || fail '首期支持 amd64'
TARGET=${1:-/opt/nofx}
[[ "$TARGET" == /* && "$TARGET" != / ]] || fail '安装目录必须为绝对路径，且不能是根目录'
mkdir -p "$(dirname "$TARGET")"
command -v flock >/dev/null || fail '缺少 util-linux/flock'
exec 8>"$(dirname "$TARGET")/.nofx-install.lock"
flock -n 8 || fail '另一个安装正在进行'
available=$(df -Pk "$(dirname "$TARGET")" | awk 'END {print $4}')
(( available >= 6*1024*1024 )) || fail '至少需要 6 GiB 可用磁盘（建议预留更多用于构建）'
memory=$(awk '/MemTotal/ {print $2}' /proc/meminfo)
(( memory >= 2*1024*1024 )) || fail '至少需要 2 GiB 内存'
(( memory >= 4*1024*1024 )) || printf '%s\n' '内存小于 4 GiB，将顺序构建。资源不足时请增加内存或 swap 后重试。'
if [[ -e "$TARGET" && ! -d "$TARGET/.git" ]]; then
    [[ -d "$TARGET" && -z "$(find "$TARGET" -mindepth 1 -maxdepth 1 -print -quit)" ]] || fail '目标目录已有其他内容，不覆盖'
fi
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl git openssl python3
if ! command -v docker >/dev/null; then
    install -m 0755 -d /etc/apt/keyrings
    curl --fail --silent --show-error --location https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
    cat > /etc/apt/sources.list.d/docker.sources <<'SOURCES'
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Architectures: amd64
Signed-By: /etc/apt/keyrings/docker.asc
SOURCES
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    systemctl enable --now docker
fi
# Do not replace a working Docker installation or silently remove packages.
docker info >/dev/null 2>&1 || fail 'Docker 已存在但不可用，请检查 systemctl status docker'
docker compose version >/dev/null 2>&1 || fail '请安装 Docker Compose v2 插件后重试'
if [[ ! -d "$TARGET/.git" ]]; then
    git clone --branch main --single-branch https://github.com/linlea666/joz.git "$TARGET"
fi
PROJECT_DIR=$(cd "$TARGET" && pwd -P)
# Validate origin before sourcing any code from an existing target directory.
case "$(git -C "$PROJECT_DIR" remote get-url origin)" in
    https://github.com/linlea666/joz|https://github.com/linlea666/joz.git|git@github.com:linlea666/joz.git|ssh://git@github.com/linlea666/joz.git) ;;
    *) fail '目标不是 linlea666/joz 仓库' ;;
esac
source "$PROJECT_DIR/scripts/deploy-common.sh"
check_repo
# Existing installations are only rebuilt; use start.sh update for a fast-forward.
exec bash "$PROJECT_DIR/start.sh" start --build
