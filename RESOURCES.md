# Resources

学这个仓库有个特殊优势：**它自带一份高密度的权威文档体系**（因为移植纪律要求一切行为有出处）。优先用仓库内的文档，外部资源做补充。

## 仓库内 · 一手权威

| 资源 | 类型 | 用在什么时候 |
|---|---|---|
| `docs/pig-architecture.md` | 架构总图 | 第一课的主读材料；任何"这在哪一层"的问题先查它 |
| `docs/README.md` | 文档路由表 | "我要改/理解 X" → 查表得到该读的文档，不要泛读 |
| `docs/piglet-resource-ontology.md` | 术语本体 | Resource/Package/Piglet/Piglet Binary 等名词的精确定义 |
| `AGENTS.md`（根 + 各包目录下） | 契约与失效模式 | 每个包读代码前先看它的 AGENTS.md：契约、失效模式表、证据要求 |
| `docs/typescript-to-go-porting.md` | 移植方法论 | 理解"为什么 Go 代码长这样"：TS 语义到 Go 的映射规则 |
| `docs/parity/PORT_MAP.md` | 文件对照表 | 上游 TS 文件 ↔ Go 文件的逐个映射，定点深入时的导航 |
| `docs/parity/DIVERGENCES.md` | 有意差异清单 | 读到"和上游不一样"的地方先查编号 |
| `docs/extension-authoring.md` / `docs/extension-api-parity.md` | 扩展系统权威 | 深入扩展宿主与三套 SDK 时读 |
| `coding/extension/host/subprocess/protocol.go` | wire 协议定义 | 扩展子进程协议的唯一权威 |
| `internal/pigdocs/content/` | 嵌入式用户手册 | 用户视角的功能说明（随二进制分发） |

## 仓库内 · 对照样本

| 资源 | 类型 | 用在什么时候 |
|---|---|---|
| `.upstream/current/` | 上游 TS 镜像（pin 1.0.3） | "Pi 原本怎么做的"——Go 代码的语义出处 |
| `test/parity/scenarios/<family>/*.toml` | 行为场景定义 | 想确认某功能"可观察行为是什么"时读 TOML |
| `test/parity/coverage.md` | 覆盖报告 | 判断某块代码的 parity 成熟度 |
| `coding/upstream.go` | 版本锚点 | `coding.UpstreamVersion` = 当前对齐的上游版本 |

## 外部 · 上游与生态

| 资源 | 类型 | 用在什么时候 |
|---|---|---|
| [github.com/earendil-works/pi](https://github.com/earendil-works/pi) | 上游源码 | 比 `.upstream/current` 更新；看演进方向 |
| [pi.dev/docs](https://pi.dev/docs/latest) | 上游官方文档 | Pi 的用户态行为定义 |
| [go.dev/doc](https://go.dev/doc/) | Go 官方 | `go doc` 浏览包接口是定点深入的主力工具 |
| Anthropic/OpenAI 等 provider API 文档 | API 权威 | 深入 `ai/` 各 provider 实现时对照 wire 格式 |

## 使用纪律

1. 读代码顺序：先该包的 `AGENTS.md`（契约）→ 上游对应文件（语义出处）→ Go 实现。
2. 任何"为什么这样写"先查 PORT_MAP / DIVERGENCES，再下结论。
3. 不相信记忆：每个细节都用工具验证后再写进课程。
