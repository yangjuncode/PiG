# NOTES

## 用户偏好

- 交流语言：中文（文档、代码注释也用中文）。
- Go 熟练，无需语言基础教学；coding agent 原理未知深浅，需确认。
- 学习方式：地图 + 定点深入。先全局后局部。
- 目标：写自己的 agent，PiG 是参考/蓝本而非要贡献的项目。

## 教学决策记录

- 用户提问一律走 agentassistant-mcp 的 `ask_question`（不用内置 ask_user_question）。
- 课程文件放 `lessons/`，可复用组件放 `assets/`，速查文档放 `reference/`，学习档案放 `learning-records/`。
- 由于教学文件与仓库代码共存于同一目录，文件名与目录都加上独立命名，避免与仓库本身文件混淆。

## 待确认 / 悬而未决

- 用户对 coding agent 运行机制（tool loop、streaming、context 管理）的熟悉程度未确认——第 1 课后的反馈可校准。
- 用户是否计划保留扩展系统/Piglet 这类产品层机制到自己的 agent，未知。
