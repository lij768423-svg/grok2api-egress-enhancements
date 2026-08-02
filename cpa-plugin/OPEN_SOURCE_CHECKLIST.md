# 公开仓发布清单

## 1. 仓库形态建议

```text
grok2api-egress-enhancements/
├── cpa-plugin/
│   ├── README.md
│   ├── OPEN_SOURCE_CHECKLIST.md
│   ├── go/
│   │   ├── *.go / page.html / tokens.css / go.mod
│   │   └── .gitignore     # 忽略 *.so *.h
│   ├── loadtest/
│   └── import_from_g2a.py
├── LICENSE               # 仓库根目录许可证
└── .github/workflows/build.yml   # 可选
```

Go module 当前：`github.com/lij768423-svg/cpa-plugin-grok2api-egress`。
插件元数据中的 GitHubRepository 已指向当前公开仓。

## 2. 必须清洗的内容

| 项 | 处理 |
|---|---|
| CPA loadtest API key | 文档用 `CPA_LOADTEST_KEY` 占位 |
| xAI refresh / access token | 禁止提交 auth JSON |
| `state.json` | 不进仓；提供 `state.example.json` |
| `*.so` 二进制 | CI 构建或 Release 附件，不进主分支（可选） |
| 日志 `monitor.jsonl` / `loadtest.stdout` | 不进仓 |
| 内网 IP、家宽出口 IP | README 实测表已抽象为通道名 |
| home-serve / Tailscale 地址 | 文档用 `http://127.0.0.1:8317` |

## 3. 建议附带的最小可运行说明

1. CPA ≥ 支持 `pluginabi` v7 的版本
2. `go build -buildmode=c-shared`
3. 挂载 `plugin-data/egress-guard`
4. 两个假节点（可都是 `http://127.0.0.1:7890` 做 UI 演示）
5. 截图：节点表、隔离倒计时、事件流（打码 token）

## 4. 版本与变更摘要（1.0.3）

- 纯 CPA 插件，无 Grok2API 运行时依赖
- 节点 CRUD + 粘性 `proxy_url` 绑定 + rebalance
- hybrid 质量守护：passive usage + active probe
- hard/soft/error → quarantine → migrate → restore
- 强制 xAI 探测头，多账号 401 重试
- auth→node 映射缓存 + 未映射 hard fallback
- 短生成窗口 / 小输出防误 hard
- 管理 UI 完整中文台
- loadtest + monitor 脚本

## 5. 已知问题（诚实写进 README / Issues）

1. **Sticky quarantine 标志**：并发路径下可能出现 `cls=healthy` 且 `quarantined=true`；运维可清 state + rebalance。需状态机单一真源。
2. **Monitor 历史 alerts**：累计 `many_5xx` / 旧 `node_quarantined` 事件会重复刷，应用 delta 而不是 lifetime。
3. **短流量 hard 敏感**：默认 `hard_tps=1000` 对超短 reply 仍可能噪；已用 minGenMs 缓解，生产建议按模型调参。
4. **CGO .so 可移植性**：必须与 CPA 同 libc/架构；建议提供 build 容器。

## 6. 发布步骤（当前公开仓）

```bash
# 1. 在现有仓库分支中替换 cpa-plugin/，不要新开仓
# 2. 本地构建自测（产物写到 /tmp，不进 git）
cd cpa-plugin/go
go test ./...
go build -buildmode=c-shared -o /tmp/grok2api-egress.so .

# 3. 检查敏感文件后提交并推送分支
git diff --check
git status --short
```

## 7. 一句话 Pitch（README / Release）

**CPA 原生多出口降智守护：粘性绑定账号到代理节点，被动+主动测 Token/s，坏出口自动隔离并迁号——不依赖 Grok2API。**
