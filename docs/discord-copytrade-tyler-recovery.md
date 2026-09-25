# Discord 跟单来源、执行恢复与 TYLER 分批进场

本次实现基于 `8795381`，解释协议为 `copytrade-v5`。本文件记录实现边界、迁移与验收；[原有价格规则](discord-copytrade-market-entry.md)仍然适用，新增分批策略须由每个跟单实例显式开启。

## 根因与对应修复

此前只读核查确认，FIL 和 FLOCK 是两个不同 message ID。FIL 的参考价为 0.9385，决策市价 0.9863，不利偏移约 5.093%；原阈值 0.2% 导致全额挂参考价限价，随后超时。这不是解析漏单。FLOCK 开仓和当时的自动保本有实际成交与日志依据。

| 根因 | 实现 |
| --- | --- |
| 当前正文和历史引用卡被拼成同一段，引用开仓参数被当成新指令 | `SourceSegment` 区分当前正文、当前卡、引用与图片；每个动作必须提供当前来源证据，执行前校验。引用本身不能授权 OPEN/ADD。 |
| 减仓、移止损及多笔信号被压成单一结果，按 root 取第一笔可能选错 | 复用 `instructions`，子动作独立分类、校验、记录；按实例、频道、引用、币种和方向唯一定位，冲突/多候选跳过。 |
| “股票合约”被提示词直接拒绝，与交易所实际能力冲突 | 移除该提示词禁令。Binance 查询实际 USDT 永续合约状态及过滤规则，SNDK 有效合约可通过；不推测未知别名。 |
| 模型将有条件的提前止盈当成无条件减仓 | 保留 `requires_add_fill` 和未决条件；仅该交易第二腿有真实成交记录才满足补仓条件。证据不能只摘取一句话而丢掉外层条件。 |
| 仓位减少被当成 TP 成交；减仓撤掉 TP 后只恢复 SL | 读取关联 TP 订单真实累计成交，保存原始档位；减仓、进场追加成交、TP 成交后统一核对 SL 与未完成 TP。 |
| 慢模型持有执行锁，保护恢复被阻塞 | 消息顺序锁与执行锁分开；模型返回后重新读取状态，已停止引擎不会执行迟到响应。待成交、退出待确认和保护错误以 5 秒周期检查，无待处理状态时为 45 秒。 |
| 只有一次请求回执，没有可恢复订单身份 | 新增动作账本与订单账本。提交意图先入库，稳定 client ID 精确查询；丢回执先查询，不盲目重复市价单。 |
| 新旧 TP 存储混用比例与数量 | 版本 1 配方单独保存价格、比例、原始档位，订单计划只保存实际数量。旧待成交记录在第一次结算时转换。 |

只读核查中的模型 504、CYS 杠杆拒绝、SEI 原文异常 TP，分别属于模型服务、交易所配置和源信号价格问题，不统一归为识别失败。代码未更改杠杆、阈值、风险金额或原文价格。

## 复用与影响范围

- **扩展复用**：`SourceInterpretation.instructions`、路由、风险计算、TP 分配、配置/API、执行详情。TYLER 没有独立执行器。
- **提取复用**：`entry_settlement.go` 统一部分成交与撤单结算；`protections.go` 统一剩余 SL/TP 恢复。解决主动操作与定时对账不一致。
- **独立新增**：`copytrade_actions`、`copytrade_orders` 以及可选 `ManagedOrderTrader` 接口。稳定订单身份和恢复状态不耦合普通自主交易流程。
- **直接复用**：原市价参考价决策、区间中点、合约数量换算、按方向撤单、持仓消失二次确认和近期误关闭恢复。

来源、目标关联和保护修复作用于 Discord 跟单公共链路。TYLER 话术规则只作用于 `interpretation_profile=tyler_v1` 的实例。分批仅作用于明确选择 `entry_policy=market_reference_split` 的新计划。普通 agent、策略系统及交易所旧平仓方法未改变；新受管接口不会隐式取消其他订单。

## 消息与指令

`interpretation_profile` 接受 `default`、`tyler_v1`；旧配置缺字段等同通用规则。不会按名称含 TYLER 自动启用。

来源保留 ID、正文/卡片、作者、时间、引用消息与图片角色。旧 embed 时间必须有回复、当前管理/播报语义等其他依据才判为引用；也可通过已存原消息正文精确匹配。Neil 原生卡不能仅因时间较早变成引用，JONZi 原地编辑仍是当前消息。无法确定引用目标时记录跳过，不靠价格校验碰巧拦截。

每个可执行子动作携带 `action_evidence={source_id,text}`。文字证据必须是对应当前段的原文；图片证据须来自本次真正取得的当前图片。历史图片不产生操作授权。OPEN/ADD 必须有当前入场/加仓用语，不能只复述旧参数。

TYLER 固定行为：

| 当前消息 | 行为 |
| --- | --- |
| 收益展示、“起飞”、单独“TP1 已到” | IGNORE |
| “止盈或者减仓”等已识别口令 | 减剩余仓位 50%；明确比例优先 |
| 明确提前平仓、全平 | 全平 |
| 成本损、保本 | 撤销后续入场资格，结算撤单竞态后取实际均价；不降低已有更有利止损 |
| 止损提升到 TP1 | `TP_LEVEL(level=1)`，执行层读原始 TP1 价格 |
| “TP2 后成本损” | 保存条件，只在关联 TP2 有真实成交后移动止损 |
| “有补仓的才提前止盈” | 必须有该交易的补仓成交证据；仅有挂单或模型 warning 不成立 |
| “仓位重”等无法客观判断的条件 | 对应动作 NEEDS_CONTEXT |

通用模式保留模型解释不同作者话术的能力，但共用来源与唯一关联校验。显式 ADD 仍保留原 V1 的“有仓不追加”限制；本次补仓证据来自受管分批计划的第二腿，不把未知手动加仓归到某笔交易。

## 分批计划与风险

新策略首版仅支持 **Binance + 以损定量 (`by_loss`)**，API 和界面均检查。默认 `legacy` 保持原有进场决策。

仅当当前入场是“市价意图 + 明确参考价”时：

| 市价相对参考价 | 行为 |
| --- | --- |
| 相等或更有利 | 全额市价 |
| 不利偏移 > 0 且 ≤ 实例配置阈值 | 50% 风险金额市价 + 50% 风险金额参考价限价 |
| 不利偏移超过阈值，或阈值为 0 | 全额参考价限价 |

纯市价、固定限价、区间单继续原规则。阈值没有改成 5%；即使手动设为 5%，样本 FIL 的约 5.093% 仍然全额限价。

两腿各按 `分配风险 / abs(该腿进场价 - 止损价)` 算数量，共同受名义额上限、可用保证金 90% 及配置杠杆约束。数量按真实 LOT_SIZE / MARKET_LOT_SIZE 向下取整，两腿最小量和最小名义额均在第一笔提交前检查。不同进场价下，币数量不必各占一半。

一个交易上下文管理两腿、占一个持仓名额。分批要求同一账户、币种、方向无其他受管上下文或已有交易所仓位，避免把别的仓位算进该计划。第一腿确认成交且 SL 成功后才提交第二腿；持续部分成交也立即受保护。实际第一腿成交占用风险后，第二腿只使用剩余预算，不因失败改成另一笔市价单。

实际成交超预算时先永久关闭补仓资格并撤未成交部分，再核对撤单竞态和实际风险，减去超额仓位；这项保护性减仓按市价最小量和步长向上取整，不把开仓数量向上扩大。确认不到实际成交均价时保持保护并报错，不用零价/参考价编造已用风险。

TP 成交、减仓、全平、撤单、保本或计划超时都会把 `entry_disabled` 永久置为 true。撤单后再次核对累计成交，已有成交保持 OPEN 并恢复保护；不能把有仓交易直接标为 CANCELLED。默认超时仍为 240 分钟，自计划创建起算，沿用实例现有超时设置。

风险金额仍是按止损距离估算的亏损预算，不包含手续费、滑点、跳空的净亏损保证。

## 订单恢复和保护

- 新 Binance 跟单使用可选受管接口；每笔 ENTRY、TP、CLOSE 有稳定 client ID 和精确交易所订单 ID。旧普通适配器调用行为保留。
- `PLANNED` 尚未提交；`SUBMITTING`/`UNKNOWN` 必须查订单。交易所明确拒绝与回执不确定分开记录。持续查不到或接口不可用时显示明确错误，保持已知仓位保护；不盲目重发。
- 动作身份包含实例、原消息、目标、动作及必要语义。消息重复投递、重启或文字修饰不会使已完成减仓再次减半。旧成功信号也参与防重放，包括多指令记录。
- 减仓金额在提交前冻结。全平回执已成交但仓位仍可见时保持待确认、继续保护；不把它误记成新的部分减仓，不重复发单。
- TP 原始序号不因排序、截取最近三档或重挂而变化。只用对应 TP 订单累计成交证明档位，手动减仓不增加命中数。完成的 TP 不重挂，部分成交跨重挂保留累计证据。
- SL 校验币种、方向和覆盖量；不能用空头 SL 证明多头有保护。保护更新失败保留错误并重试，移动失败尝试恢复原 SL。
- 旧匿名 TP 没有足够订单身份时，不猜成交档位、不盲目重挂；日志要求人工核对。非受管交易所的模糊下单回执同样不盲目重发。

Binance 请求使用官方 USDⓈ-M Futures [下单/查询/撤单接口](https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/New-Order)，适配器回归通过本地 HTTP 模拟验证稳定 client ID、仓位方向及真实步长，没有调用真实交易账户。

## 数据迁移与兼容

启动时复用现有 GORM 迁移，增加两个表和可选字段，不删除已有表或修改原配置 JSON：

| 数据 | 新内容 |
| --- | --- |
| `copytrade_actions` | 语义动作 ID、消息/交易关联、执行状态、参数与错误 |
| `copytrade_orders` | 稳定 client ID、订单 ID、角色、计划量、累计成交量、均价、状态 |
| 交易上下文 | 执行版本、入场计划/截止时间、补仓关闭标记、实际追加成交、退出意图、版本化 TP 配方 |
| 信号 | 执行版本、逐动作结果、有限延后重试时间与次数 |

旧交易 `execution_version=0`，不自动加第二腿，不重挂历史开仓，不改既有保本结果。旧 Binance 持仓接收到新的管理动作时，可为该次退出建立受管账本，重启时按退出意图恢复，不改变旧入场版本。

版本 1 的 `tp_recipe_json` 保存计划比例及原始价格；`tp_plan_json` 保存订单实际数量、已成交、原始档位和重挂代次。旧待成交记录的比例只在首次成交结算时迁入配方，之后不再混用数量字段。

Discord 元数据补全、作者/时间补全、签名图片 URL 更新、哈希算法差异只刷新元数据，不增加 revision、不重新投递历史消息。真正正文/卡片参数/图片身份变化仍按编辑处理。新旧信号的历史失败记录不自动补执行。

API 原字段与 `executed` 处理状态保留，新增 `instruction_results`、`action_results`、`order_legs`。界面区分消息已处理与各交易待成交/已开仓/已撤单，并显示累计成交、均价与失败原因。

模型临时故障在原重试耗尽后，只有尚无动作记录的新任务可延后重试两次，间隔 30/120 秒。超过信号 TTL 或被新 revision 替代即跳过；执行前仍按 OPEN/管理动作分别检查 TTL。

## 验收与测试

新增脱敏样本：`testdata/copytrading/tyler_regressions.json`。`source=screenshot_transcription` 是截图文字转录，`sanitized_audit_text` 是已核查文本；账号、频道和原 message ID 未写入新样本。`response` 是离线协议测试输入，其中引用播报故意输入旧 OPEN 误判以验证规则拦截，并非本次在线模型回执。样本行情作为测试输入，不能据此证明任何历史成交。

验收覆盖：

- FIL 与 FLOCK 独立消息、FIL 0.2%/5% 均挂参考价限价、SNDK 按实际合约能力、收益播报、引用减仓。
- 同消息双开仓、减仓并移动 SL、冲突引用、条件语句完整性、补仓资格、原始 TP1 引用。
- TP1 真成交/TP2 条件、手动减仓、SL 失败恢复、多空隔离、剩余 TP 重建。
- 阈值边界、风险金额分摊、最小量、滑点后剩余预算、超额风险、两腿部分成交、撤单竞态、单腿失败、丢回执与重启。
- 老单新退出恢复、TP 比例/数量分离、数据库版本冲突阻止外部下单、元数据更新不重放。
- Neil 原生 embed、JONZi 编辑消息、DSC 样本及此前市价参考价样本。
- 回放对订单、交易、信号、动作、AI 记录、事件和历史消息完全不写入；模型等待不持执行锁，引擎停止后不执行迟到结果。
- API 数据隔离、逐动作/订单腿字段；前端显式开启配置、非法组合禁用保存、保留已有风险设置及执行状态展示。

关键验证命令：

```sh
go test -race ./copytrader ./discord ./store ./api ./trader/binance ./trader/okx ./trader/types
go build ./...
cd web
npm test
npm run build
```

本次使用离线模型协议样本、本地模拟交易所和临时数据库验收；没有发送真实订单，也没有将回放结果写成生产成交证明。前端构建现有的 Browserslist 数据过期和大 chunk 提示不在本次改动范围。

## 手动上线与配置

1. 按现有部署流程备份数据库、拉取本次提交、编译并重启。启动会自动完成新增表/字段迁移；保留旧配置。
2. 编辑目标 TYLER 跟单实例，将“消息解读规则”设为 **TYLER**。这是话术策略，其他实例保持通用。
3. 如要启用分批，确认该实例为 **Binance + 以损定量**，再将“进场策略”设为“50% 市价 + 50% 参考价限价（风险金额）”。解读规则和进场策略可分别启用。
4. 风险金额、BTC/ETH 阈值、山寨币阈值、限价超时、杠杆和 TP1 后保本均继续使用原保存值。若采用 5%，由维护者自行在实例编辑页设置；本次未代改。
5. 在新信号的“查看 AI 与执行详情”核对实际阈值、逐动作结果、两腿累计成交与关联交易状态。尚未启用分批的实例不产生第二腿。

关闭分批选项只影响新计划，已有计划继续按建立时的持久化规则管理。已有受管仓位/订单尚未结算时，不直接切回不认识账本的旧二进制；应先核实未成交腿、剩余仓位与保护状态，再安排回退。

实施阶段未修改生产配置、订单或部署。CYS 杠杆、未知别名、SEI 异常原文价格仍需分别按实际情况处理，没有擅自改成可成交值。

## 本次文件清单

以下为本次修改或新增的文件（包含实现、回归、样本和本文档）：

api：

- [api/handler_copytrade_test.go](../api/handler_copytrade_test.go)
- [api/handler_trader.go](../api/handler_trader.go)

copytrader：

- [copytrader/config.go](../copytrader/config.go)
- [copytrader/engine.go](../copytrader/engine.go)
- [copytrader/entry_decision_test.go](../copytrader/entry_decision_test.go)
- [copytrader/executor.go](../copytrader/executor.go)
- [copytrader/executor_recovery_test.go](../copytrader/executor_recovery_test.go)
- [copytrader/parser.go](../copytrader/parser.go)
- [copytrader/parser_test.go](../copytrader/parser_test.go)
- [copytrader/prompt.go](../copytrader/prompt.go)
- [copytrader/reconcile.go](../copytrader/reconcile.go)
- [copytrader/reconcile_close_test.go](../copytrader/reconcile_close_test.go)
- [copytrader/reconcile_selfheal_test.go](../copytrader/reconcile_selfheal_test.go)
- [copytrader/replay.go](../copytrader/replay.go)
- [copytrader/types.go](../copytrader/types.go)
- [copytrader/validate.go](../copytrader/validate.go)
- [copytrader/entry_settlement.go](../copytrader/entry_settlement.go)
- [copytrader/managed_execution.go](../copytrader/managed_execution.go)
- [copytrader/managed_execution_test.go](../copytrader/managed_execution_test.go)
- [copytrader/managed_recovery_edges_test.go](../copytrader/managed_recovery_edges_test.go)
- [copytrader/pipeline.go](../copytrader/pipeline.go)
- [copytrader/pipeline_recovery_test.go](../copytrader/pipeline_recovery_test.go)
- [copytrader/protections.go](../copytrader/protections.go)
- [copytrader/source.go](../copytrader/source.go)
- [copytrader/source_policy_test.go](../copytrader/source_policy_test.go)
- [copytrader/tyler_regressions_test.go](../copytrader/tyler_regressions_test.go)

discord：

- [discord/normalizer.go](../discord/normalizer.go)
- [discord/types.go](../discord/types.go)

store：

- [store/copytrade.go](../store/copytrade.go)
- [store/discord_copytrade_test.go](../store/discord_copytrade_test.go)
- [store/discord_message.go](../store/discord_message.go)
- [store/copytrade_ledger.go](../store/copytrade_ledger.go)

trader：

- [trader/auto_trader_copytrading.go](../trader/auto_trader_copytrading.go)
- [trader/binance/futures_orders.go](../trader/binance/futures_orders.go)
- [trader/types/interface.go](../trader/types/interface.go)
- [trader/binance/managed_orders.go](../trader/binance/managed_orders.go)
- [trader/binance/managed_orders_test.go](../trader/binance/managed_orders_test.go)
- [trader/types/managed.go](../trader/types/managed.go)

web：

- [web/src/components/trader/CopyTradeExecutionDetails.tsx](../web/src/components/trader/CopyTradeExecutionDetails.tsx)
- [web/src/components/trader/CopyTradeLogModal.test.tsx](../web/src/components/trader/CopyTradeLogModal.test.tsx)
- [web/src/components/trader/CopyTradeLogModal.tsx](../web/src/components/trader/CopyTradeLogModal.tsx)
- [web/src/components/trader/TraderConfigModal.tsx](../web/src/components/trader/TraderConfigModal.tsx)
- [web/src/i18n/translations.ts](../web/src/i18n/translations.ts)
- [web/src/types/discord.ts](../web/src/types/discord.ts)
- [web/src/components/trader/TraderConfigModal.test.tsx](../web/src/components/trader/TraderConfigModal.test.tsx)

docs：

- [docs/discord-copytrade-tyler-recovery.md](../docs/discord-copytrade-tyler-recovery.md)

testdata：

- [testdata/copytrading/tyler_regressions.json](../testdata/copytrading/tyler_regressions.json)
