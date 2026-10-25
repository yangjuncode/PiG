// 中文导读：CreateModelsOptions——Models 的构造参数。
//
// 对应 models.ts：把应用持有的 CredentialStore、ModelsStore 与
// AuthContext 注入模型集合——这三个都是调用方拥有的存储/上下文，
// Models 只引用不接管。
package ai

// Ports packages/ai/src/models.ts (CreateModelsOptions).

// CreateModelsOptions supplies application-owned stores and auth context to model collections.
type CreateModelsOptions struct {
	Credentials CredentialStore
	ModelsStore ModelsStore
	AuthContext *AuthContext
	// Identity is the default identity of every request; a request's Identity overrides it field by field.
	Identity *ClientIdentity
}
