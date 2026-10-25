//go:build requestvalidity

// 中文导读：请求有效性检查——enabled_tag。
package requestvalidity

// taggedBuild enables the checker in a binary built with `-tags requestvalidity`, so scenario, conformance and oracle suites that run the pig binary check every provider request.
const taggedBuild = true
