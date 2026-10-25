// 中文导读：env API key 的登录实现（auth/helpers.ts 移植）。
//
// envAPIKeyLogin 返回一个 login 函数：从环境变量取 key 构造
// Credential，支持 ctx 取消（context.Cause）。
package ai

// Ports packages/ai/src/auth/helpers.ts

import "context"

func envAPIKeyLogin(name string) func(context.Context, AuthInteraction) (Credential, error) {
	return func(ctx context.Context, interaction AuthInteraction) (Credential, error) {
		if ctx.Err() != nil {
			return Credential{}, context.Cause(ctx)
		}
		credential, err := promptAPIKey(ctx, interaction, "Enter "+name)
		if err != nil {
			return Credential{}, err
		}
		if ctx.Err() != nil {
			return Credential{}, context.Cause(ctx)
		}
		return credential, nil
	}
}
