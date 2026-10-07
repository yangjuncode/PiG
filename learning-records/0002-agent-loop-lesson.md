# 0002 · 第 2 课：agent loop 精读

日期：2026-10-07

## 触发

用户明确选定"agent loop 精读"作为第 2 课（IDE 里已打开 agent/agent.go）。

## 深读所得（本课基于真实代码，非二手描述）

`agent/` 包的核心结构：

- **入口收敛**：Send/SendContent/SendMessages/Continue → `beginRun`（占用锁 + runContext）→ `runLoop` → `loopRun.loop()`。
- **双层循环**：内层 `for hasMoreToolCalls || len(pendingMessages)>0` 跑 turn；外层处理 follow-up 队列与 `AgentTurnContinue` 决策。退出路径三种：break / return nil / return err。
- **runTurn 三元返回**：`(completedTurn, done, error)`；error/aborted 是硬退出（跳过 FinishTurn 决策）。
- **stopReason 控制流**：stop 自然退出 / toolUse 进工具批 / aborted+error 硬退 / length 整批工具调用拒执行（截断参数危险）。
- **工具批三段式**：prepare 串行（保 start 事件序 + QueueOrderable 票据序）→ execute 全并发（wg.Go，复刻 Promise.all）→ finalize 的 end 事件按完成序、transcript 按源序。
- **emit 单出口管线**：appendMessages（仅 message_end）→ reduceEventState → listeners（同步，error→panic runFailure）→ OnEvent（可原地替换）→ persistMessage → EventCh → AfterEvent。
- **错误哲学**：provider 建流失败合成 stopReason=error 的 assistant 消息；listener/持久化错误经 panic(runFailure)→catchRunFailure→handleRunFailure 走完整失败生命周期。
- **三通道**：Steer（回合间插入）/ FollowUp（run 收尾接管）/ QueueNextTurn（贴下次 prompt）。

## 教学决策

- 课程框架从"逐行讲"改为"状态机 + 不变量"：三个不变量（单 run 占用、事件单出口、message_end 提交点）承载全部细节。
- 第 9 节"可抄清单"直接服务 mission——区分"抄 / 按需 / 三思"，其中 panic(runFailure) 和 runDeadlineContext 标为"三思"：它们是 TS→Go 翻译产物，不是通用最佳实践。这是本课对"写自己 agent"最关键的判断输出。
- Q6 设计成"必须真打开文件数退出路径"，延续第 1 课"动手验证"的风格。

## 环境事实（写课时的发现）

- `.upstream/` 镜像不在工作区，由 `make upstream-mirror` 按需拉取。课程里已注明。
- `agent/` 目录下没有专属 AGENTS.md（只有 ai/、tui/、coding/ 等有）。

## 下一课候选（由用户选）

- ai.Provider 接缝与流式协议（consumeStream 的另一半）
- 事件到 TUI 的桥接（interactive_events.go）
- 扩展钩子全景（transformContext/FinishTurn 等怎么连到扩展宿主）
