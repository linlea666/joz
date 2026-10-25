# jonzi 普通频道模式：配置与运维

适用于用 `jonzi_v1`（界面“作者风格预设”选 jonzi）跟随 JONZi 交易卡片频道的交易员。2026-10 起推荐使用**普通频道**来源模式；Chroma 通知模式仍可选，见[全新安装与验收](gateway-fresh-deployment.zh-CN.md#三位交易员配置)。

## 推荐配置

| 设置 | 推荐值 | 说明 |
|---|---|---|
| 来源模式 | 普通频道（`source_mode: "channel"`） | 采集新消息与原卡编辑 |
| 逻辑频道（Discord 频道 ID） | `1452344484103848026` | `primary_channel_id` |
| 实际监听频道 | 留空，或与逻辑频道一致 | 留空即监听逻辑频道。改动会改变执行键并撤销当前路由，必须在**空仓且无挂单**时操作 |
| 当前消息作者 ID | `1453389441585647798` | JONZi 卡片由 webhook 发布，按作者 ID 过滤 |
| 当前消息作者名称 | 留空 | webhook 显示名可能变化，名称过滤易误伤；两种过滤同时填写时须同时满足 |
| 作者风格预设 | jonzi（`jonzi_v1`） | |
| 单仓最大名义价值（USD） | 大于 0 | 必填且须大于 0，保存时校验；旧配置里存的 `0` 按默认上限 30000 读取 |
| 解析模型 | 支持图片输入的模型 | 卡片含图片，交给 AI 的消息需要读图 |

对应的配置片段（合并到现有跟单配置，不替换风险、账户等其他字段）：

```json
{
  "primary_channel_id": "1452344484103848026",
  "source_mode": "channel",
  "source_author_ids": ["1453389441585647798"],
  "interpretation_profile": "jonzi_v1"
}
```

`source_channel_ids` 在普通频道模式下可选；填写时最多 20 个、须为不重复的数字 ID。Chroma 模式下它和至少一种作者过滤都是必填。

## 消息处理规则

| 消息 | 处理 |
|---|---|
| rev0 新卡（原生新卡，Entry 带 ✔） | 确定性市价开仓：参考价取 Entry，仍受偏移阈值约束 |
| Entry 不带 ✔，或卡片字段解析冲突 | 交给 AI 解释 |
| 原卡编辑：Trailed stop | 确定性移动止损 |
| 结束卡：stop/loss hit、take profit hit、trailed stop hit、manually closed | 确定性平仓（要求唯一对应原交易） |
| 原卡的其他编辑 | 忽略 |
| 评论附卡（如 `+R booked`） | 忽略 |
| 其他消息 | 交给 AI，但**不能开仓** |

**jonzi 只有 rev0 原生新卡能开仓。** 编辑后的卡片、引用旧卡、收益播报和普通文字都不会产生新增风险。

### 远端止盈

距离当前价很远的止盈（例如 49k 的波段目标）超出 Binance 限价价格带时，交易所会拒单。系统把该档止盈标记为延后，每 5 分钟重试一次，直到价格进入价格带后挂上。延后只影响这一档止盈，**不影响止损**。

## 运维要点

- **持仓期间不要停止 JONZi 交易员。** 停止期间卡片编辑（移止损、结束卡）不会被采集；重新启动后建立新的激活边界，停止前发布的卡片不能再开仓。
- Binance 账户必须为**双向持仓**模式，否则跟单拒绝启动（切换前需先平仓并撤单）。
- 跟单只支持 Binance 和 OKX。
- 止损连续 3 分钟无法确认、且该方向看不到止损单时，会走应急平仓。
- 部署选在没有待成交开仓的时段；先用 `observe`（仅采集验证）观察新消息的解释结果，确认无误后再切 `live`。

## 排查

### 跳过原因码

| 原因码 | 含义 |
|---|---|
| `BEFORE_LIVE_BOUNDARY` | 消息在全局切换到 `live` 之前发布（或接收），不新增风险 |
| `BEFORE_TRADER_ACTIVATION` | 新卡在交易员本次启动（激活）之前发布；重启后的交易员不会开启动前的卡片 |
| `CONFIG_REVOKED` | 交易员的路由已撤销，或消息采集时的执行键与当前配置不一致（例如改了实际监听频道、作者过滤等执行相关配置） |

旧记录里可能还有 `BEFORE_EXECUTION_BOUNDARY`，是上面两种边界的旧合并码，新版本不再产生。

### 作者过滤

事件 `copytrade.source.author_filtered` 表示消息作者不在作者过滤中，消息被消费但不生成信号。事件上下文带有实际 `author_id`、`author_name` 以及配置的允许列表；频繁出现时先核对作者 ID 是否填错。

### 导出某频道的消息修订

以下命令只读打开主数据库（路径用实际数据目录替换），不会修改数据。建议先复制一份数据库再查。

```sh
# 每条消息的最新状态（编辑会更新同一行并递增 revision）
sqlite3 -readonly -header -csv '<数据目录>/data.db' \
  "SELECT message_id, revision, author_id, author_name, message_timestamp, edited_at,
          received_at, processing_status, processing_error, content, embeds_json
     FROM discord_messages
    WHERE channel_id = '1452344484103848026'
    ORDER BY message_timestamp DESC LIMIT 200;" > jonzi-messages.csv

# 不可变的逐修订记录（每次新增/编辑/删除一行）
sqlite3 -readonly -header -csv '<数据目录>/data.db' \
  "SELECT event_id, kind, message_id, revision, baseline, config_version,
          received_at, message_json
     FROM discord_inbounds
    WHERE channel_id = '1452344484103848026'
    ORDER BY id DESC LIMIT 500;" > jonzi-revisions.csv

# 某条消息的解释结果与跳过原因
sqlite3 -readonly -header -csv '<数据目录>/data.db' \
  "SELECT message_id, message_revision, classification, action, symbol, direction,
          status, skip_reason, error_message, created_at
     FROM copytrade_signals
    WHERE message_id = '<消息ID>'
    ORDER BY created_at;"
```

导出文件可能包含频道原文，分享前确认不含账户或个人信息。
