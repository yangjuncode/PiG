package compaction

import (
	"encoding/json"
	"slices"

	"github.com/MichaelKinsy/PiG/internal/codingagent"
)

func isProjectedTurnStart(entry codingagent.ProjectedSessionEntry) bool {
	if entry.SourceEntry.Base().Type == "compaction" {
		return false
	}
	return slices.ContainsFunc(entry.Messages, isTurnStartMessage)
}

func findProjectedTurnStartIndex(entries []codingagent.ProjectedSessionEntry, entryIndex, startIndex int) int {
	for i := entryIndex; i >= startIndex; i-- {
		if isProjectedTurnStart(entries[i]) {
			return i
		}
	}
	return -1
}

// isIntrinsicallyVisible reports whether an entry contributes context before
// context edits apply. Context edits themselves never do.
func isIntrinsicallyVisible(entry codingagent.ProjectedSessionEntry) bool {
	return entry.SourceEntry.Base().Type != "context_edit" && len(codingagent.SessionEntryToContextMessages(entry.SourceEntry)) > 0
}

func isOmitted(entry codingagent.ProjectedSessionEntry) bool {
	return isIntrinsicallyVisible(entry) && len(entry.Messages) == 0
}

func isOmittedAssistantAttempt(entry codingagent.ProjectedSessionEntry) bool {
	if entry.SourceEntry.Base().Type != "message" || !isOmitted(entry) {
		return false
	}
	message, ok := codingagent.AsMessage(entry.SourceEntry)
	return ok && message.Message.Assistant != nil
}

// isRecoveryOmissionSuffix reports whether the entries after a cut form a
// closed recovery suffix: an omitted assistant attempt, its omission edits, and
// context-invisible metadata only. A replacement edit of any entry that is not
// itself omitted in the suffix blocks it, because the omitted attempt answered
// the pre-edit input.
func isRecoveryOmissionSuffix(suffix []codingagent.ProjectedSessionEntry) bool {
	// 三步验收"恢复残骸"：
	//  1. 收集后缀里所有被 edit 隐藏的条目 ID；
	//  2. 后缀里若有指向"未被隐藏条目"的替换 edit → 不算纯
	//     恢复残骸（那条 edit 还改着活条目）；
	//  3. 必须真的含一条被隐藏的 assistant 尝试，且后缀不含
	//     compaction、不含仍可见的条目——全过才算封闭恢复尾部。
	omittedIDs := make(map[string]struct{})
	for _, entry := range suffix {
		if isOmitted(entry) {
			omittedIDs[entry.SourceEntry.Base().ID] = struct{}{}
		}
	}
	for _, entry := range suffix {
		if entry.SourceEntry.Base().Type != "context_edit" {
			continue
		}
		var edit codingagent.ContextEditEntry
		if json.Unmarshal(entry.SourceEntry.Raw(), &edit) != nil || edit.Replacement == nil {
			continue
		}
		if _, omitted := omittedIDs[edit.TargetID]; !omitted {
			return false
		}
	}
	if !slices.ContainsFunc(suffix, isOmittedAssistantAttempt) {
		return false
	}
	for _, entry := range suffix {
		if entry.SourceEntry.Base().Type == "compaction" || (isIntrinsicallyVisible(entry) && !isOmitted(entry)) {
			return false
		}
	}
	return true
}

// findProjectedCutPoint finds the cut point over projected entries
// (compaction.ts findProjectedCutPoint). Only projected messages count, so
// omitted entries neither become cut points nor consume the keep budget.
//
// 与 FindCutPoint（裸条目版）的区别：这里在投影结果上选点，context_edit
// 删除过的条目既不占预算也不能当切口。多的一步是"恢复尾部"检查：
// 如果切口之后的尾巴恰好是一次溢出恢复留下的残骸（被删除的
// assistant 尝试 + 它的删除编辑 + 不可见元数据），切口再进一格
// 越过它——这些条目已不在上下文里，留着只会污染摘要输入。
func findProjectedCutPoint(entries []codingagent.ProjectedSessionEntry, startIndex, endIndex, keepRecentTokens int) CutPointResult {
	// 第一遍：收集所有合法切点（非 compaction 且含
	// isCutPointMessage——即用户消息这类"一条消息可自成
	// 切割边界"的条目）。
	var cutPoints []int
	for i := startIndex; i < endIndex; i++ {
		if entries[i].SourceEntry.Base().Type != "compaction" && slices.ContainsFunc(entries[i].Messages, isCutPointMessage) {
			cutPoints = append(cutPoints, i)
		}
	}
	if len(cutPoints) == 0 {
		return CutPointResult{FirstKeptEntryIndex: startIndex, TurnStartIndex: -1}
	}

	// 第二遍：从尾部向头累计可见 token，到达 keepRecentTokens
	// 预算时取"该位置之后最近的合法切点"——保留预算内的尾部，
	// 把预算耗尽处对齐到最近的天然边界。
	accumulated := 0
	exceededBudget := false
	cutIndex := cutPoints[0]
	for i := endIndex - 1; i >= startIndex; i-- {
		tokens := estimateMessagesTokens(entries[i].Messages)
		if tokens == 0 {
			continue
		}
		accumulated += tokens
		if accumulated >= keepRecentTokens {
			exceededBudget = true
			cutIndex = closestCutPointAtOrAfter(cutPoints, i)
			break
		}
	}

	// A recovery attempt and its omission edits are context-invisible after the
	// last visible input. Advance only past such a closed suffix; arbitrary
	// metadata must not move the cut past unsent input.
	if exceededBudget && isRecoveryOmissionSuffix(entries[cutIndex+1:endIndex]) {
		cutIndex++
	}

	// 向前收缩：切口前若是无投影消息的条目（纯元数据/被全部
	// 隐藏），把切口往前让——避免摘要尾部刚切完就留给一串
	// 无内容条目做"新保留区"的开场。
	for cutIndex > startIndex {
		previous := entries[cutIndex-1]
		if previous.SourceEntry.Base().Type == "compaction" || len(previous.Messages) > 0 {
			break
		}
		cutIndex--
	}
	// 切口语义收尾：切口恰在 turn 开头 → 干净切割；在 turn
	// 中间 → 向前找该 turn 的起点记为 turnStartIndex，
	// IsSplitTurn=true 提示上层这次压缩劈开了一个回合。
	startsTurn := isProjectedTurnStart(entries[cutIndex])
	turnStartIndex := -1
	if !startsTurn {
		turnStartIndex = findProjectedTurnStartIndex(entries, cutIndex, startIndex)
	}
	return CutPointResult{
		FirstKeptEntryIndex: cutIndex,
		TurnStartIndex:      turnStartIndex,
		IsSplitTurn:         !startsTurn && turnStartIndex != -1,
	}
}
