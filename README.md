# grok2api 出口增强补丁

这是一个非官方补丁分发仓库，为 [chenyme/grok2api](https://github.com/chenyme/grok2api) 增加固定代理快速恢复和可选的出口质量守护。仓库不复制上游完整源码，只发布可审计的 Git patch、功能说明和 AI 合并指南。

当前补丁基于：

- 上游版本：`v3.0.11`
- 上游提交：`090104504b403d65675a01dab9c92b3a235ee832`
- 补丁提交：`690a641deb06c5c1a73983677f6b454557727113`
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
- 被动高 TPS 只触发固定 Prompt 的逐节点主动复测，普通真实请求不会因一次高 TPS 直接隔离节点。
- 主动软/硬阈值、连续错误、最低健康节点、隔离和恢复保护。
- 管理端质量守护页面、手动诊断、策略热加载和累计统计。
- 独立 Python sidecar、Docker Compose、systemd、安全说明和中英文文档。

质量守护是启发式熔断器，不是模型能力鉴定器。中间层缓冲、已有文件、长常量或缓存内容都可能造成异常高的真实请求瞬时 Token/s，因此必须以固定 Prompt 主动复测确认。

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
python3 -m unittest -v tools/egress-quality-guard/quality_guard_test.py
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

## 安全与隐私

补丁不包含部署配置。不要向 AI 工具提供真实 `.env`、`config.yaml`、数据库、状态卷、代理 URL、账号凭据或生产日志。合并时只需要上游源码、本仓库补丁和测试输出。

## 相关项目

- [Grok Register + Live Panel](https://github.com/lij768423-svg/grok-register-panel)：基于 Camoufox 的 Grok 注册流程与 Web 管理面板，支持多邮箱后端、外部代理池、出口预检、ASN 黑名单、运行统计和账号补录。它是独立项目，不包含在本补丁中。

## 友情链接

- [LINUX DO](https://linux.do) — 新的理想型社区

## 许可与归属

补丁在上游 MIT 许可框架下发布。保留上游项目的 LICENSE、版权信息和提交历史。本仓库不是 grok2api 官方发行版，也不代表上游维护者认可这些改动。

English documentation: [README.en.md](./README.en.md)
