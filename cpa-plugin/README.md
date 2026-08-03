# grok2api-egress

> 纯 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 原生插件：多出口节点管理 · 账号粘性绑定 · 被动/主动质量探测 · 降智隔离与自动迁出。
> **零 Grok2API 运行时依赖** — 账号、代理、探测全部走 CPA Host API。

从代理出口规划、每节点账号容量、Docker 网络、隔离迁号，到强制住宅 IP 轮换和真实模型复测的完整操作见 [AI 部署与运维指南](./AI_USAGE_GUIDE.md)。

| | |
|---|---|
| 插件名 | `grok2api-egress` |
| 当前版本 | **1.0.5** |
| 语言 | Go (`-buildmode=c-shared` → `.so`) |
| CPA SDK | `CLIProxyAPI/v7` (`pluginabi` / `pluginapi`) |
| 能力 | Management UI + Usage Plugin + Scheduler + Request Interceptor |
| License | MIT（见仓库根目录 `LICENSE`） |

---

## 它解决什么问题

多账号 + 多出口（住宅/ISP 代理）跑 xAI / Grok 时，常见两类故障：

1. **出口降智 / 限速**：同一出口 IP 被打穿后，输出 Token/s 可能异常飙升，表现像“模型变笨”。
2. **账号与出口纠缠**：出问题后账号仍粘在坏出口上，整池成功率一起塌。

本插件在 CPA 内完成：

- 把出口抽象成 **Node**（存 proxy URL）
- 把 CPA `xai-*.json` 账号的 `proxy_url` **粘性绑定**到 Node
- 用 **被动 usage 观测 + 主动 quality probe** 判定 healthy / soft / hard / error；账号、额度、上游权限失败只记录为 ignored，不消耗出口错误次数
- **隔离（quarantine）坏节点**，并 **migrate** 账号到健康通道
- 调度阶段跳过隔离/冷却账号；选定账号与迁移发生竞态时返回可重试的 `503 + Retry-After: 1`
- 可选调用受信任的内部换 IP Webhook；只有确认出口 IP 已变化并通过真实模型复测才恢复节点
- 提供完整 **管理 UI**（节点 CRUD、批量、重平衡、质量测试、策略、事件）

灵感来自 Grok2API 侧的 quality-guard / egress 思路，但实现已完全 native 化，**不需要、也不连接 Grok2API**。

**CPA 本身不会导致模型降智。** 本插件是多账号、多出口场景下的可选熔断器；如果只有单账号或稳定静态代理，没有出口轮换和迁号需求，可以不安装。

---

## 架构

```
┌─────────────────────────────────────────────────────────┐
│  CLIProxyAPI (host)                                     │
│   ├─ Management UI  ──► /v0/resource/plugins/.../status │
│   ├─ Management API ──► /v0/management/grok2api-egress  │
│   ├─ Usage hook     ──► MethodUsageHandle (被动 TPS)    │
│   └─ Auth files     ──► xai-*.json (proxy_url 绑定)     │
│                                                         │
│  plugin: grok2api-egress.so                             │
│   ├─ store.go      状态持久化 (nodes/policy/events)      │
│   ├─ auth_bind.go  绑定 / 重平衡 / 迁出 / 禁用          │
│   ├─ guard.go      探测 · 分类 · 隔离 · 恢复            │
│   ├─ main.go       ABI · UI 代理 · API 路由             │
│   └─ page.html     管理台（embed）                      │
└─────────────────────────────────────────────────────────┘
          │ sticky proxy_url
          ▼
   Node1 :7951   Node2 :7952   Node3 :7953   … (任意 HTTP/SOCKS)
```

**状态文件**（默认）：

```text
/CLIProxyAPI/plugin-data/egress-guard/state.json
```

可通过插件配置字段 `state_file` 覆盖。请把该路径挂进容器可写卷。

---

## 功能一览

### 节点（Egress Node）

| 能力 | 说明 |
|---|---|
| CRUD | 名称、proxy URL、启用、容量、是否参与池 |
| 批量导入 | 单行代理 URL，或 `名称 | 代理 URL | 容量 | fixed/pool`；最多 500 条、整批原子写入 |
| 连通性测试 | 经该出口探测外网出口 IP / 延迟 |
| 质量测试 | 真实 chat 探测，计算 output Token/s |
| 绑定账号 | 查看粘在该节点 `proxy_url` 上的账号列表 |
| 批量启停 / 删除 | 删除前自动解绑 proxy |

### 账号粘性与调度

| 能力 | 说明 |
|---|---|
| Rebalance | 把启用中的 xAI 账号均分到健康节点 |
| Migrate on quarantine | 隔离后立刻把账号迁到其他健康节点 |
| Disable on hard（可选） | 隔离后无健康通道可迁移或迁移失败时，兜底 disable 原节点账号 |

绑定介质是 CPA auth JSON 里的 **`proxy_url` 字段**，不引入外部账号库。

### 质量守护（降智隔离）

| 模式 | 行为 |
|---|---|
| `passive` | 只吃 Usage 事件，算 TPS 分类 |
| `active` | 定时对节点做 quality probe |
| `hybrid`（默认） | 被动 + 主动 |

分类阈值（默认，可在 UI 改）：

| 等级 | 默认阈值 | 动作 |
|---|---|---|
| healthy | TPS &lt; soft | 保持 |
| soft | ≥ `soft_tps`（500） | 连续 N 次 → 隔离 |
| hard | ≥ `hard_tps`（1000）且满足最小 Token 证据 | 立即隔离 |
| error | 探测失败 | 连续 N 次 → 隔离 |

隔离时：

1. 节点 `quarantined_until = now + quarantine_seconds`
2. 记事件 `node_quarantined`
3. 同步摘除受影响账号，仅迁移到近期主动检测 healthy 且出口 IP 不同的节点（`accounts_migrated`）
4. 到期后 probe 通过 → `node_restored`；可选换 IP Webhook 必须先确认新 IP 与旧 IP 不同

保护项：

- `min_healthy_nodes`：低于阈值则 **suppressed**，避免全军覆没
- `min_generation_ms = 1000`：极短生成窗口不虚高 TPS，降低 loadtest / 短回复误隔离
- `min_output_tokens = 32`：证据不足的短输出标记为 ignored，不触发 soft/hard 隔离

### 管理 UI

菜单名：**出口守护**
路径：CPA 管理台 → 插件资源 `/status`

- 总览指标（健康 / 软 / 硬 / 隔离）
- 节点表：状态徽章、TPS、出口 IP、绑定数、隔离倒计时
- 行内：连通测试 / 质量测试 / 编辑 / 绑定账号 / 启停
- 策略表单、事件时间线、一键重平衡
- 单条添加与逐行批量导入；保存后的代理 URL 不读取、不回显

UI 经 management 代理，请求头需 `X-Grok2API-Egress-UI: 1`（页面已内置）。

---

## 目录结构

```text
cpa-plugin/
├── go/
│   ├── main.go          # CGO ABI、注册、Management/Usage 入口
│   ├── store.go         # state.json 读写、节点/策略/事件
│   ├── auth_bind.go     # list/get/save auth、rebalance、migrate
│   ├── guard.go         # TPS、probe、quarantine、background worker
│   ├── page.html        # 管理 UI（go:embed）
│   ├── tokens.css       # 设计 token
│   ├── main_test.go
│   ├── go.mod
│   └── grok2api-egress.so   # 构建产物（勿提交可执行二进制到 git；可 CI 产出）
├── loadtest/
│   ├── cpa-egress-loadtest.py   # 持续压测
│   └── cpa-egress-monitor.py    # 健康监视 + 告警 JSONL
├── import_from_g2a.py           # （可选）从 Grok2API 导出账号一次性导入 CPA
└── README.md
```

---

## 构建

依赖：Go 1.22+（开发环境曾用 1.26）、CGO、与目标 CPA 同架构的 glibc。

```bash
cd go
go mod tidy
go build -buildmode=c-shared -o grok2api-egress.so .
```

产物：

- `grok2api-egress.so`
- `grok2api-egress.h`（可忽略，host 不依赖此头）

交叉编译注意：`.so` 必须与 **CPA 进程架构 / libc** 一致（常见 `linux/amd64`）。

---

## 安装（CPA）

### 插件商店（推荐）

在 CPA 管理中心打开插件商店，搜索 **Grok Egress Guard** 或插件 ID
`grok2api-egress`，选择与 CPA 主机架构一致的版本安装。发布包提供
`linux/amd64` 和 `linux/arm64`；商店会从 GitHub Release 下载并校验
`checksums.txt`。升级时在同一条目选择新版本即可，状态文件不会随插件二进制覆盖。

如果商店暂未收录，可从仓库最新 Release 下载对应 zip，校验
`checksums.txt` 后按下面的手动方式安装。

### 手动安装

1. 复制插件：

```bash
cp grok2api-egress.so /path/to/CLIProxyAPI/plugins/
```

2. 确保可写状态目录（compose 示例）：

```yaml
volumes:
  - ./plugin-data/egress-guard:/CLIProxyAPI/plugin-data/egress-guard
```

3. 插件配置（CPA 插件 YAML）：

```yaml
state_file: /CLIProxyAPI/plugin-data/egress-guard/state.json
# 可选：仅配置到受信任的内部服务；留空即完全关闭自动换 IP
rotation_url: http://rotation-service:19099/rotate
# 令牌只从 CPA 容器环境读取，不写入 YAML 或 state.json
rotation_token_env: EGRESS_ROTATION_TOKEN
rotation_timeout_seconds: 45
# 只允许这些 Node ID 触发自动轮换，留空即禁止
rotatable_node_ids: ["1", "2"]
```

`rotation_url` 需要接受 `POST {"nodeId":"...","oldExitIp":"..."}`，返回
`{"newExitIp":"..."}`。插件会拒绝空 IP、未变化 IP 和非 2xx 响应，随后仍会使用真实模型探测确认质量；Webhook 成功本身不会解除隔离。

4. 重启 CPA，管理台应出现菜单 **「出口守护」**。

---

## 快速上手

1. **加节点**
   每个出口一条，例如：
   - `http://127.0.0.1:7951`
   - `http://127.0.0.1:7952`
   - `http://127.0.0.1:7953`
   （CPA 若用 host 网络，可直接打本机 sticky 代理端口。）

   多个节点可点 **「批量添加」**，每行使用以下任一格式：

   ```text
   socks5h://user:pass@host:port
   美西固定 01 | http://user:pass@host:port | 20 | fixed
   轮换池 01 | socks5h://user:pass@host:port | 50 | pool
   ```

   空行以及 `#`、`//` 开头的注释会忽略。任一行无效时整批拒绝，不会留下部分节点；导入完成后 API 和页面都不会回显代理 URL。

2. **连通测试** → 确认 `exit_ip` 各不相同（真正粘在不同出口）。

3. **导入 / 准备 xAI 账号**
   CPA 标准 auth 文件；本仓库 `import_from_g2a.py` 可做一次性迁移。
   **注意**：若账号 refresh token 只能单端使用，导入 CPA 后请在原端禁用，避免双端互踢。

4. **一键重平衡**
   UI「重平衡」或 API `POST /nodes/rebalance`，把账号 `proxy_url` 均分到健康节点。

5. **开 hybrid 守护**
   默认即可；按业务调 `soft_tps` / `hard_tps` / `quarantine_seconds`。

6. **质量测试**
   单节点「质量测试」应返回 healthy + 合理 TPS；401 时检查 xAI auth 是否带齐客户端头（插件 probe 会强制 `X-XAI-Token-Auth` 等）。

---

## Management API 摘要

入口（CPA）：

```http
POST /v0/management/grok2api-egress/api
Header: X-Grok2API-Egress-UI: 1
Content-Type: application/json

{
  "method": "GET|POST|PUT|PATCH|DELETE",
  "path": "/status",
  "body": { }
}
```

| path | method | 作用 |
|---|---|---|
| `/status` | GET | 总览、节点 map、策略、事件、统计 |
| `/policy` | GET/PUT | 读写守护策略 |
| `/nodes` | GET/POST/DELETE | 列表 / 创建 / 批量删 |
| `/nodes/import` | POST | 原子批量创建 1-500 个节点；代理 URL 不回显 |
| `/nodes/batch` | PATCH | 批量启停 |
| `/nodes/test` | POST | 批量连通测试 |
| `/nodes/rebalance` | POST | 账号重平衡 |
| `/nodes/{id}` | GET/PUT/DELETE | 单节点 |
| `/nodes/{id}/test` | POST | 连通测试 |
| `/nodes/{id}/quality-test` | POST | 质量探测 |
| `/nodes/{id}/accounts` | GET | 绑定账号列表 |

（另保留若干 `/quality-guard/*` 别名路径，便于从旧 UI 习惯迁移。）

---

## 压测与监视（可选）

```bash
# 持续 chat 压测（示例）
python3 loadtest/cpa-egress-loadtest.py \
  --base http://127.0.0.1:8317/v1 \
  --api-key "$CPA_LOADTEST_API_KEY" \
  --workers 3 --max-tokens 16 --hours 2 \
  --log-dir /var/log/cpa-loadtest

# 旁路监视（读插件 status + loadtest 进度，写 monitor.jsonl）
CPA_LOADTEST_LOG_DIR=/var/log/cpa-loadtest \
  python3 loadtest/cpa-egress-monitor.py
```

监视脚本只从 `CPA_MANAGEMENT_KEY` 读取管理密钥；如管理地址不是本机，设置
`CPA_MANAGEMENT_BASE_URL`。压测和监视日志应写到仓库外的运行目录。

建议：

- 使用 **专用 CPA API Key**，与生产流量隔离
- 短 `max_tokens` 适合打通链路；测真实降智请加大生成量并相应调高 `hard_tps` 判定窗口
- `NO_PROXY` / 直连 CPA，避免本机 HTTP_PROXY 把管理/业务流量拐走

### 历史实测快照（v1.0.3）

| 项 | 结果 |
|---|---|
| 时长 | ~30 min（SIGINT 停止） |
| 请求 | ok **985** / fail **50**（fail 多为重启窗口历史错误，稳态后几乎不再涨） |
| 成功率 | **~95.2%** |
| 节点 | 3 sticky 出口，分配 184 / 183 / 183 |
| 守护动作 | quarantined **4** · restored **6** · suppressed **9** |
| 结束态 | **Q=0 · H=3**，三通道 healthy |

---

## 设计要点（给贡献者）

1. **Auth → Node 映射**
   以 `proxy_url` 字符串相等为键；Usage 事件里的 auth 标识会经 cache（index / id / name / email / path）反查。映射失败的异常只记录诊断事件，不猜测并隔离某个“最繁忙”节点，避免误杀。

2. **Quality probe**
   强制 Grok/xAI 客户端头；节点上多账号轮试，降低单账号 401 误判。

3. **隔离与恢复**
   quarantine 写状态 → 同步摘除账号 → 仅迁移到近期主动检测 healthy 且出口 IP 不同的节点 → 后台 worker 到期探测 → restore。迁移写入后会从 CPA Host API 再读一次，校验 `proxy_url` 与 disabled 状态。

4. **误报控制**
   - 最短生成窗口 `min_generation_ms`
   - 小输出标记 ignored，不重置或增加异常 strike
   - `min_healthy_nodes` 抑制

5. **安全**
   - UI API 校验自定义头，降低 CSRF 式误触
   - `proxy_url` 对前端 DTO 可做脱敏（按部署需要加强）
   - 状态文件含出口 URL，权限应仅 host 可读

---

## 从 Grok2API 迁移（可选）

`import_from_g2a.py`：读取 Grok2API 侧账号导出 → 写成 CPA xAI auth JSON → 可选在源端 disable refresh，保证 **token 只服务 CPA**。

这是一次性迁移工具，不是插件运行时依赖。运行前通过环境变量提供
`GROK2API_ADMIN_USERNAME`、`GROK2API_ADMIN_PASSWORD`，并按需设置
`GROK2API_BASE_URL`、`CPA_AUTH_DIR`；不要把这些值写进仓库或命令示例。

迁移清单：

- [ ] 导出账号
- [ ] 写入 CPA auth 目录
- [ ] 源端禁用 / 停止 refresh
- [ ] CPA 建齐 egress 节点
- [ ] rebalance
- [ ] 小流量 quality-test
- [ ] 再放生产

---

## 路线图 / 欢迎 PR

- [x] 隔离/恢复状态机、迁移后校验与请求竞态保护
- [ ] 事件/统计按时间窗滑动，避免历史 5xx 永久污染告警
- [ ] 节点维度的成功率 SLO 与自动扩缩绑定
- [x] CI：`go test` + Linux amd64/arm64 `.so` release
- [ ] 英文 UI / i18n
- [ ] 补 SPDX License

---

## 致谢

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件 ABI 与 Host API
- Grok2API quality-guard 社区对「出口降智」问题的早期实践

---

## 免责声明

本项目用于 **自有基础设施** 上的出口质量治理与账号调度。请遵守 xAI / 代理服务商 ToS 与当地法律；不要把他人的 token、未授权的出口或生产密钥提交进仓库。开源发布前请清理：

- 真实 API Key / refresh token
- 生产 `state.json`
- loadtest 日志与 `monitor.jsonl`
- 内网 IP / 家庭出口指纹（文档里用占位符）
