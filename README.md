# Grok2API & CPA 出口质量守护

这是一个非官方增强分发仓库：为 [chenyme/grok2api](https://github.com/chenyme/grok2api) 提供固定代理快速恢复和出口质量守护补丁，同时提供零 Grok2API 运行时依赖的纯 CPA 原生插件。仓库不复制上游完整源码，只发布可审计的 Git patch、功能说明、AI 指南和 CPA 插件源码。

当前补丁基于：

- 上游版本：`v3.0.11`
- 上游提交：`090104504b403d65675a01dab9c92b3a235ee832`
- 补丁提交：见 `MANIFEST.json` 的 `patch_commit`（严格隔离与自动换 IP 完整版）
- 上游 Draft PR：[chenyme/grok2api#837](https://github.com/chenyme/grok2api/pull/837)
- 可运行 Fork：[lij768423-svg/grok2api](https://github.com/lij768423-svg/grok2api/tree/agent/egress-resilience-quality-guard)

## 包含功能

### 固定代理快速恢复

- 请求提交前发生连接拒绝、reset、timeout 或 EOF 时，固定节点先进入冷却，再立即异步复测。
- 同一节点的并发故障只启动一个探针。
- 后续绑定请求最多等待 5 秒；复测健康后重新读取持久化状态并继续，不健康则保留冷却。
- 请求取消立即停止等待，不会取消共享探针。
- 不重放已经提交的生成请求，也不把认证、额度或限流错误当作代理故障。
- 官方已有的代理池模式继续按新隧道处理，单个旋转出口失败不会冷却整个池。

### 出口质量守护

- 被动审计按 grok2api 面板同口径计算 `输出 Token / (总耗时 - 首字耗时)`，其中输出 Token 包含推理 Token。
- **被动硬阈值立即隔离节点**；软阈值触发固定 Prompt 主动复测，连续命中后才隔离。
- 主动软/硬阈值、连续探测错误、最低健康节点、隔离与自动恢复保护。
- 严格模式下先摘流再确认；短窗口流式缓冲突增会先在原 IP 复测，确认异常后才换 IP。
- 支持受信任的节点级换 IP Webhook，以及 1024Proxy `sid-...-t-...` 粘性会话轮换器。
- 新 IP 只执行一次真实模型质量检测；正常立即恢复，异常或不确定则保持隔离。
- 账号调度失败与代理故障分开处理：暂无可调度账号时延后复测，不累计代理错误、不浪费流量换 IP。
- 目标节点绑定账号不可调度时，管理员质量探针会借用任意健康 Build 账号，但实际请求仍强制走被测节点；普通流量不受影响。
- 整个账号池不可用时按独立长退避延后检测并抑制重复日志，节点仍保持隔离。
- 管理端质量守护页面、手动诊断、策略热加载和累计统计。
- 手动检测与节点操作使用单条可更新提示；隔离或轮换中的节点禁止并发手动检测。
- 在节点质量表中直接添加、编辑、删除、启用、停用和刷新 Build 代理节点。
- 支持单选、全选、批量启用、批量停用和批量删除，并为删除操作提供确认。
- `QUALITY_GUARD_NODE_IDS` 留空时自动发现所有已启用的代理 Build 节点；状态文件同时发布已解析节点，兼容旧版管理页面。
- 独立 Python sidecar、Docker Compose、systemd、安全说明和中英文文档。

### CPA 原生出口守护插件

`cpa-plugin/` 现为 **v1.0.5 纯 CPA 原生插件**，不依赖、不连接 Grok2API 运行时。它通过 CPA Host API 读取认证文件和 Usage 事件，把账号的 `proxy_url` 粘性绑定到出口节点，并提供节点 CRUD、逐行批量导入、批量操作、连通性/真实质量检测、隔离迁号、策略热加载、统计事件和深浅色管理 UI。v1.0.5 额外让 CPA 调度跳过隔离/冷却出口，账号或额度错误只记为 ignored，迁移会写后读回校验，并支持节点白名单化的内部换 IP Webhook。构建与部署方法见 [cpa-plugin/README.md](./cpa-plugin/README.md)，代理规划、账号容量、隔离恢复和强制住宅 IP 轮换见 [AI 部署与运维指南](./cpa-plugin/AI_USAGE_GUIDE.md)。

CPA 本身不会让模型“降智”；这个插件只是在多账号、多出口运行场景中，根据可观测质量信号做可选的出口熔断与迁移。单账号或稳定静态代理场景可以不安装。

质量守护是启发式熔断器，不是模型能力鉴定器。中间层缓冲、已有文件、长常量或缓存内容可能造成异常高瞬时 Token/s。硬阈值策略偏激进，可按链路调高 `hard_tps`；软阈值仍以固定 Prompt 复测确认。

## 直接应用

在干净的 grok2api 仓库中执行：

```sh
git fetch --tags origin
git checkout -b egress-enhancements v3.0.11
git am --3way /path/to/grok2api-egress-enhancements/patches/0001-feat-add-egress-recovery-and-quality-guard.patch
```

如果目标版本高于 `v3.0.11`，建议使用 [AI 合并指南](./docs/AI_MERGE_GUIDE.md)，让工具按功能不变量解决冲突，而不是直接覆盖新版文件。

## 验证

```sh
go test ./...
python3 -m unittest -v \
  tools/egress-quality-guard/quality_guard_test.py \
  tools/egress-quality-guard/session_rotator_test.py  # 26 tests
cd frontend
pnpm lint
pnpm build
```

生产部署前还应验证：

- 固定代理失败后只启动一个立即探针；
- 健康探针只清理 `transport error` 冷却；
- 不健康探针不会提前恢复节点；
- 动态代理池不会因单次连接失败进入全局冷却；
- 管理 API 不返回管理员密码、Client Key 密钥、代理 URL、探测 Prompt 或模型响应正文。
- 账号调度失败不会触发代理 IP 轮换，未经模型质量验证的隔离节点也不会被恢复。

## 安全与隐私

补丁不包含部署配置。不要向 AI 工具提供真实 `.env`、`config.yaml`、数据库、状态卷、代理 URL、账号凭据或生产日志。合并时只需要上游源码、本仓库补丁和测试输出。

## 相关项目

- [Grok Register + Live Panel](https://github.com/lij768423-svg/grok-register-panel)：基于 Camoufox 的 Grok 注册流程与 Web 管理面板，支持多邮箱后端、外部代理池、出口预检、ASN 黑名单、运行统计和账号补录。它是独立项目，不包含在本补丁中。

## 友情链接

- [LINUX DO](https://linux.do) — 新的理想型社区

## 许可与归属

补丁在上游 MIT 许可框架下发布。保留上游项目的 LICENSE、版权信息和提交历史。本仓库不是 grok2api 官方发行版，也不代表上游维护者认可这些改动。

English documentation: [README.en.md](./README.en.md)
