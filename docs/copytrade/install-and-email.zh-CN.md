# 一键安装、更新与邮件告警

支持首期验收环境：Ubuntu 24.04 LTS、amd64。默认目录 `/opt/nofx`，源码 `linlea666/joz` 的 `main`。最低检查为 2 GiB 内存、6 GiB 可用磁盘；建议至少 4 GiB 内存并留出 Docker 构建空间。三个服务顺序构建。低内存机器如构建被 OOM 终止，增加内存或 swap 后重试。

## 首次安装

在全新服务器执行：

```bash
curl -fsSL https://raw.githubusercontent.com/linlea666/joz/main/install.sh -o /tmp/nofx-install.sh
sudo bash /tmp/nofx-install.sh
```

需要 GitHub 和 Docker 官方源可达。私有仓库需要先配置服务器的 Git 访问权限，授权信息不要写入安装脚本。可传入绝对安装目录，例如 `sudo bash /tmp/nofx-install.sh /opt/nofx`。

脚本检查系统、架构、权限、资源及端口；缺少 Docker 时安装官方 Engine、Buildx、Compose 插件。有 Docker 时验证其可用性，不卸载或替换。已有非项目目录会停止；已有正确仓库可以重复运行，保留 `.env`、数据库和采集卷。

安装自动创建 `.env`（权限 600）、数据目录（700）并生成缺失的新安装密钥。没有 Discord 凭证时，采集器与后端通信仍应健康，Discord 状态显示等待配置。安装不要求填写 SMTP。默认前端 `http://服务器IP:3000`，后端端口 8080；只需让客户端可访问前端。访问端口也受云安全组、防火墙影响。

全新数据库默认**仅采集验证**；安装脚本不会启用实盘。完成 Discord 来源、作者过滤及解释验证后，由用户在界面明确启用正常执行。已有数据库的执行模式不会被安装、更新或重启重置。

## 更新与日常操作

原命令继续有效：

```bash
cd /opt/nofx && ./start.sh update
```

更新要求 main 分支、正确 origin、无已跟踪文件修改，且本地 HEAD 是 origin/main 的祖先。只允许快进，不重置、切分支或覆盖修改。部署锁防止并发运行。拉取后重新执行新脚本，先构建全部服务，再更新容器。构建失败保留旧容器；启动或健康验收失败返回非零状态，不自动回滚数据库。

```bash
./start.sh status
./start.sh logs backend
./start.sh logs frontend
./start.sh logs discord-collector
./start.sh restart
```

`start` 保留 `--build` 参数，始终按后端、前端、采集器顺序构建和健康启动。需要 Docker 权限；非 root 用户可加 `sudo`。固定 Compose 项目名 `nofx`、源码 Compose 文件，不使用旧 `docker-compose.prod.yml`。

保留的持久化资源：`.env`（含加密密钥）、`data/`（数据库和邮件配置）、`nofx_discord-buffer`（未确认事件）、`nofx_discord-runtime`（本地通信）。日常更新不得执行 `down -v`、`clean` 或 `regenerate-keys`。后两者仍是需要单独确认的破坏性命令。丢失原数据加密密钥不能靠生成新密钥恢复已有凭证。

## 前端邮件配置

打开 Discord 设置中的邮件告警区域，独立配置：

1. 选择 163 邮箱预设（`smtp.163.com`、465、TLS），或自定义 SMTP。
2. 填写发件邮箱／登录账号、SMTP 授权码。发件地址与登录账号共用同一字段。
3. 填一个收件邮箱；首次输入发件邮箱时可自动带入，之后可单独修改。
4. 选择是否启用自动告警，点击“保存邮件配置”。无需 Discord Token、采集重连或容器重启。
5. 可点击“发送测试邮件”：使用当前表单，**不会保存**。SMTP 接受只表示已交给邮件服务，请检查收件箱和垃圾邮件。

授权码加密入库，读取接口只返回“是否已设置”。空授权码表示保留；首次配置、换 SMTP 主机或换登录账号必须填写对应授权码。测试失败不覆盖保存的配置。自动告警关闭时不发送告警；用户主动测试仍可使用。

数据库无 SMTP 配置时，整组使用 `SMTP_HOST`、`SMTP_PORT`、`SMTP_USER`、`SMTP_PASS`，可选 `SMTP_SECURITY=tls|starttls`（未填写按 465/TLS，其他端口/STARTTLS）。页面显示“来自服务器环境”。保存后完整使用数据库配置，不混合字段；数据库、解密或发送错误不自动回退。数据库配置不会自动导入服务器环境变量。

SMTP 全程有 20 秒总超时，校验证书；STARTTLS 不支持时失败，不退回明文认证。页面请求遵循 `TRANSPORT_ENCRYPTION` 配置；启用时须使用 HTTPS 或 localhost 以支持浏览器 Web Crypto。生产访问建议使用 HTTPS。

## 故障定位

| 情况 | 操作 |
|---|---|
| 错误仓库、分支、本地修改 | 根据报错人工核对 Git 状态，再更新；脚本不会清理代码 |
| 端口占用 | 调整 `.env` 的 `NOFX_FRONTEND_PORT` / `NOFX_BACKEND_PORT`，或处理冲突服务 |
| 构建失败 | 检查磁盘、内存、网络、构建输出；旧容器未被脚本停止，可修正后再次 update |
| 健康检查失败 | `./start.sh status` 和对应服务 logs；失败不会被当作成功 |
| IPC 健康但 Discord 等待配置 | 首次安装正常；在前端配置凭证与来源，随后检查频道权限和消息缺口 |
| Docker 已安装但不可用 | 检查 `systemctl status docker`、当前用户权限、Compose v2 插件 |
| SMTP authentication 失败 | 核对发件账号及邮箱提供的 SMTP 授权码，不使用网页登录密码 |
| TLS/STARTTLS 失败 | 核对端口与连接方式、服务器证书和系统时间，不关闭证书校验 |
| SMTP recipient 失败 | 核对单个收件地址及邮件服务的投递限制 |
| 解密失败 | 恢复部署原有 `DATA_ENCRYPTION_KEY`；不要重新生成密钥覆盖旧配置 |

## 验证边界

本地使用临时数据库、模拟 SMTP 和模拟部署命令，不发送真实邮件、不下单。脚本测试不等于 Docker 实机验收；Ubuntu 24.04 的三服务构建、启动和 IPC 烟雾检查在 `.github/workflows/pr-docker-compose-healthcheck.yml` 中运行。没有 Docker 的开发环境必须将实机验收列为未完成，不能宣称通过。请在首次部署时保留各服务健康结果。

### 本轮本地验证记录

- Shell 语法检查和 14 项模拟安装／更新测试：覆盖 Ubuntu/Docker 分支、资源与端口、目录保护、并发锁、快进／重新执行、构建失败、健康失败和密钥保留。
- 模拟 SMTP 与配置 API／存储测试：TLS、STARTTLS、认证／证书／收件人拒绝、连接及发送超时、无 Token 配置、加密存储与加密表单、数据库事务回滚、失败不回退。
- 7 个相关 Go 包的竞态测试、Go 构建和相关 `go vet` 通过。完整 `go test ./...` 中 13 项既有 CoinAnk／Hyperliquid 外网测试因超时／DNS 失败；仅排除这 13 个确定的用例后，其余全量测试通过，未修改这些用例。
- 前端 203 项测试、构建及修改文件 ESLint 通过；采集器 15 项测试通过。
- 本机没有 Docker：尚未完成实际 Ubuntu 安装和容器烟雾验收。已补 Ubuntu 24.04 CI 作业，实际结果以作业及部署健康验收为准。没有发送真实邮件或触发真实下单。
