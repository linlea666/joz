#!/usr/bin/env bash
# Shared by repository deployments, always rooted at the script checkout.
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
compose() { docker compose --project-directory "$PROJECT_DIR" --env-file "$PROJECT_DIR/.env" -f "$PROJECT_DIR/docker-compose.yml" -p nofx "$@"; }
check_repo() {
    [[ "$(git -C "$PROJECT_DIR" rev-parse --show-toplevel)" == "$PROJECT_DIR" ]] || fail '目标不是独立项目仓库'
    local remote
    remote=$(git -C "$PROJECT_DIR" remote get-url origin)
    case "$remote" in
        https://github.com/linlea666/joz|https://github.com/linlea666/joz.git|git@github.com:linlea666/joz.git|ssh://git@github.com/linlea666/joz.git) ;;
        *) fail 'origin 必须是 linlea666/joz；不自动修改仓库来源' ;;
    esac
    [[ "$(git -C "$PROJECT_DIR" branch --show-current)" == main ]] || fail '必须位于 main 分支'
    [[ -z "$(git -C "$PROJECT_DIR" status --porcelain --untracked-files=no)" ]] || fail '存在已跟踪文件修改；请先处理，不自动覆盖'
}
check_docker() {
    command -v docker >/dev/null || fail '缺少 Docker，请运行 install.sh'
    docker info >/dev/null 2>&1 || fail 'Docker 未启动或当前用户无权限；使用 sudo 或检查 Docker 服务'
    docker compose version >/dev/null 2>&1 || fail '需要 Docker Compose v2 插件'
    docker buildx version >/dev/null 2>&1 || fail '需要 Docker Buildx 插件'
}
lock_deployment() {
    command -v flock >/dev/null || fail '需要 flock（Ubuntu: util-linux）'
    exec 9>"$PROJECT_DIR/.git/nofx-deploy.lock"
    flock -n 9 || fail '另一个部署正在进行'
}
prepare_env() { python3 "$PROJECT_DIR/scripts/deploy-env.py" "$@"; }
check_ports() {
    # Existing project containers own their ports. First installations must bind
    # both host ports before spending time building images.
    if [[ -n "$(compose ps -aq)" ]]; then return; fi
    local ports
    ports=$(python3 "$PROJECT_DIR/scripts/deploy-env.py" ports)
    python3 - "$ports" <<'PY'
import socket, sys
sockets=[]
try:
    for port in sys.argv[1].splitlines():
        s=socket.socket()
        sockets.append(s)
        s.bind(('0.0.0.0', int(port)))
except OSError:
    sys.exit('服务端口被占用；请修改 .env 的端口或停止冲突服务。')
finally:
    for s in sockets: s.close()
PY
}
build_services() {
    # Never stop current containers before every build succeeds.
    for service in nofx nofx-frontend discord-collector; do
        compose build "$service" || fail "$service 构建失败；现有容器未停止"
    done
}
verify_services() {
    local failed=0 service id health
    for service in nofx nofx-frontend discord-collector; do
        id=$(compose ps -q "$service")
        health=missing
        if [[ -n "$id" ]]; then health=$(docker inspect --format '{{if .State.Running}}{{if .State.Health}}{{.State.Health.Status}}{{else}}running{{end}}{{else}}{{.State.Status}}{{end}}' "$id"); fi
        printf '%s: %s\n' "$service" "$health"
        [[ "$health" == healthy ]] || failed=1
    done
    [[ "$failed" == 0 ]] || fail '服务健康验收失败；运行 ./start.sh logs <服务名> 检查，不自动回滚数据库'
    # Local IPC is verified even without Discord credentials.
    compose exec -T discord-collector python healthcheck.py --describe || fail '采集器与后端通信验收失败'
}
deploy_services() {
    prepare_env "${1:-fresh}"
    compose config --quiet
    check_ports
    build_services
    if ! compose up -d --no-build --wait --wait-timeout 180; then
        compose ps -a
        fail '启动或健康检查失败；不会打印更新成功或自动回滚数据库'
    fi
    verify_services
    local ports frontend
    ports=$(python3 "$PROJECT_DIR/scripts/deploy-env.py" ports)
    frontend=${ports%%$'\n'*}
    printf '部署验收通过，版本：%s\n访问：http://<服务器IP>:%s\n邮件在 Discord 设置中独立配置。已有运行模式保持不变；全新数据库默认仅采集验证。\n' "$(git -C "$PROJECT_DIR" rev-parse --short HEAD)" "$frontend"
}
