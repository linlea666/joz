#!/usr/bin/env bash
set -euo pipefail
PROJECT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
export PROJECT_DIR
source "$PROJECT_DIR/scripts/deploy-common.sh"
cd "$PROJECT_DIR"

case "${1:-start}" in
 help|--help|-h)
    printf '%s\n' '用法：./start.sh [start [--build]|update|restart|stop|status|logs [服务名]|clean|regenerate-keys]' '更新：cd /opt/nofx && ./start.sh update' '安装及更新不会自动启用实盘。clean 和 regenerate-keys 是独立破坏性操作。'
    exit 0 ;;
esac
check_docker
case "${1:-start}" in
 update)
    lock_deployment
    check_repo
    git fetch origin main
    git merge-base --is-ancestor HEAD origin/main || fail '本地 main 有未在 origin/main 的提交；停止更新'
    git merge --ff-only origin/main
    # Re-execute the updated implementation; FD 9 retains the deployment lock.
    exec bash "$PROJECT_DIR/start.sh" --continue-update "$(git rev-parse HEAD)"
    ;;
 --continue-update)
    [[ -e /dev/fd/9 ]] || fail '此入口只允许由 update 调用'
    flock -n 9 || fail '部署锁丢失'
    check_repo
    [[ "$(git rev-parse HEAD)" == "${2:-}" ]] || fail '更新版本已变化'
    deploy_services verify
    ;;
 start)
    lock_deployment
    # On a first source deployment build all three services; --build stays compatible.
    deploy_services
    ;;
 restart|stop)
    lock_deployment
    [[ -f .env ]] || fail '尚未安装'
    if [[ "$1" == stop ]]; then compose stop; else compose restart; compose up -d --no-build --wait --wait-timeout 180; verify_services; fi
    ;;
 status)
    compose ps -a
    verify_services
    ;;
 logs)
    service=${2:-}
    [[ "$service" == backend ]] && service=nofx
    [[ "$service" == frontend ]] && service=nofx-frontend
    if [[ -n "$service" ]]; then compose logs --tail 200 -f "$service"; else compose logs --tail 200 -f; fi
    ;;
 clean)
    lock_deployment
    printf '%s\n' '将删除容器及采集持久卷（data/.env 保留）。这会丢失待确认采集事件。输入 yes 确认：'
    read -r answer
    [[ "$answer" == yes ]] || exit 1
    compose down -v
    ;;
 regenerate-keys)
    lock_deployment
    printf '%s\n' '更换密钥会使数据库里的现有凭证无法解密！应先备份 .env。输入 yes 确认：'
    read -r answer
    [[ "$answer" == yes ]] || exit 1
    python3 "$PROJECT_DIR/scripts/deploy-env.py" regenerate
    printf '%s\n' '密钥已更换；现有凭证需重新配置。容器尚未重启。'
    ;;
 *) fail "未知命令：$1" ;;
esac
