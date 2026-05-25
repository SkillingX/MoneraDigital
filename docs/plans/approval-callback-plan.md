# Approval Callback Service — 实施计划

> **Spec**: `docs/spec/approval-callback-spec.md`
> **分支**: dev
> **日期**: 2026-05-21

---

## 概述

4 个任务，按依赖顺序串行执行。每个任务结束有检查点。

**施工原则**：
- TDD — 每个模块先写测试
- 所有新增代码写在 spec 指定的文件路径，不要散落到其他地方
- 参考现有 webhook handler（`safeheron_webhook_handler.go`）的代码风格和错误处理模式
- SDK 已有的能力直接用，不重新实现（参见 spec §3.1）
- **测试覆盖率 ≥ 90%（目标 100%）** — 钱包业务零容忍，所有正常路径、异常路径、边界条件都要有测试

---

## Task 1：基础设施（Migration + SDK 封装 + 数据模型）

**目标**：建好地基 — DB 表可用、SDK 类型定义完整、Repository CRUD 可测。

**产出文件**：
- `internal/migration/migrations/023_create_approval_and_sweep_tables.go` — 两张表的 DDL
- `internal/safeheron/cosigner.go` — 业务类型定义 + CosignerClient 封装
- `internal/approval/models.go` — ApprovalRecord / SweepTransaction / ApprovalConfig
- `internal/approval/repository.go` — DB CRUD

**Migration**：
- `approval_records` + `sweep_transactions` 两张表，DDL 照搬 spec §5.1 和 §5.2
- 注意 `chain_symbol DEFAULT 'UNKNOWN'`
- 注册到 `cmd/migrate/main.go`，遵循 015 的 step struct 模式

**SDK 封装**（`cosigner.go`）：
- 业务类型按 spec §3.2 照搬（所有字段和 json tag）
- `CosignerClient` 包装 SDK `cosigner.CoSignerConverter`：`NewCosignerClient` / `ParseRequest` / `BuildResponse`
- `BuildResponse` 内部构造 `cosigner.CoSignerResponseV3{ApprovalId: approvalId, Action: action}` 传给 SDK `ResponseV3Converter`。SDK 会自动填充 `code`/`message`/`timestamp`/`version` 并签名，无需手动处理（已确认 SDK 源码 `cosigner_converter.go:124-143`）
- SDK 的 `CoSignerConfig` 字段名有误导（名叫 PubKey 但实际传文件路径），用我们的 `CosignerConfig` 清晰命名包装，复用 `validateKeyFile()` 校验权限
- 不实现 SafeheronClient 接口，两者独立

**Repository**：
- `InsertApprovalRecord` — INSERT，遇 approval_id UNIQUE 冲突返回特定 error（幂等用）
- `GetApprovalByID` — 按 approval_id 查询（幂等回查用）
- `InsertSweepTransaction` — 仅 APPROVE 时调用
- `UpdateSweepStatus(ctx, txKey, status, subStatus, txHash string, completedAt *time.Time) error` — webhook 更新用
- `raw_request` 字段存解码后的 bizContent JSON（`json.RawMessage`）

**检查点**：
- [ ] `go run ./cmd/migrate/` 成功建表
- [ ] `go test ./internal/safeheron/... -run Cosigner -v` 通过
- [ ] `go test ./internal/approval/... -v` 通过（sqlmock）
- [ ] `go vet ./...` 无报错

---

## Task 2：审批逻辑（Approver 接口 + 分发 + Service）

**目标**：核心审批链路可运行 — 给定 bizContent 输入，输出 APPROVE/REJECT 决定，写 DB，触发告警。

**产出文件**：
- `internal/approval/approver.go` — Approver 接口 + DefaultRejectApprover + CallbackTestApprover
- `internal/approval/transaction_approver.go` — TRANSACTION 类型按 tx_type 分发校验
- `internal/approval/service.go` — ApprovalService 入口

**Approver 接口**：
- `Evaluate(ctx, *CoSignerBizContentV3) (*ApprovalDecision, error)`
- `DefaultRejectApprover` 用于 MPC_SIGN / WEB3_SIGN / 未知 type
- `CallbackTestApprover` 固定 APPROVE

**TransactionApprover** — 按 `TransactionType`（Safeheron JSON 字段名）分发：
- `COSIGNER_ALLOWED_TX_TYPES` 白名单前置校验，不在白名单直接 REJECT
- AUTO_SWEEP：`destinationAccountType == "VAULT_ACCOUNT"` + `destinationAccountKey` 在白名单
- AUTO_FUEL / UTXO_COLLECTION：`destinationAccountType == "VAULT_ACCOUNT"`
- NORMAL：默认 REJECT（预留提币扩展点）
- `COSIGNER_SWEEP_TARGET_ACCOUNTS` 为空 → 一律 REJECT
- 金额校验本期不实现，代码预留 TODO 注释
- `chain_symbol` 推导：调用 `WalletRegistry.GetCoinChainBySafeheronKey(coinKey)` 获取 `*CoinChain`，再取 `CoinChain.Chain.ShortName`（值如 `ETH`/`BSC`/`TRX`/`BTC`）。查不到记 `"UNKNOWN"` + warning。**注意**：方法名不是 `CoinChainByCoinKey`，入参是 Safeheron 的 coinKey（如 `USDT_ERC20`）

**ApprovalService 构造依赖**：
- `Repository` — DB 操作
- `AlertFunc func(level, title string, fields map[string]string)` — 告警回调，与 PoolManager 用同一签名模式，Container 中从 `AlertService.Send` 注入
- `WalletRegistry` — chain_symbol 推导（内存缓存）
- `ApprovalConfig` — 白名单配置，字段：`SweepTargetAccounts []string` + `AllowedTxTypes []string`（从 env 解析）

**ApprovalService.Evaluate()**：
1. 按 type 选择 Approver → 得到 decision
2. chain_symbol 推导（`GetCoinChainBySafeheronKey` → `Chain.ShortName`）
3. 组装 ApprovalRecord（含 raw_request = 解码后的 bizContent JSON），INSERT DB
4. **幂等**：UNIQUE 冲突 → 查回首次 action 返回（不重复告警）
5. APPROVE + TRANSACTION → INSERT sweep_transactions（REJECT 不写）
6. REJECT → 调用 AlertFunc（飞书告警含 approvalId/txKey/type/reason/coinKey/txAmount/地址）

**检查点**：
- [ ] `go test ./internal/approval/... -v -race` 全部通过
- [ ] 覆盖率 ≥ 90%
- [ ] 每个 tx_type 的 APPROVE/REJECT 路径都有测试
- [ ] 异常路径覆盖：白名单为空、UNIQUE 冲突幂等、未知 type、detail 反序列化失败、Registry 查不到 coinKey

---

## Task 3：HTTP 层 + 组装（Handler + Container + 路由）

**目标**：端到端可通过 HTTP 调用。

**产出文件**：
- `internal/handlers/cosigner_callback_handler.go` — Gin handler

**修改文件**：
- `internal/container/container.go` — 新增 `WithCosignerCallback()` option
- `internal/routes/routes.go` — 注册 `POST /api/cosigner/callback`

**Handler**（参考 `safeheron_webhook_handler.go` 的代码风格）：
- `CosignerCallbackHandler` 持有：CosignerClient、ApprovalService、AllowedIPs、**AlertFunc**
- 5s context timeout
- IP 白名单（同 webhook handler 模式）
- MaxBytesReader 1MB
- JSON 解码 → `CosignerClient.ParseRequest()` → `ApprovalService.Evaluate()` → `CosignerClient.BuildResponse()` → 200 返回签名 map
- 验签失败 → 401 + **Handler 直接调用 AlertFunc 告警**（此时还没进入 ApprovalService）
- 内部错误 → 500（Co-Signer 重试命中幂等）

**Container**：
- 读 `COSIGNER_PUBLIC_KEY_PATH` / `COSIGNER_CALLBACK_PRIVATE_KEY_PATH`，都为空则跳过 + warning
- 构造 CosignerClient → ApprovalConfig（从 env） → ApprovalService → Handler
- IP 白名单读 `COSIGNER_ALLOWED_IPS`（独立于 `SAFEHERON_WEBHOOK_ALLOWED_IPS`，不做 fallback），为空时 warning

**路由**：
- public 路由组内，紧邻 webhooks，`if cont.CosignerCallbackHandler != nil` 守卫

**检查点**：
- [ ] `go build ./cmd/server/` 编译通过
- [ ] `go test ./internal/handlers/... -run Cosigner -v -race` 通过（覆盖 200/400/401/403/413/500/503）
- [ ] `go test ./... -race` 全量测试不引入回归
- [ ] `go vet ./...` 无报错

---

## Task 4：Webhook 扩展 + 收尾

**目标**：归集交易状态变更能通过 webhook 更新 sweep_transactions，完成闭环。

**修改文件**：
- `internal/handlers/safeheron_webhook_handler.go` — 增加出向交易分支（**确定在 handler sync 路径中处理**，不是 deposit worker）
- `.env.example` — 新增 env 变量

**Webhook 出向分支**：
- `transactionDirection == "SEND"` 时，按 `txKey` 查 sweep_transactions
- 找到 → UPDATE tx_status / tx_sub_status / tx_hash / updated_at / completed_at
- 找不到 → 忽略 + warning（由定时轮询兜底）
- 注释标注：`// 竞态待验证 — spec §5.2`
- **不改动现有充值（RECEIVE）方向的逻辑**

**收尾**：
- `.env.example` 加上 spec §8.1 的所有新增 env
- 全量 `go test ./... -race -cover`，覆盖率 ≥ 90%
- 确认文件路径与 spec §2.2 一致、env 变量名与 spec §8.1 一致

**检查点（最终）**：
- [ ] `go test ./... -race` 全部通过
- [ ] 覆盖率 ≥ 90%
- [ ] `go vet ./...` 无报错
- [ ] `.env.example` 包含新增 env
- [ ] 代码审查通过

---

## 依赖图

```
Task 1 (基础设施) → Task 2 (审批逻辑) → Task 3 (HTTP+组装) → Task 4 (Webhook+收尾)
```

严格串行，每个任务的产出是下一个任务的输入。

---

## 验收标准

以下条件全部满足时，本次开发视为完成：

### 功能验收

1. **回调接收与验签**：发送一个合法的 V3 协议请求到 `POST /api/cosigner/callback`，返回 200 + 包含 `sig`/`bizContent`/`version` 的签名响应
2. **APPROVE 路径**：发送 `type=TRANSACTION` + `transactionType=AUTO_SWEEP` + 目标账户在白名单 → 返回 action=APPROVE，`approval_records` 和 `sweep_transactions` 各写入一条记录
3. **REJECT 路径**：发送 `type=TRANSACTION` + 目标账户不在白名单 → 返回 action=REJECT，`approval_records` 写入一条记录（`sweep_transactions` 无记录），飞书收到告警
4. **类型覆盖**：AUTO_SWEEP / AUTO_FUEL / UTXO_COLLECTION 按 spec §4.3 的规则校验；NORMAL / MPC_SIGN / WEB3_SIGN / 未知 type 一律 REJECT
5. **CALLBACK_TEST**：返回 APPROVE，写入 `approval_records`
6. **幂等**：同一 `approvalId` 重复请求，返回首次决定，DB 不产生重复记录，不重复告警
7. **Webhook 联动**：收到归集交易的出向 webhook（`transactionDirection=SEND`），`sweep_transactions` 的 `tx_status` / `tx_hash` 正确更新
8. **Webhook 未知 txKey**：SEND 方向 webhook 的 `txKey` 不在 `sweep_transactions` 中时，忽略 + warning 日志，正常返回 ack
9. **降级**：未配置 cosigner 密钥路径时，服务正常启动（callback 端点不注册），不影响其他功能

### 安全验收

10. **验签失败**：伪造的请求签名 → 401，不写 DB
11. **IP 白名单**：配置 `COSIGNER_ALLOWED_IPS` 后，非白名单 IP → 403
12. **Body 限制**：超过 1MB 的请求体 → 413
13. **密钥权限**：启动时校验 PEM 文件权限，权限过宽时 log warning

### 质量验收

14. **测试覆盖率** ≥ 90%，目标 100%（`go test ./internal/approval/... ./internal/safeheron/... ./internal/handlers/... -cover`）—— 钱包业务零容忍，所有正常路径、异常路径、边界条件都必须覆盖
15. **全量测试通过**：`go test ./... -race` 无失败（排除已知 baseline 失败）
16. **静态分析**：`go vet ./...` 无报错
17. **编译通过**：`go build ./cmd/server/` 成功
18. **环境变量文档化**：`.env.example` 包含 spec §8.1 列出的所有新增变量

### 代码质量

19. **文件路径**：所有新增文件与 spec §2.2 模块划分一致
20. **命名一致**：DB 字段用 `tx_type` / `tx_status`，Go struct JSON tag 用 Safeheron 原始字段名（`transactionType`），env 变量用 `COSIGNER_` 前缀
21. **待验证标记**：金额校验扩展点和 webhook 竞态处理有 TODO 注释 + spec 章节引用
22. **异常路径测试全覆盖**：DB 错误、JSON 反序列化失败、context 超时取消、白名单为空、Registry coinKey 查不到、approval_id 冲突幂等 — 每个都要有测试用例

---

## 风险与缓解

| 风险 | 概率 | 缓解 |
|------|------|------|
| Webhook 与审批回调时序不确定 | 中 | 代码预留注释，测试环境验证后定稿（spec §5.2） |
| 金额校验逻辑待定 | 低 | 预留扩展点 + TODO 注释，不阻塞本期交付 |
| SDK CoSignerConfig 字段名误导 | 低 | spec §3.3 已注释说明，CosignerConfig 用清晰命名包装 |
| 生产环境密钥未就绪 | 低 | Container option 降级处理（密钥缺失则跳过） |

---

# v1.1 Phase 1 实施计划（AML 硬封堵）

> **关联 spec**: `docs/spec/approval-callback-spec.md` §13（D-AML-1..12 决策已闭环）
> **日期**: 2026-05-25
> **范围**: 仅 Phase 1（仅 `TRIGGERED + LOW` APPROVE，其余全 REJECT）
> **不在范围**: Phase 2（override 表 / admin API / 三元决策 / 告警分级），等主流程跑通 + §13.10 实测后启动

2 个任务，串行执行，每个对应一个独立 commit。预计 1 天。

```
Task 1 (基础设施) ──► Task 2 (AML 接入 + 验收)
```

---

## Task 1：基础设施 — SummarizeRiskLevel 上提 + Migration 024

> **决策**: D-AML-10, D-AML-11
> **目标**: 解耦 approval/deposit 模块共享 AML 等级聚合逻辑；在 approval_records 表加 aml_risk_level 列

### 1.1 SummarizeRiskLevel 迁移到 safeheron 包

**硬约束**: 零行为变更 — deposit 包内所有测试用例的**期望字符串值不变**，仅允许 import / 符号名替换。

**迁移清单**（字符串值完全保持）:

| 旧位置 | 新位置 |
|--------|--------|
| `deposit.SummarizeRiskLevel(amlList)` | `safeheron.SummarizeAmlRiskLevel(amlList []AmlReport) string` |
| `deposit.riskSeverity` 私有 | `safeheron.amlRiskSeverity` 私有 |
| `deposit.KytLow`/`KytMedium`/`KytHigh`/`KytSevere`/`KytUnknown`/`KytFailed`/`KytSkipped`/`KytPending`/`KytEmpty` | `safeheron.AmlRiskLow` / ... / `safeheron.AmlRiskEmpty`（值不变，全部 `"LOW"`/`"MEDIUM"`/...） |

**保留在 deposit 包**（不迁）: `KytActionCredit/KeepPending/ManualReview` 常量、`KytDecision` 类型、`DecideKYT` 函数、`ReasonKyt*` 常量、`AlertLevelForKyt` 函数。

**改动文件**:
- 新增：`internal/safeheron/aml.go`（函数 + 常量 + 私有 helper）
- 新增：`internal/safeheron/aml_test.go`（把 `deposit/kyt_test.go::TestSummarizeRiskLevel` 整体搬来，改符号名）
- 修改：`internal/wallet/deposit/kyt.go`（删迁出符号；DecideKYT 内部调用改 `safeheron.SummarizeAmlRiskLevel` + `safeheron.AmlRisk*`）
- 修改：`internal/wallet/deposit/kyt_test.go`（`TestDecideKYT` 用例期望值符号名改 `safeheron.AmlRiskLow` 等）

### 1.2 Migration 024 — approval_records 加 aml_risk_level 列

**新增**: `internal/migration/migrations/024_add_aml_risk_level_to_approval_records.go`（参照 023 的 step struct 模式）

**DDL**:
```sql
-- Up
ALTER TABLE approval_records ADD COLUMN IF NOT EXISTS aml_risk_level VARCHAR(16);
CREATE INDEX IF NOT EXISTS idx_approval_records_aml_risk ON approval_records(aml_risk_level);

-- Down
DROP INDEX IF EXISTS idx_approval_records_aml_risk;
ALTER TABLE approval_records DROP COLUMN IF EXISTS aml_risk_level;
```

**修改**: `cmd/migrate/main.go` 注册 024；如有 migration step 测试，追加 024 条目。

历史数据不回填，新列默认 NULL。

### 1.3 验收

```bash
go build ./... && go vet ./...
go test ./internal/wallet/deposit/... ./internal/safeheron/... ./internal/migration/... -race -count=1
grep -rn "deposit\.\(SummarizeRiskLevel\|Kyt\(Low\|Medium\|High\|Severe\|Unknown\|Failed\|Skipped\|Pending\|Empty\)\)" internal/ \
    || echo "OK: no remaining external references"
```

**Checkpoint**:
- [ ] `safeheron/aml.go` 行覆盖 = 100%
- [ ] deposit 包测试全绿，且 `git diff internal/wallet/deposit/` 仅符号名/import 改动，无期望字符串值变更
- [ ] grep 输出 "OK: no remaining external references"
- [ ] migration 二次执行幂等

---

## Task 2：AML 硬封堵接入 + 验收

> **决策**: D-AML-1, D-AML-3, D-AML-5, D-AML-7, D-AML-11
> **目标**: 把 AML 校验串入 AUTO_SWEEP / UTXO_COLLECTION 审批，结果落库 approval_records.aml_risk_level，REJECT 触发飞书告警

### 2.1 DecideSweepAML（TDD）

**新增**: `internal/approval/sweep_aml.go` + `sweep_aml_test.go`

```go
type SweepAMLDecision struct {
    Approve   bool
    RiskLevel string  // 写入 approval_records.aml_risk_level
    Reason    string  // 写入 approval_records.reason
}

func DecideSweepAML(state string, amlListRaw json.RawMessage) SweepAMLDecision
```

**算法**:
- `state != "TRIGGERED"` → REJECT，`RiskLevel = "STATE_<state或MISSING>"`，`Reason = "SWEEP_AML_STATE_<...>"`
- `amlListRaw` 为空 / `"null"` → REJECT，`RiskLevel = "EMPTY"`，`Reason = "SWEEP_AML_RISK_EMPTY"`
- `amlListRaw` 反序列化失败 → REJECT，`RiskLevel = "PARSE_FAILED"`，`Reason = "SWEEP_AML_PARSE_FAILED"`
- `safeheron.SummarizeAmlRiskLevel(...) == AmlRiskLow` → APPROVE，`RiskLevel = "LOW"`，`Reason = "SWEEP_AML_OK"`
- 其他 risk → REJECT，`RiskLevel = risk`，`Reason = "SWEEP_AML_RISK_<risk>"`

**单测矩阵**（表格驱动 15 条子用例）: TRIGGERED × {LOW / MEDIUM / HIGH / SEVERE / UNKNOWN / PENDING / FAILED / SKIPPED / 空数组 / null / mixed-high / invalid-json} + {UNTRIGGERED / IN_PROGRESS / 空字符串}。每条断言 `Approve` + `RiskLevel` + `Reason` 三元组。

### 2.2 ApprovalRecord / Repository / Approver / Service 串联

**改 `internal/approval/approver.go`**: `ApprovalDecision` 加 `AmlRiskLevel string` 字段。

**改 `internal/approval/models.go`**: `ApprovalRecord` 加 `AmlRiskLevel string ` 带 `json:"amlRiskLevel,omitempty" db:"aml_risk_level"`。

**改 `internal/approval/repository.go`**: `InsertApprovalRecord` + `GetApprovalByID` 加 `aml_risk_level` 列；空字符串通过 `sql.NullString{Valid: false}` 落 NULL。

**改 `internal/approval/transaction_approver.go`**: `evaluateAutoSweep` 和 `evaluateUTXOCollection` 在现有"VAULT_ACCOUNT + 白名单"校验后，追加：

```go
amlDecision := DecideSweepAML(detail.AmlScreeningTriggeredState, detail.AmlList)
if !amlDecision.Approve {
    return &ApprovalDecision{
        Action:       "REJECT",
        Reason:       fmt.Sprintf("AUTO_SWEEP rejected: %s (risk=%s)", amlDecision.Reason, amlDecision.RiskLevel),
        AmlRiskLevel: amlDecision.RiskLevel,
    }, nil
}
return &ApprovalDecision{
    Action:       "APPROVE",
    Reason:       fmt.Sprintf("AUTO_SWEEP approved (AML=%s)", amlDecision.RiskLevel),
    AmlRiskLevel: amlDecision.RiskLevel,
}, nil
```

`evaluateAutoFuel` **不改**（D-AML-5）。`AmlRiskLevel` 字段保持零值 `""`。

**改 `internal/approval/service.go`**: 组装 ApprovalRecord 时 `record.AmlRiskLevel = decision.AmlRiskLevel`；REJECT 告警 fields 追加 `"riskLevel": decision.AmlRiskLevel`，告警级别一律 `WARN`（D-AML-7）。

### 2.3 测试增量

**`transaction_approver_test.go` 9 条新增**:

| 场景 | tx_type | state | risk | 白名单 | 期望 Action | 期望 AmlRiskLevel |
|------|---------|-------|------|-------|------|-------|
| autosweep low approves | AUTO_SWEEP | TRIGGERED | LOW | ✓ | APPROVE | LOW |
| autosweep high rejects | AUTO_SWEEP | TRIGGERED | HIGH | ✓ | REJECT | HIGH |
| autosweep medium rejects | AUTO_SWEEP | TRIGGERED | MEDIUM | ✓ | REJECT | MEDIUM |
| autosweep untriggered rejects | AUTO_SWEEP | UNTRIGGERED | — | ✓ | REJECT | STATE_UNTRIGGERED |
| autosweep state missing rejects | AUTO_SWEEP | `""` | — | ✓ | REJECT | STATE_MISSING |
| autosweep whitelist fail before aml | AUTO_SWEEP | TRIGGERED | LOW | ✗ | REJECT | `""` |
| utxo high rejects | UTXO_COLLECTION | TRIGGERED | HIGH | ✓ | REJECT | HIGH |
| utxo low approves | UTXO_COLLECTION | TRIGGERED | LOW | ✓ | APPROVE | LOW |
| autofuel high approves | AUTO_FUEL | TRIGGERED | HIGH | ✓ | APPROVE | `""` |

**`repository_test.go` 2 条**:
- INSERT 含 aml_risk_level；空字符串落 NULL
- SELECT 读 NULL 返回 ""

**`service_test.go` 2 条**:
- LOW → InsertApprovalRecord 参数 aml_risk_level="LOW"
- HIGH → 同上 + alert hook 被调用一次，fields 含 `riskLevel="HIGH"` + level=`"WARN"`

### 2.4 现有测试的预期回归（⚠️ 不是 bug）

现有 `transaction_approver_test.go` 中 AUTO_SWEEP / UTXO_COLLECTION 的 APPROVE 用例多半没 mock AML 字段，改造后会变 REJECT。**修复方法**：在原 mock 输入补：

```go
AmlScreeningTriggeredState: "TRIGGERED",
AmlList: json.RawMessage(`[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"LOW"}]`),
```

REJECT 用例（白名单失败 / 非 VAULT_ACCOUNT）不需要改。

### 2.5 验收（§13.13 Phase 1 全部 10 条）

```bash
go build ./cmd/server/ && go vet ./...
go test ./... -race -count=1
go test ./internal/safeheron/... ./internal/approval/... -coverprofile=cover.out
go tool cover -func=cover.out | grep -E "aml\.go|sweep_aml\.go|transaction_approver\.go"
```

**Checkpoint**:
- [ ] §13.13 Phase 1 验收 10 条全部对应测试通过
- [ ] `safeheron/aml.go` = 100%，`approval/sweep_aml.go` = 100%，`approval/transaction_approver.go` ≥ 90%
- [ ] 全量 `go test ./... -race` 不引入新失败（与 `monera_test_baseline_failures.md` 对照）
- [ ] `go vet ./...` 无报错

---

## 决策反向索引

施工对设计有疑问 → 反查 spec §13.1 的 D-AML-*：

| 关联任务 | 决策 ID | 摘要 |
|---------|--------|------|
| Task 1 | D-AML-10 | SummarizeRiskLevel 上提到 safeheron |
| Task 1, 2 | D-AML-11 | Phase 1 加 aml_risk_level 列 |
| Task 2 | D-AML-1 | AML 数据源 = callback 字段 |
| Task 2 | D-AML-3 | 非标准状态全 REJECT（fail closed） |
| Task 2 | D-AML-5 | AUTO_FUEL 不改造 |
| Task 2 | D-AML-7 | Phase 1 告警一律 WARN |
| Phase 2 | D-AML-2 / 4 / 6 / 8 / 9 / 12 | override 表 / admin API / 节奏 / schema 扩展 / 鉴权 / bypass — **不在 Phase 1**

## 风险

| 风险 | 概率 | 缓解 |
|------|------|------|
| Task 1 迁移漏改某调用点导致 vet 红 | 低 | 1.3 节 grep 检查；编译期可发现 |
| Task 2 现有 APPROVE 测试集体变红 | 高 | 预期；按 2.4 节补 mock |
| 全量 race 出现 data race | 低 | 无新增 goroutine |
| Phase 1 上线后地址池资金积压 | 中 | 部署前确认 Safeheron 归集策略可暂停；spec §13.11 已记录 |
