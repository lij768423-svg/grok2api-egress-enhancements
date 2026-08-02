# AI 合并指南

这份指南用于把补丁移植到高于 `v3.0.11` 的 grok2api。推荐把目标仓库放在干净分支中，只向 AI 工具提供源码、补丁和脱敏后的测试错误。

本页只描述 Grok2API 补丁移植。纯 CPA 插件的安装、代理规划、账号容量、隔离恢复和强制住宅 IP 轮换请单独阅读 [CPA 出口守护 AI 部署与运维指南](../cpa-plugin/AI_USAGE_GUIDE.md)，不要把两套运行时混为一体。

## 推荐提示词

```text
你正在把 grok2api-egress-enhancements 合并到一个更新版本的 chenyme/grok2api。

基线补丁：patches/0001-feat-add-egress-recovery-and-quality-guard.patch
设计说明：docs/FEATURES.md

要求：
1. 先阅读目标仓库当前的出口节点、代理池、请求审计、管理员路由和前端结构。
2. 使用 git am --3way 尝试应用补丁；有冲突时按语义移植，不得整文件覆盖新版实现。
3. 保留目标版本新增的数据库字段、API、路由策略、鉴权中间件和 UI 行为。
4. 固定代理快速恢复必须保持：先持久化冷却，再启动按节点合并的独立探针；绑定请求限时等待；健康后重新读取状态；不健康维持冷却；请求取消立即退出。
5. 不得重放可能已经提交上游的请求，不得把认证、额度或限流错误归类为代理传输错误。
6. 代理池单次连接失败不得修改节点级健康、失败次数或冷却。
7. 健康探针只能清理 last_error 精确等于 transport error 的冷却，不得清理 anti-bot 或管理员状态。
8. 质量守护 API 必须保持管理员鉴权，响应和日志不得包含管理员密码、Client Key 密钥、代理 URL、Prompt 或模型响应正文。
9. 主动和被动质量速度必须保持 grok2api 面板口径：outputTokens / (durationMs - firstTokenMs)，不得减去 reasoningTokens。
10. 被动 hard TPS 必须立即隔离；被动 soft TPS 仍需固定 Prompt 主动复测确认，主动 hard 立即隔离，主动 soft 连续命中后隔离。被动触发的复测错误不得直接隔离。
11. 质量守护页必须复用出口节点 API，保持代理 URL 只写，并提供单节点 CRUD、启停、刷新以及批量选择、批量启停和批量删除。
12. `QUALITY_GUARD_NODE_IDS` 为空时必须自动发现已启用代理 Build 节点，并在状态里发布已解析 ID 以兼容旧版页面；手工停用节点不得被主动探测。
13. 严格模式下可疑节点先摘流；短窗口缓冲突增必须先在原 IP 复测，确认异常后才换 IP。新 IP 只检测一次，正常立即恢复，否则保持隔离。
14. 连续主动探测错误达到阈值后才隔离。账号选择失败必须返回独立错误码，守护只延后复测，不得累计代理故障或触发换 IP。
15. 节点级换 IP 只能作用于显式允许的节点，不得覆盖其他代理。1024Proxy 粘性会话用户名应保持 `sid-...-t-...` 结构并验证出口确实变化。
16. 质量检测、节点启停和批量操作的提示不得互相堆叠；隔离或轮换中的节点不得并发手动检测。
17. 不得读取或修改真实 .env、config.yaml、数据库、状态卷或生产代理配置。
18. 完成后运行 Go 全量测试、质量守护与轮换器单测、前端 lint/build，并列出所有语义冲突和处理方式。
```

## 手工起点

```sh
git checkout -b port-egress-enhancements
git am --3way patches/0001-feat-add-egress-recovery-and-quality-guard.patch
```

如果 `git am` 停在冲突状态，让 AI 工具先运行 `git status`，逐个读取冲突文件的新版上下文和补丁对应 hunk。不要使用 `git checkout --theirs` 批量覆盖。

## 高概率冲突位置

- `backend/internal/app/application.go`：依赖注入和 HTTP server 构造。
- `backend/internal/infra/egress/manager.go`：代理选择、冷却和请求反馈。
- `backend/internal/infra/persistence/relational/egress_repository.go`：健康状态持久化。
- `backend/internal/transport/http/egress/handler.go`：管理员出口节点和质量守护 API。
- `frontend/src/shared/i18n/index.ts`：中英文资源对象。
- `frontend/src/app/*`：管理页面路由与导航。

## 验证命令

```sh
go test ./...
python3 -m unittest -v \
  tools/egress-quality-guard/quality_guard_test.py \
  tools/egress-quality-guard/session_rotator_test.py
cd frontend && pnpm lint && pnpm build
git diff --check
```
