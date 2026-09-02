# 服务器部署与更新指南

本项目的发布流程：

```
本地修改代码 → git push 到 main → GitHub Actions 自动构建镜像并推送到 ghcr.io
→ 服务器 git pull + docker compose pull 拉取新镜像 → 重启容器完成更新
```

- 代码仓库：`https://github.com/linlea666/joz`（私有）
- 后端镜像：`ghcr.io/linlea666/joz/nofx-backend:latest`
- 前端镜像：`ghcr.io/linlea666/joz/nofx-frontend:latest`
- 端口：前端 `3000`，后端 API `8080`

## 一、准备 GitHub 访问令牌（一次性）

服务器需要一个 Personal Access Token (PAT) 来克隆私有仓库和拉取私有镜像。

1. 打开 GitHub → Settings → Developer settings → [Personal access tokens (classic)](https://github.com/settings/tokens/new)
2. 勾选权限：`repo`（克隆私有仓库）+ `read:packages`（拉取镜像）
3. 生成后保存好令牌（形如 `ghp_xxxx`），下文用 `<PAT>` 指代

## 二、服务器首次部署（一次性）

SSH 登录服务器（或用宝塔的「终端」），依次执行：

```bash
# 1. 克隆仓库（令牌写入 remote，之后 git pull 免密）
git clone https://linlea666:<PAT>@github.com/linlea666/joz.git /opt/nofx
cd /opt/nofx

# 2. 生成 .env（密钥自动生成，只需执行一次，之后千万不要删除或重新生成）
JWT_SECRET=$(openssl rand -base64 32)
DATA_ENCRYPTION_KEY=$(openssl rand -base64 32)
RSA_PRIVATE_KEY=$(openssl genrsa 2048 2>/dev/null | tr '\n' '\\' | sed 's/\\/\\n/g' | sed 's/\\n$//')
cat > .env << EOF
NOFX_BACKEND_PORT=8080
NOFX_FRONTEND_PORT=3000
TZ=Asia/Shanghai
JWT_SECRET=${JWT_SECRET}
DATA_ENCRYPTION_KEY=${DATA_ENCRYPTION_KEY}
RSA_PRIVATE_KEY=${RSA_PRIVATE_KEY}
EOF
chmod 600 .env

# 3. 登录镜像仓库（一次性，凭证会保存在 ~/.docker/config.json）
echo "<PAT>" | docker login ghcr.io -u linlea666 --password-stdin

# 4. 启动
mkdir -p data
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d

# 5. 验证
docker compose -f docker-compose.prod.yml ps
curl http://localhost:8080/api/health
```

然后在阿里云安全组和宝塔「安全」页放行 `3000` 和 `8080` 端口，浏览器访问 `http://服务器IP:3000`。

## 三、日常更新流程

本地：

```bash
git add -A && git commit -m "描述改动" && git push
```

等 GitHub Actions 构建完成（仓库 Actions 页可看进度，首次约 15~20 分钟，之后有缓存会快一些），然后在服务器上：

```bash
cd /opt/nofx
git pull
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d
```

三条命令即可完成更新，数据库和配置不受影响。也可以把这三条命令保存为宝塔「计划任务」里的 Shell 脚本，需要更新时手动执行一次。

## 四、常用运维命令

```bash
cd /opt/nofx
docker compose -f docker-compose.prod.yml logs -f nofx      # 看后端日志
docker compose -f docker-compose.prod.yml logs -f           # 看全部日志
docker compose -f docker-compose.prod.yml restart           # 重启
docker compose -f docker-compose.prod.yml down              # 停止（不删数据）
```

宝塔的 Docker →「容器」页面也能看到这两个容器的状态、日志和资源占用。

## 五、必须备份的东西

| 内容 | 位置 | 说明 |
|------|------|------|
| 数据库 | `/opt/nofx/data/` | SQLite 库，所有交易记录、配置都在里面 |
| 密钥 | `/opt/nofx/.env` | `DATA_ENCRYPTION_KEY` 丢失后，库里加密存储的交易所 API Key 将永远无法解密 |

建议用宝塔「计划任务」定期打包 `/opt/nofx/data` 和 `/opt/nofx/.env`。

## 六、绑定域名 + HTTPS（可选）

1. 宝塔新建站点绑定域名，设置「反向代理」到 `http://127.0.0.1:3000`
2. 宝塔 SSL 页申请 Let's Encrypt 证书
3. `.env` 中设置 `TRANSPORT_ENCRYPTION=true` 后重启容器
4. 之后可在安全组关闭 3000/8080 的公网访问，只走 443
