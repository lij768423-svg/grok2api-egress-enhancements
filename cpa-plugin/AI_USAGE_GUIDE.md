# CPA 出口守护 AI 部署与运维指南

本文用于让 AI 工具或运维人员从零部署、配置和维护 `grok2api-egress` v1.0.4。插件是纯 CPA 原生实现，只读写 CLIProxyAPI（下称 CPA）的 xAI auth 文件和 Usage 事件，不依赖 Grok2API 运行时。

CPA 本身不会导致模型降智。这个插件只是在多账号、多出口场景中把 Token/s 等信号用作出口熔断依据；单账号或稳定静态代理部署可以不启用。

> 安全边界：不要把真实代理用户名、密码、CPA 管理密钥、xAI token、`state.json` 或生产日志交给 AI。示例里的 `<...>` 都必须在本机私密配置中替换，不能提交到 Git。

## 1. 先理解它会做什么

一条出口节点（Node）对应一个 CPA 可访问的代理 URL。插件把 xAI auth JSON 中的 `proxy_url` 设为这个 URL，以此建立账号到出口的粘性绑定。

```text
普通请求 -> CPA 选择账号 -> 读取该账号 proxy_url -> 固定出口
                    |
                    +-> Usage 事件 -> 被动 Token/s 检测

定时任务 -> 真实模型主动探测 -> 节点分类 -> 隔离/恢复
异常节点 -> 账号迁到其他健康节点
```

插件负责：

- 出口节点的添加、编辑、删除、启停和批量操作；
- xAI 账号的 `proxy_url` 粘性绑定和重平衡；
- 被动请求审计与定时真实模型探测；
- 节点隔离、账号迁出、到期复测和健康恢复；
- 节点、事件、统计和策略的管理 UI。

插件不负责：

- 购买代理或绕过代理商的访问限制；
- 调用代理商的“换 IP”接口；
- 自动修改 sticky session 用户名；
- 保证 `account_capacity` 永远不超限；
- 判断模型真实智力。Token/s 只是用于熔断的可观测特征。

## 2. 规划出口和账号容量

### 2.1 最小拓扑

生产建议至少准备 3 个相互独立的出口节点：

```text
节点 A -> sticky 会话 A -> 出口 IP A
节点 B -> sticky 会话 B -> 出口 IP B
节点 C -> sticky 会话 C -> 出口 IP C
```

如果三个本地端口最终显示同一个 `exit_ip`，它们只是三个入口，不是三个独立出口，不能形成真正的故障域隔离。

### 2.2 每个出口挂多少账号

账号数量本身不会直接产生压力，并发请求数、请求长度和代理商风控更关键。没有自己的压测数据时，使用以下保守起点：

| 阶段 | 建议账号数/出口 | 适用条件 |
|---|---:|---|
| 首次上线 | 50-100 | 低并发、小流量观察 |
| 稳定扩容 | 100-150 | 已连续观察成功率和质量事件 |
| 高密度 | 150-200 | 已用实际业务压测证明稳定 |

始终预留 20%-30% 的迁移余量。容量计算不要只看正常状态，还要看“坏一个节点后”的状态：

```text
故障后单节点负载 = 总账号数 / (总节点数 - 允许同时故障数)
```

例：3 个节点，每个节点实测安全承载 100 个账号，要求容忍 1 个节点隔离，则总账号数不应超过约 200，而不是 300。若计划放 240 个账号，剩余两个节点故障时各承受约 120 个，必须先确认 120 仍安全。

`account_capacity` 是重平衡时的目标容量，不是硬上限：

- 正常 `rebalance` 会优先跳过已达到容量的节点；
- 如果所有节点都满了，当前实现仍会把剩余账号放到最后一个可用节点；
- 隔离迁号当前按健康节点轮询，不把容量当作绝对硬限制。

因此必须从拓扑上预留容量，不能依赖一个数字阻止超配。

## 3. 启动代理出口

### 3.1 方案 A：CPA 直接连接上游代理

如果 CPA 所在网络可以直接访问代理商入口，节点 URL 可直接填写：

```text
http://<PROXY_USER_A>:<PROXY_PASS>@<PROXY_HOST>:<PROXY_PORT>
http://<PROXY_USER_B>:<PROXY_PASS>@<PROXY_HOST>:<PROXY_PORT>
http://<PROXY_USER_C>:<PROXY_PASS>@<PROXY_HOST>:<PROXY_PORT>
```

动态住宅代理通常通过用户名中的 sticky session 区分出口，例如：

```text
<ACCOUNT>-region-US-sid-<SESSION_A>-t-10
<ACCOUNT>-region-US-sid-<SESSION_B>-t-10
<ACCOUNT>-region-US-sid-<SESSION_C>-t-10
```

具体字段以代理商文档为准。`t-10` 一般表示 10 分钟粘性，但不同代理商语义不完全相同。

先在 CPA 主机验证每条 URL：

```bash
curl --fail --max-time 20 \
  --proxy 'http://<PROXY_USER_A>:<PROXY_PASS>@<PROXY_HOST>:<PROXY_PORT>' \
  https://api.ipify.org
```

三个会话应能连通，且应得到预期的不同出口 IP。

### 3.2 方案 B：本地端口映射到独立 sticky 会话

当上游代理需要链式中转、统一管理或热重载时，可用本地代理侧车暴露固定端口：

```text
7951 -> 上游 sticky A
7952 -> 上游 sticky B
7953 -> 上游 sticky C
```

下面是 sing-box 的脱敏示例。请固定经过验证的镜像版本，不要在生产长期使用 `latest`。

`sing-box.json`：

```json
{
  "log": { "level": "info" },
  "inbounds": [
    { "type": "mixed", "tag": "sticky-a-in", "listen": "0.0.0.0", "listen_port": 7951 },
    { "type": "mixed", "tag": "sticky-b-in", "listen": "0.0.0.0", "listen_port": 7952 },
    { "type": "mixed", "tag": "sticky-c-in", "listen": "0.0.0.0", "listen_port": 7953 }
  ],
  "outbounds": [
    {
      "type": "http",
      "tag": "sticky-a-out",
      "server": "<PROXY_HOST>",
      "server_port": 3000,
      "username": "<ACCOUNT>-region-US-sid-<SESSION_A>-t-10",
      "password": "<PROXY_PASS>"
    },
    {
      "type": "http",
      "tag": "sticky-b-out",
      "server": "<PROXY_HOST>",
      "server_port": 3000,
      "username": "<ACCOUNT>-region-US-sid-<SESSION_B>-t-10",
      "password": "<PROXY_PASS>"
    },
    {
      "type": "http",
      "tag": "sticky-c-out",
      "server": "<PROXY_HOST>",
      "server_port": 3000,
      "username": "<ACCOUNT>-region-US-sid-<SESSION_C>-t-10",
      "password": "<PROXY_PASS>"
    }
  ],
  "route": {
    "rules": [
      { "inbound": ["sticky-a-in"], "outbound": "sticky-a-out" },
      { "inbound": ["sticky-b-in"], "outbound": "sticky-b-out" },
      { "inbound": ["sticky-c-in"], "outbound": "sticky-c-out" }
    ]
  }
}
```

同一个 Compose 网络中的示例：

```yaml
services:
  cpa:
    image: <CPA_IMAGE>:<CPA_TAG>
    restart: unless-stopped
    ports:
      - "8317:8317"
    volumes:
      - ./config.yaml:/CLIProxyAPI/config.yaml:ro
      - ./auths:/root/.cli-proxy-api
      - ./plugins:/CLIProxyAPI/plugins:ro
      - ./plugin-data/egress-guard:/CLIProxyAPI/plugin-data/egress-guard
    depends_on:
      - egress-proxy

  egress-proxy:
    image: ghcr.io/sagernet/sing-box:<PINNED_VERSION>
    restart: unless-stopped
    volumes:
      - ./sing-box.json:/etc/sing-box/config.json:ro
```

此时 CPA 插件中的节点 URL 应填写：

```text
http://egress-proxy:7951
http://egress-proxy:7952
http://egress-proxy:7953
```

不要填写 `127.0.0.1`：在普通 Docker bridge 网络中，CPA 容器内的 `127.0.0.1` 指向 CPA 容器自身，不是 `egress-proxy`。

如果 CPA 和代理侧车都使用 `network_mode: host`，才可填写：

```text
http://127.0.0.1:7951
http://127.0.0.1:7952
http://127.0.0.1:7953
```

### 3.3 固定代理与“代理池模式”

UI 中的“固定代理/代理池”在 v1.0.4 主要是节点元数据：

- 固定住宅/静态 ISP：关闭“代理池模式”；
- 上游 URL 每次建立新连接就会轮换出口：可开启“代理池模式”作为标记；
- 用户名中包含 sticky session 时，仍建议把每个 session 建成独立节点。

当前纯 CPA 插件不会因为开启“代理池模式”就自动改用户名、调用换 IP API 或创建新会话。是否真的换 IP，由上游代理或本地侧车决定，最终必须以连通检测返回的 `exit_ip` 为准。

## 4. 构建和安装插件

优先在 CPA 管理中心的插件商店搜索 `grok2api-egress` 或 **Grok Egress Guard**。商店安装会自动选择 GitHub Release 中与主机匹配的 Linux amd64/arm64 包并校验 SHA256。商店尚未收录或需要本地调试时，再使用下面的源码构建流程。

要求：Go 1.22+、CGO、C 编译器，构建环境的架构和 libc 必须与 CPA 运行环境兼容。

```bash
cd cpa-plugin/go
go test ./...
go build -buildmode=c-shared -trimpath -o /tmp/grok2api-egress.so .
```

把 `.so` 安装到 CPA 的插件目录，不要把构建产物提交到仓库：

```bash
install -m 0755 /tmp/grok2api-egress.so <CPA_PLUGIN_DIR>/grok2api-egress.so
mkdir -p <CPA_PLUGIN_DATA_DIR>/egress-guard
chmod 700 <CPA_PLUGIN_DATA_DIR>/egress-guard
```

插件配置只需要状态文件路径：

```yaml
state_file: /CLIProxyAPI/plugin-data/egress-guard/state.json
```

确保容器中该目录可写，并在升级前备份状态文件：

```bash
cp <CPA_PLUGIN_DATA_DIR>/egress-guard/state.json \
  <CPA_PLUGIN_DATA_DIR>/egress-guard/state.json.backup
```

重启 CPA 后，在管理台打开“出口守护”。如果菜单不存在，先查 CPA 日志中的插件 ABI、架构或 libc 错误。

## 5. 添加节点和导入账号

### 5.1 在 UI 添加节点

对每个独立出口执行：

1. 点击“添加节点”；
2. 名称填写不含秘密的标识，例如 `US-A`；
3. 代理 URL 填直连 URL 或侧车 URL；
4. 设为启用；
5. `account_capacity` 填该出口的目标账号量；
6. 保存后立即执行“连通”；
7. 记录显示的 `exit_ip`，确认不同节点不是同一个出口；
8. 使用少量账号执行一次“质量”测试。

代理 URL 属于敏感数据。`state.json` 中会保存完整 URL，状态目录必须使用最小权限，不应通过 Web、备份分享或 Issue 附件公开。

### 5.2 批量添加节点

点击“批量添加”，每行可只填代理 URL，也可完整填写：

```text
socks5h://user:pass@host:port
US-A | http://user:pass@host:port | 80 | fixed
US-Pool-A | socks5h://user:pass@host:port | 120 | pool
```

字段依次为 `名称 | 代理 URL | 账号容量 | 类型`，类型只能是 `fixed` 或 `pool`。空行以及 `#`、`//` 开头的注释会忽略，单次最多 500 个。只填 URL 时由服务端生成 `Node 001` 一类名称。导入是原子操作：任一行非法时整批拒绝；成功响应和后续列表只返回 `hasProxy`，不会回显提交过的代理 URL。导入后仍需逐节点执行连通检测并核对 `exit_ip`。

### 5.3 准备 CPA xAI auth

插件只处理 CPA Host API 能列出的 xAI auth。auth JSON 至少应由 CPA 正常识别，并包含可用的 `access_token`；插件会写入或修改：

```json
{
  "type": "xai",
  "proxy_url": "http://egress-proxy:7951",
  "disabled": false
}
```

不要手工用上面的不完整 JSON 覆盖真实 auth 文件。它只展示插件关心的字段。

如果从 Grok2API 一次性迁移，先 dry-run：

```bash
export GROK2API_BASE_URL='http://127.0.0.1:8181'
export GROK2API_ADMIN_USERNAME='<ADMIN_USER>'
export GROK2API_ADMIN_PASSWORD='<ADMIN_PASSWORD>'
export CPA_AUTH_DIR='<CPA_AUTH_DIR>'

python3 cpa-plugin/import_from_g2a.py \
  --limit 100 \
  --channels 'http://egress-proxy:7951,http://egress-proxy:7952,http://egress-proxy:7953' \
  --dry-run
```

确认列表后去掉 `--dry-run`。默认脚本会在导入后禁用 Grok2API 源账号，避免同一个 refresh token 被两端同时刷新。只有明确理解 token 互踢风险时才使用 `--skip-disable`。

### 5.4 重平衡

节点和账号都准备好后，点击“重平衡账号”。插件会：

1. 读取所有 CPA xAI auth；
2. 跳过已禁用账号的重新分配；
3. 只选启用、未被守护隔离且有代理 URL 的节点；
4. 按容量感知轮询写入每个账号的 `proxy_url`；
5. 刷新节点绑定数量。

重平衡是写操作。首次生产执行前应备份 auth 目录。节点恢复后不会自动把已经迁走的账号搬回来，需要人工确认该出口稳定后再点一次“重平衡账号”。

## 6. 守护策略如何配置

推荐从默认 hybrid 配置开始：

```yaml
mode: hybrid
active_interval_seconds: 1800
passive_poll_seconds: 5
quarantine_seconds: 120
soft_tps: 500
hard_tps: 1000
consecutive_soft: 2
consecutive_errors: 2
min_healthy_nodes: 1
model: grok-4.5
disable_auth_on_hard: true
max_output_tokens: 384
```

这些字段通过管理 UI 保存到 `state.json`，不是 CPA 主配置中的插件 YAML。字段含义：

| 字段 | 说明 | 调整建议 |
|---|---|---|
| `mode` | `passive`、`active`、`hybrid` | 生产推荐 `hybrid` |
| `active_interval_seconds` | 健康节点主动质量探测间隔 | 默认 1800 秒，流量敏感可加长 |
| `passive_poll_seconds` | 策略保留字段 | 当前 Usage 由 CPA 事件直接推送，不要把它理解成请求日志扫描间隔 |
| `quarantine_seconds` | 隔离后等待自动复测的时间 | 已能强制换 IP 时 120 秒可作为起点 |
| `soft_tps` | 可疑速度阈值 | 连续命中才隔离，先按实测分布调 |
| `hard_tps` | 硬阈值 | 命中立即隔离；误报代价高时适当上调 |
| `consecutive_soft` | soft 连续次数 | 默认 2，降低误杀 |
| `consecutive_errors` | 探测错误连续次数 | 默认 2，避免瞬断直接隔离 |
| `min_healthy_nodes` | 隔离后至少保留的健康节点数 | 3 节点通常设 1；需要双出口冗余可设 2 |
| `model` | 主动探测模型 | 必须是 CPA/xAI auth 实际可用模型 |
| `disable_auth_on_hard` | 迁移失败时是否禁用原节点账号 | 建议开启，防止坏出口继续承载请求 |
| `max_output_tokens` | 主动探测最大输出 | 默认 384，太小不利于稳定计算 TPS |

模式选择：

- `passive`：没有额外主动探测流量，但无普通请求时无法判断恢复质量；
- `active`：定时检测稳定，但异常只能等到主动周期发现；
- `hybrid`：普通请求实时发现异常，30 分钟主动兜底，推荐使用。

默认后台 worker 每 30 秒扫描一次，所以“隔离 120 秒”表示到期后的下一个扫描周期触发复测，不保证精确到秒。

## 7. 隔离、迁号和恢复状态机

```text
healthy
  | hard 一次 / soft 连续 / error 连续
  v
quarantined
  |-- 账号迁到其他健康节点
  |-- 无健康目标时禁用原地账号
  |-- 最低健康节点不足时抑制隔离
  v
等待 quarantine_seconds
  |
  v
真实模型主动复测
  |-- healthy -> 恢复节点
  `-- soft/hard/error -> 保持隔离并继续等待
```

几个关键行为：

- 被动输出少于 32 token 的极短回复不会触发 hard 隔离；
- 小于 200 ms 的生成窗口不会用虚高 TPS 直接判 hard；
- hard 达阈值会立即隔离，soft 和 error 需要连续命中；
- 如果隔离会让其他健康节点少于 `min_healthy_nodes`，动作会显示为 `suppressed`；
- 质量检测可轮试同一通道的多个账号，单个过期/401 不应立即代表出口坏；
- 恢复只代表节点重新可用，不会自动把已迁账号迁回。

## 8. 隔离后强制住宅 IP 轮换并复测

这是动态住宅场景的标准操作顺序：

1. **确认节点仍处于隔离。** 不要先手工启用节点或修改 `state.json`。
2. **记下旧 `exit_ip`。** 从节点详情或最近一次连通测试读取。
3. **在插件外换 IP。** 选择一种方式：
   - 修改该节点对应的 sticky session ID；
   - 调用代理商提供的可信换 IP API；
   - 修改本地代理侧车的上游并热重载；
   - 静态代理则直接替换为新的代理 URL。
4. **只改目标节点。** 每个本地端口应对应独立配置，不能重载时顺带覆盖其他健康节点。
5. **执行连通检测。** 必须成功，并确认 `exit_ip` 与旧值不同。仅看到“连接成功”不等于已换 IP。
6. **执行真实模型质量检测。** UI 允许对隔离节点手动执行“质量”。
7. **只接受 `healthy`。** healthy 会提前解除守护隔离；soft、hard、error 都继续隔离。
8. **观察小流量。** 先放少量请求检查 401、首字延迟、Token/s 和事件。
9. **重新重平衡。** 确认稳定后点击“重平衡账号”，让迁出的账号逐步回到恢复节点。

不要使用“连通成功后直接恢复”的捷径。连通检测只能证明代理能访问外网，不能证明该出口的真实模型质量正常。

### 8.1 sticky 会话轮换示例

旧配置：

```text
<ACCOUNT>-region-US-sid-<OLD_SESSION>-t-10
```

新配置：

```text
<ACCOUNT>-region-US-sid-<NEW_RANDOM_SESSION>-t-10
```

修改侧车配置并用对应工具校验和重载：

```bash
sing-box check -c /etc/sing-box/config.json
docker compose restart egress-proxy
```

如果侧车不支持按单个 outbound 热重载，重启可能短暂影响全部本地端口。生产应拆成“一节点一侧车”，或使用支持原子重载的管理层，以缩小影响范围。

### 8.2 静态住宅代理

静态代理没有可轮换 IP 时：

- 不要反复点击恢复；
- 保持节点隔离，等待代理商处理；
- 或编辑节点，替换成新的代理 URL；
- 替换后仍需先连通检测，再真实模型质量检测，最后重平衡。

## 9. API 和自动化示例

优先使用管理 UI。需要自动化时，所有内部操作都通过 CPA 已鉴权的 management 入口转发：

```bash
export CPA_MANAGEMENT_BASE_URL='http://127.0.0.1:8317'
export CPA_MANAGEMENT_KEY='<CPA_MANAGEMENT_KEY>'

curl --fail --silent --show-error \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  -H 'Content-Type: application/json' \
  -H 'X-Grok2API-Egress-UI: 1' \
  -d '{"method":"GET","path":"/status"}' \
  "${CPA_MANAGEMENT_BASE_URL}/v0/management/grok2api-egress/api"
```

创建节点：

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  -H 'Content-Type: application/json' \
  -H 'X-Grok2API-Egress-UI: 1' \
  -d '{
    "method":"POST",
    "path":"/nodes",
    "body":{
      "name":"US-A",
      "proxy_url":"http://egress-proxy:7951",
      "enabled":true,
      "proxy_pool":false,
      "account_capacity":100
    }
  }' \
  "${CPA_MANAGEMENT_BASE_URL}/v0/management/grok2api-egress/api"
```

连通检测、质量检测和重平衡：

```json
{"method":"POST","path":"/nodes/<NODE_ID>/test"}
{"method":"POST","path":"/nodes/<NODE_ID>/quality-test"}
{"method":"POST","path":"/nodes/rebalance"}
```

管理密钥只从环境变量或 secret manager 注入，不要写进脚本、Compose、截图或 shell history。对 URL 中包含密码的代理，同样优先通过本机私密配置生成，不要提交明文示例。

## 10. 上线检查清单

- [ ] 至少 3 个节点已添加并启用；
- [ ] 每个节点连通测试成功；
- [ ] 每个 sticky 节点的 `exit_ip` 符合预期，故障域确实独立；
- [ ] auth 目录和 `state.json` 已备份；
- [ ] 先导入 50-100 账号/出口的小批量；
- [ ] 重平衡后的绑定数没有超过计划容量；
- [ ] 每个节点至少执行一次真实模型质量检测；
- [ ] 策略为 `hybrid`，模型名可用；
- [ ] 软硬阈值根据实际正常请求分布核对过；
- [ ] 隔离一个测试节点后，账号能迁到其他健康节点；
- [ ] 已验证换 sticky 后 `exit_ip` 确实变化；
- [ ] 新 IP 未经真实质量检测不会被投入使用；
- [ ] 插件数据目录可写且未公开；
- [ ] `.so`、auth、状态和日志均未进入 Git。

## 11. 常见问题

### UI 没有“出口守护”

检查 CPA 是否支持 plugin ABI v7、`.so` 架构/libc 是否匹配、插件目录是否正确，以及 CPA 日志是否有注册错误。

### 节点连通失败

先从 CPA 所在网络执行 `curl --proxy`。Docker bridge 下不要把宿主机或另一容器错误写成 `127.0.0.1`；检查代理协议、认证格式、DNS、TLS 和链式中转。

### 质量测试 401

401 通常是 auth 的 access token、过期时间、客户端头或上游权限问题，不等于代理降智。确认 CPA 中存在可用 xAI auth，模型名正确；插件会在同一通道最多轮试多个候选账号。

### 显示“没有可用的 CPA xAI 账号”

确认 auth 被 CPA Host API 识别为 xAI，存在 `access_token`，未全部 disabled，且至少有一个账号可用于探测。

### 重平衡后容量被超过

这是当前实现边界：容量是调度提示，不是硬上限；所有节点满时仍会继续分配，隔离迁号也不强制容量。减少账号、增加节点或提高已经压测过的安全容量。

### 所有节点都被隔离或隔离被抑制

检查 `min_healthy_nodes`。插件会防止隔离动作把健康节点降到下限；被抑制不代表异常消失，应尽快增加健康出口并逐个复测。

### 隔离到期后仍未恢复

自动复测最多还会受 30 秒 worker 扫描周期影响。若真实模型检测返回 soft、hard 或 error，节点会保持隔离。先处理代理或账号问题，不要直接改状态文件。

### 节点恢复后账号数量还是 0

这是正常行为：隔离时账号已经迁出，恢复不会自动迁回。确认新出口稳定后手工执行“重平衡账号”。

### UI 状态看起来滞后

页面数据每 15 秒自动刷新，但“刷新显示”只读取当前快照，不会立即发起真实模型检测。节点状态的更新时间取决于策略：`passive` 随下一次映射到该节点的普通请求更新，`active` 按主动检测间隔更新，`hybrid` 取两者中先发生的一次；默认主动间隔是 1800 秒。节点表会显示预计的下一次主动检测或“随下一次请求更新”。需要立即确认时，使用节点行内的“质量”，不要为了让 UI 看起来更实时而盲目缩短主动间隔，否则会增加模型请求和住宅代理流量。

auth 到代理的映射仍有短缓存，后台隔离复测最多受 worker 扫描周期影响。刷新后仍异常时查看最近事件与 CPA 日志。不要在 CPA 运行中直接编辑 `state.json`。

### 同时出现“检测成功”和“检测失败”

连通检测与真实模型质量检测是两条不同链路：代理能访问 `api.ipify.org` 只代表网络通，模型请求仍可能因 token、权限、限流或质量分类失败。以事件的检测类型、HTTP 状态和时间为准，不要只看一条 toast。

## 12. 给 AI 工具的推荐任务提示词

```text
你正在部署 grok2api-egress-enhancements/cpa-plugin v1.0.4。

先阅读：
1. cpa-plugin/README.md
2. cpa-plugin/AI_USAGE_GUIDE.md
3. cpa-plugin/OPEN_SOURCE_CHECKLIST.md

约束：
- 这是纯 CPA 插件，不得连接或依赖 Grok2API 运行时。
- 不得读取、打印、提交真实 API key、代理凭据、xAI token、auth JSON、state.json 或日志。
- 不得提交 *.so 和 CGO 生成的 *.h。
- 先只读检查 CPA 版本、架构、libc、插件目录、auth 目录和 Docker 网络。
- 每个出口使用独立 sticky session；先连通检测并确认出口 IP 不同。
- 初始按 50-100 账号/出口部署，至少 3 个节点，并预留 20%-30% 故障迁移容量。
- account_capacity 不是硬上限，不得声称插件能阻止超配。
- proxy_pool 是类型标记，不得声称插件会自动调用代理商换 IP。
- 隔离后先迁号；强制换 sticky 或替换代理必须在插件外完成。
- 换 IP 后先确认 exit_ip 变化，再执行真实模型质量检测；只有 healthy 才可恢复。
- 节点恢复后确认稳定，再执行 rebalance 把账号迁回。
- 所有配置示例必须脱敏，任何破坏性操作前先备份。

完成后输出：实际拓扑、节点数量、计划容量、故障后容量计算、守护策略、测试结果、未解决风险和回滚方法。
```

## 13. 回滚

升级或策略调整出现问题时：

1. 停止 CPA，避免状态和 auth 继续变化；
2. 保存当前日志到仓库外的私密目录；
3. 恢复升级前的 `.so`；
4. 必要时恢复备份的 `state.json` 和 auth 目录；
5. 启动 CPA；
6. 先检查账号 `proxy_url` 和节点连通性，再恢复业务流量。

不要在 CPA 正在写 auth 或状态时直接覆盖文件。生产回滚必须保留升级前 `.so`、状态和 auth 三者的同一时间点备份。
