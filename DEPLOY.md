# 服务器部署与更新指南

本项目的发布流程（服务器源码构建方式）：

```
本地修改代码 → git push 到 main → 服务器 ./start.sh update（git pull + 重新构建 + 重启容器）
```

- 代码仓库：`https://github.com/linlea666/joz`（公开，服务器拉取无需任何令牌）
- 端口：前端 `3000`，后端 API `8080`
- 服务器要求：2 核 4G 可用，但**必须先加 swap**（见下文），否则前端构建可能内存不足失败

## 一、服务器首次部署（一次性）

SSH 登录服务器（或用宝塔的「终端」），依次执行：

```bash
# 1. 加 4G swap，防止构建时内存不足（只需执行一次）
fallocate -l 4G /swapfile
chmod 600 /swapfile
mkswap /swapfile
swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab

# 2. 安装 git（如果没有）
command -v git >/dev/null 2>&1 || { command -v yum >/dev/null 2>&1 && yum install -y git || apt install -y git; }

# 3. 克隆仓库
git clone https://github.com/linlea666/joz.git /opt/nofx
cd /opt/nofx

# 4. 一键构建并启动
#    脚本会自动：复制 .env.example → 生成三个加密密钥 → 创建 data/ 目录 → 构建镜像并启动
./start.sh start --build
```

首次构建约 15~25 分钟（要编译 TA-Lib、Go 后端和前端），耐心等待。完成后验证：

```bash
./start.sh status
curl http://localhost:8080/api/health
```

然后在阿里云安全组和宝塔「安全」页放行 `3000` 和 `8080` 端口，浏览器访问 `http://服务器IP:3000`。

## 二、日常更新流程

本地：

```bash
git add -A && git commit -m "描述改动" && git push
```

服务器上一条命令：

```bash
cd /opt/nofx && ./start.sh update
```

它会自动 `git pull` 并重新构建、重启容器，数据库和配置不受影响。

注意两点：

- 构建期间（几分钟到十几分钟）服务器 CPU 会被占满，**建议避开 trader 交易活跃的时段更新**
- 也可以把这条命令存为宝塔「计划任务」里的 Shell 脚本，需要更新时手动点一次执行

## 三、常用运维命令

```bash
cd /opt/nofx
./start.sh logs            # 看全部日志（Ctrl+C 退出）
./start.sh logs nofx       # 只看后端日志
./start.sh status          # 容器状态 + 健康检查
./start.sh restart         # 重启（不重新构建）
./start.sh stop            # 停止（不删数据）
```

宝塔的 Docker →「容器」页面也能看到 `nofx-trading`（后端）和 `nofx-frontend`（前端）两个容器的状态、日志和资源占用。

磁盘维护：多次构建会积累无用的镜像层，偶尔清理一次：

```bash
docker image prune -f
```

## 四、必须备份的东西

| 内容 | 位置 | 说明 |
|------|------|------|
| 数据库 | `/opt/nofx/data/` | SQLite 库，所有交易记录、配置都在里面 |
| 密钥 | `/opt/nofx/.env` | `DATA_ENCRYPTION_KEY` 丢失后，库里加密存储的交易所 API Key 将永远无法解密 |

建议用宝塔「计划任务」定期打包 `/opt/nofx/data` 和 `/opt/nofx/.env`。
**千万不要**运行 `./start.sh clean`（会删除全部数据）和 `./start.sh regenerate-keys`（会导致已加密数据无法解密），除非明确知道自己在做什么。

## 五、安全提醒（仓库是公开的）

- `.env`、密钥文件、`data/` 数据库已被 `.gitignore` 排除，不会被提交
- 以后改代码时**绝对不要把 API Key、私钥等硬编码进源码**——公开仓库一旦推送就等于泄露
- 提交前可用 `git diff --cached` 快速过一眼要提交的内容

## 六、备用方案：直接拉预构建镜像

仓库的 GitHub Actions 会在每次 push 到 main 后自动构建镜像（`ghcr.io/linlea666/joz/nofx-backend:latest` / `nofx-frontend:latest`）。如果某次服务器构建失败或想跳过本地构建，可以改用：

```bash
cd /opt/nofx
git pull
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d
```

两种方式共用同一个 `data/` 和 `.env`，可随时切换（切换前先 `docker compose down` 停掉原来的容器组合）。

## 七、绑定域名 + HTTPS（可选）

1. 宝塔新建站点绑定域名，设置「反向代理」到 `http://127.0.0.1:3000`
2. 宝塔 SSL 页申请 Let's Encrypt 证书
3. `.env` 中设置 `TRANSPORT_ENCRYPTION=true` 后重启容器
4. 之后可在安全组关闭 3000/8080 的公网访问，只走 443
