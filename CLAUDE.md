# joz：给 Claude 会话的工作说明

joz 是 NOFX（开源 AI 交易终端）的分支，在原版基础上重点开发了 **Discord 跟单**（`copytrader/`、`discord/`、`collector/`）、邮件通知、日志中心和恢复审查。实盘相关，出错会亏钱，改动一律保守。

- 后端：Go 1.25（`go.mod`），入口 `main.go`；前端：`web/`（Vite + React 18 + TypeScript + vitest）；采集器：`collector/`（Python，pytest）。
- 注意：根目录的 `agents.md` 是产品内“交易智能助手”的规范，不是开发说明，别当成本文件。
- 回复用户一律用简体中文。

## 工作方式：主会话规划审查，子代理写代码

### 主会话（用最高思维）
- 负责：读需求、定方案、拆任务、写子代理的规格、审查 diff、跑检查、提交推送。
- 不亲自大段写代码，也不重读整个文件；审查时只看 `git diff`，有疑问再定点读相关函数。
- 多文件改动先向用户说明方案再动手；涉及实盘行为的改动（下单、止损、仓位、对账），先给用户看结论再合入。

### 子代理（general-purpose，按任务定强度）
**effort high**：约束多、出错会直接影响资金或数据的部分
- 跟单核心：`copytrader/` 里的解析与规则（`parser.go`、`message_rules.go`、`presets.go`、`preset_policy.go`）、入场与仓位（`entry_*.go`、`risk.go`、`reduction_ratio.go`、`tp_*.go`）、止损与保护（`stop_*.go`、`protections.go`）、执行与对账（`executor.go`、`managed_execution.go`、`management_v4.go`、`reconcile.go`、`state_machine.go`、`recovery_*.go`、`replay*.go`）
- 交易执行：`trader/` 下各交易所适配和 `auto_trader_*.go`（订单、风控、网格、仓位重建、同步）
- 决策引擎与风险钳制：`kernel/`
- 数据与迁移：`store/`（表结构、迁移、账本、对账、持仓）
- Discord 采集与恢复：`discord/`、`collector/`（消息不丢、不重复处理）
- 安全：`crypto/`、`auth/`、`security/`、`wallet/`
- 回归样本：`testdata/` 里的真实频道样本和回归用例（只能加，不改已有期望，除非用户确认）

**effort medium**：页面和周边
- 前端 `web/src/`（页面、组件、i18n 文案、hooks）
- `api/` 里只做参数透传、展示的 handler（涉及下单或配置写入的按 high）
- 通知文案（`notify/`、`telegram/` 的消息模板）、测试样板、`docs/` 文档、部署脚本说明

### 给子代理的任务怎么写
- 写清：目标、要改哪些文件、要先读哪些文件和函数、验收标准（哪些测试要过、要加哪些测试）、不许动的地方。
- 少让它摸索：已经知道的结论直接写进规格。
- 几件小事合并给一个子代理，减少冷启动；互不相关的大任务才分开。
- 要求子代理改完自己跑相关包的测试，并在汇报里列出改动的文件和测试结果。

### 主会话审查后必须跑的检查
- 后端：`go build ./... && go vet ./... && go test ./...`（只改了某个包时，先跑 `go test ./copytrader/...` 这类定点测试，提交前再跑全量）。
- 前端（改了 `web/` 时）：`cd web && npm run lint && npm run test && npm run build`。
- 采集器（改了 `collector/` 时）：`cd collector && python -m pytest tests`。
- 测试失败如实告诉用户，不跳过、不删测试、不改期望值来凑通过。

## 开发原则
- 根因优先、全局视角、复用已有规则，一次到位；不扩大范围，但顺手发现的关键缺陷要修。
- 跟单规则改动要补回归用例（`testdata/` + 对应 `*_regression_test.go`），并说明对已有频道（Tyler、Jonzi、Neil、DSC 等）有没有影响。
- 不把服务器 IP、密码、API Key、Discord token、私钥写进仓库。

## 提交
- 沿用仓库现有风格：英文 Conventional Commits（`fix(copytrade): ...`、`feat(discord): ...`），说明写清改了什么、为什么。
- 末尾带会话提示里的 Co-Authored-By 等署名行；仓库内容里不写模型 ID。
