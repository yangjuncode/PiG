// Package ai provides provider abstractions, model definitions, and
// auth storage for pig.
//
// The model catalogs are generated from the exact published Pi package pinned
// by extensions/sdk-ts. Run `make model-catalogs` after changing the upstream
// pin.
//
// 中文导读：ai 是 provider 抽象层——统一的消息/事件流类型（types.go、
// events.go）、模型目录与运行时（models_*、registry.go）、认证存储与
// OAuth（auth_*、oauth_*）、各 provider 的 API 实现（anthropic.go、
// openai.go 等），以及把 JS 异步语义精确搬进 Go 的基础设施工具
// （js_promise、js_async_generator、continuation、node_readable 等）。
// 模型目录由上游 pin 的 Pi 包生成，改动 pin 后运行
// `make model-catalogs`。
package ai

//go:generate ../automation/gen/generate-model-catalogs.sh
