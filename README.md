# Zone Rollout Control

Zone Rollout Control 是仅依赖 Go 标准库的本机 HTTP 控制面，用于协调区域版本上线。它管理滚动计划、不可变区域版本、解析节点租约、探针结果和一致性判定。

本仓库是健康项目初始化基线。当前不包含单元测试、夹具或浏览器测试；测试工作有意延后到后续测试阶段。现在通过真实 HTTP 路由的生产探针验证构建和四条核心工作流。

## 环境要求

- Go 1.22 或更高版本。
- 本机可用 TCP 端口。
- 无需外部数据库、远程 DNS 服务或环境变量。

## 构建

```bash
go build ./...
```

## 启动服务

```bash
go run ./cmd/zrc \
  --addr 127.0.0.1:8090 \
  --state data/zrc-state.json
```

参数：

- `--addr`：HTTP 监听地址，默认 `127.0.0.1:8090`。
- `--state`：JSON 状态快照路径，默认 `data/zrc-state.json`。
- `--shutdown-timeout`：中断后的关闭窗口，默认 `10s`。

服务会锁定 `<state>.lock`，避免两个进程同时修改同一份状态。每次成功事务先复制状态，再通过同目录临时文件和原子替换写入快照。

## 目录结构

```text
cmd/zrc                     服务进程入口
cmd/workflowprobe           工作流探针入口
internal/domain             聚合、DNS 值校验、状态迁移和判定计算
internal/store              锁内副本事务和原子 JSON 持久化
internal/platform           时钟、标识、游标和文件锁
internal/plan               计划及区域版本服务
internal/fleet              解析节点档案服务
internal/allocation         节点选择和租约生命周期
internal/probe              探针登记、幂等重放和租约完成
internal/assessment         一致性聚合和最新结果查询
internal/freeze             区域变更冻结策略和拦截记录
internal/httpapi            HTTP 路由、中间件和响应映射
internal/workflowprobe      真实 HTTP 路径验证
```

## 快速使用

以下示例假设服务监听 `127.0.0.1:8090`。

登记解析节点：

```bash
curl -sS -X PUT http://127.0.0.1:8090/v1/nodes \
  -H 'Content-Type: application/json' \
  -d '{
    "node_name": "resolver-east-01",
    "region": "east"
  }'
```

创建滚动计划：

```bash
curl -sS -X PUT http://127.0.0.1:8090/v1/plans \
  -H 'Content-Type: application/json' \
  -d '{
    "zone": "edge.example",
    "minimum_success_ratio": 1,
    "maximum_latency_ms": 80,
    "expected_nodes": 2
  }'
```

添加区域版本：

```bash
curl -sS -X PATCH http://127.0.0.1:8090/v1/plans/<plan_id>/revisions \
  -H 'Content-Type: application/json' \
  -d '{
    "serial": 2026092501,
    "authority": "ns1.edge.example",
    "entries": [
      {"owner": "edge.example", "type": "A", "value": "192.0.2.10"},
      {"owner": "www.edge.example", "type": "CNAME", "value": "edge.example"}
    ]
  }'
```

激活计划：

```bash
curl -sS -X PATCH http://127.0.0.1:8090/v1/plans/<plan_id>/activate \
  -H 'Content-Type: application/json' \
  -d '{"expected_version": 2}'
```

分配节点租约：

```bash
curl -sS -X PATCH http://127.0.0.1:8090/v1/plans/<plan_id>/leases \
  -H 'Content-Type: application/json' \
  -d '{
    "request_key": "lease-request-0001",
    "region": "east",
    "lease_seconds": 600,
    "max_nodes": 2
  }'
```

提交探针：

```bash
curl -sS -X PATCH http://127.0.0.1:8090/v1/leases/<lease_id>/probes \
  -H 'Content-Type: application/json' \
  -d '{
    "request_key": "probe-request-0001",
    "success": true,
    "observed_serial": 2026092501,
    "latency_ms": 12,
    "observed_at": "2026-09-25T12:00:00Z"
  }'
```

请求一致性判定：

```bash
curl -sS -X PATCH http://127.0.0.1:8090/v1/plans/<plan_id>/assessments \
  -H 'Content-Type: application/json' \
  -d '{"request_key": "assess-request-0001"}'
```

## 区域变更冻结

重大活动期间可以登记冻结策略，让系统代替人工约定阻止误操作。策略按区域（或 `*` 表示全部区域）、范围和时间窗口生效：

```bash
curl -sS -X PUT http://127.0.0.1:8090/v1/freeze-policies \
  -H 'Content-Type: application/json' \
  -d '{
    "zone": "edge.example",
    "scope": "all",
    "reason": "National day ceremony traffic protection",
    "exception_roles": ["release_manager"],
    "starts_at": "2026-10-01T09:00:00+08:00",
    "ends_at": "2026-10-02T09:00:00+08:00"
  }'
```

- `scope` 取 `all`、`revision`、`activation` 或 `allocation`，决定覆盖哪些入口。
- 时间窗口为半开区间 `[starts_at, ends_at)`，按 UTC 时刻比较，可用任意时区偏移书写；到达 `ends_at` 即自动失效。
- 生效期间，追加区域版本、激活计划和分配租约三个入口返回同一条解释，错误码为 `zone_frozen`（HTTP 409）。
- 调用方通过 `X-Actor-Role` 请求头声明角色；只有全部匹配中的生效策略都例外该角色时才放行（重叠冻结取最严）。
- 更新策略用 `PATCH /v1/freeze-policies/{id}`，撤销用 `PATCH /v1/freeze-policies/{id}/revoke`，两者都需要 `expected_version` 匹配当前版本。
- 每次拦截都会留下不可变记录，通过 `GET /v1/freeze-interceptions`（支持 `zone`、`policy_id` 过滤和分页）查询；策略失效或撤销不会抹除已有记录。

冻结生效前已经开始的合法流程按以下规则继续：

1. 已领取的租约可正常提交探针、释放和过期，节点不会被冻结卡住。
2. 已记录的分配请求用相同请求键和内容重放时仍返回原分配组。
3. 已激活的计划可正常完成；探针登记和一致性判定不被拦截。
4. 被拦截的请求不留下任何幂等记录，解冻或撤销后用相同参数重试即可。

未配置策略的区域行为完全不变。

## API 概览

成功响应：

```json
{
  "data": {},
  "meta": {
    "request_id": "req_..."
  }
}
```

失败响应：

```json
{
  "error": {
    "code": "validation_failed",
    "message": "plan input is invalid",
    "details": {
      "zone": "contains unsupported characters"
    }
  }
}
```

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/healthz` | 进程健康检查 |
| `PUT` | `/v1/nodes` | 登记解析节点 |
| `GET` | `/v1/nodes` | 按状态和区域分页查询节点 |
| `PUT` | `/v1/plans` | 创建计划草稿 |
| `GET` | `/v1/plans` | 按状态分页查询计划 |
| `GET` | `/v1/plans/{id}` | 查询计划 |
| `PATCH` | `/v1/plans/{id}/revisions` | 添加区域版本 |
| `PATCH` | `/v1/plans/{id}/activate` | 激活计划 |
| `PATCH` | `/v1/plans/{id}/complete` | 完成计划 |
| `PATCH` | `/v1/plans/{id}/leases` | 给节点分配租约 |
| `PATCH` | `/v1/leases/{id}/release` | 提前释放租约 |
| `PATCH` | `/v1/leases/{id}/probes` | 登记探针并完成租约 |
| `PATCH` | `/v1/plans/{id}/assessments` | 创建一致性判定 |
| `GET` | `/v1/plans/{id}/assessments/latest` | 查询最近判定 |
| `PUT` | `/v1/freeze-policies` | 登记区域变更冻结策略 |
| `GET` | `/v1/freeze-policies` | 按区域和生效状态分页查询策略 |
| `GET` | `/v1/freeze-policies/{id}` | 查询单条策略 |
| `PATCH` | `/v1/freeze-policies/{id}` | 更新策略（乐观并发） |
| `PATCH` | `/v1/freeze-policies/{id}/revoke` | 撤销策略 |
| `GET` | `/v1/freeze-interceptions` | 查询拦截记录 |

## 状态及失败行为

- 计划按 `draft -> active -> completed` 单向迁移，激活和完成需要匹配版本。
- 区域版本只在计划草稿状态可追加，序列号严格递增且内容不可变。
- A、AAAA、CNAME 条目在写入前规范化并稳定排序，重复条目直接拒绝。
- 节点按 `ready -> leased -> ready` 变化，隔离节点不会被选中。
- 一个节点同一时刻最多有一个 `claimed` 租约。
- 相同请求键和相同内容可安全重放；同键异载荷返回 `idempotency_conflict`。
- 一致性判定综合节点覆盖率、序列号、成功比例和最大延迟，返回 `consistent` 或 `review_required`。
- 所有校验和冲突在快照替换前返回，不留下部分状态。

## 后续工程任务的行为边界

项目保留了以下可独立观察和验证的生产边界，适合后续工程任务围绕它们设计需求：

1. 计划草稿、区域版本、激活、完成和版本冲突。
2. DNS 名称规范化、IP 校验、条目去重和稳定排序。
3. 序列号递增与重复版本拒绝。
4. 节点隔离、区域筛选、稳定选择和唯一租约。
5. 租约期限、释放、过期清理和重复释放。
6. 租约请求键与请求指纹重放。
7. 探针请求键、载荷指纹、租约完成原子性和冲突检测。
8. 成功率、节点覆盖、序列号一致性和延迟上限计算。
9. 原子快照替换、文件锁、进程关闭和并发事务串行化。
10. 严格 JSON 解码、分页游标和领域错误到 HTTP 状态的映射。

这些是正常运行机制，不是预置缺陷，也不包含缺陷票据、参考答案或测试文件。

## 验证

测试阶段有意延后，因此当前不运行单元测试或浏览器测试。以下命令分别验证四条工作流；每条命令启动真实本机 HTTP 服务、使用独立临时状态、覆盖成功与失败路径，然后关闭服务：

```bash
go run ./cmd/workflowprobe plan-revision-lifecycle
go run ./cmd/workflowprobe node-lease-allocation
go run ./cmd/workflowprobe probe-ingestion
go run ./cmd/workflowprobe consistency-assessment
go run ./cmd/workflowprobe zone-freeze-enforcement
```

探针不访问外部网络，也不依赖已有状态文件。
