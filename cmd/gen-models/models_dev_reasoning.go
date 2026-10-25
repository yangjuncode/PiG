// 中文导读：模型目录生成器——models_dev_reasoning。
package main

import "github.com/MichaelKinsy/PiG/internal/modelgen"

type modelsDevReasoningOption = modelgen.ModelsDevReasoningOption

func getEffortThinkingLevelMap(options []modelsDevReasoningOption) map[string]*string {
	return modelgen.GetEffortThinkingLevelMap(options)
}
