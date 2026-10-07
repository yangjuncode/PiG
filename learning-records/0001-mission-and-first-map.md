# 0001 · 任务确立与第一张架构地图

日期：2026-10-07

## 用户访谈结论（ZPD 锚点）

- **任务**：写自己的 coding agent，以 PiG 代码为参考蓝本。
- **基础**：Go 熟练。coding agent 内部机制熟练度未确认（待第 1 课反馈校准）。
- **偏好**：中文；先全局地图、再定点深入；首课选整体导览。

## 本次产出

- `MISSION.md`：任务定义与成功标准。
- `RESOURCES.md`：资源清单——仓库内文档优先（pig-architecture.md / AGENTS.md / PORT_MAP / upstream 镜像），外部资源补充。
- `assets/lesson.css` + `assets/quiz.js`：可复用的课程样式与测验组件。
- `lessons/0001-architecture-map.html`：架构地图 + 一次 prompt 的旅程 + 5 题测验 + 动手定位任务。
- `reference/glossary.html`：术语表（移植纪律 / 生态本体 / 引擎内部三类词汇）。

## 关键教学决策

- **取舍镜头是第一课的核心知识点**：对一个"要抄架构"的学习者，最重要的不是功能清单，而是"哪些是骨架、哪些是移植地基"。`internal/nodepath` 这类 TS 语义 shim 明确标记为"可跳过"，防止用户误把时间花在移植专用层。
- **旅程给了真实函数名**（`Send → runLoop → runTurn → streamAssistantResponse → handleAgentEvent`），让地图可以被工具验证，而不是纯抽象描述。
- **下一课候选**：agent loop 精读（agent_loop.go 的 loop/runTurn/事件契约）。备选：ai/ Provider 接缝、扩展宿主 wire 协议。等用户反馈后定。

## 观察到的用户状态

- 主动选了"整体导览"而非某个子系统 → 处于探索早期，先建立可导航的地图比深入细节收益大。

## 待校准

- 测验难度是否合适（Q5 需要真的开文件，是最难的一题）。
- 每课时长是否合适（本课目标 ~15 分钟）。
