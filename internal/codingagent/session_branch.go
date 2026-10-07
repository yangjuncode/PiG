// SPDX-FileCopyrightText: Copyright (c) 2025 Mario Zechner
// SPDX-License-Identifier: MIT

package codingagent

import (
	"slices"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
	"github.com/MichaelKinsy/PiG/internal/sessionentry"
)

// Ports packages/coding-agent/src/core/session-manager.ts (createBranchedSession).
// Removing label entries rechains the retained path and moves compaction boundaries to the next retained entry. Resolved labels retain their original timestamps.
//
// 分支快照的"重链"语义：label 条目不进克隆（它们是展示层标注），
// 路径上其余条目按顺序把 parentId 改指向前一条保留条目，树就塌成
// 直线；若压缩条目的 firstKeptEntryId 恰指向被剔除的 label，则改写
// 到其后第一条保留条目。第二遍从源会话收齐"仍在生效的 label"
// （后写覆盖先写、空值即删除），对被保留的目标重新发新 ID、挂在
// 克隆末尾——原条目不搬，避免 label 的 targetId 在克隆里悬空。
func branchedSessionEntries(source *Session, path []SessionEntry) ([]json.RawMessage, []FileEntry, error) {
	records := make([]json.RawMessage, 0, len(path))
	decoded := make([]FileEntry, 0, len(path))
	retained := make(map[string]struct{})
	replacements := make(map[string]string)
	var pending []string
	var parent *string
	for _, entry := range path {
		if entry.Base().Type == "label" {
			pending = append(pending, entry.Base().ID)
			continue
		}
		for _, id := range pending {
			replacements[id] = entry.Base().ID
		}
		pending = pending[:0]
		raw, err := replaceJSONField(entry.Raw(), "parentId", parent)
		if err != nil {
			return nil, nil, err
		}
		if entry.Base().Type == "compaction" {
			var comp CompactionEntry
			if err := json.Unmarshal(raw, &comp); err != nil {
				return nil, nil, err
			}
			if replacement, found := replacements[comp.FirstKeptEntryID]; found && comp.FirstKeptEntryID != entry.Base().ID {
				raw, err = replaceJSONField(raw, "firstKeptEntryId", replacement)
				if err != nil {
					return nil, nil, err
				}
			}
		}
		records = append(records, raw)
		decoded = append(decoded, rebasedMessage(entry, parent, raw))
		retained[entry.Base().ID] = struct{}{}
		id := entry.Base().ID
		parent = &id
	}
	labels := make(map[string]LabelEntry)
	var order []string
	for _, entry := range source.getEntriesOfType("label") {
		if entry.Base().Type != "label" {
			continue
		}
		var label LabelEntry
		if err := json.Unmarshal(entry.Raw(), &label); err != nil {
			return nil, nil, err
		}
		if label.Label == nil || *label.Label == "" {
			delete(labels, label.TargetID)
			order = slices.DeleteFunc(order, func(id string) bool { return id == label.TargetID })
			continue
		}
		if _, found := labels[label.TargetID]; !found {
			order = append(order, label.TargetID)
		}
		labels[label.TargetID] = label
	}
	for _, target := range order {
		if _, found := retained[target]; !found {
			continue
		}
		label := labels[target]
		id, err := generateID(retained)
		if err != nil {
			return nil, nil, err
		}
		label.ID = id
		label.ParentID = parent
		raw, err := marshalSessionLine(label)
		if err != nil {
			return nil, nil, err
		}
		records = append(records, raw)
		decoded = append(decoded, nil)
		retained[id] = struct{}{}
		parent = &id
	}
	return records, decoded, nil
}

// rebasedMessage is the decode of raw, entry's record with its parentId set to parent, when entry is a decoded message entry whose parent
// raw names as a string: the same entry with that parent and raw as its record, since the rewrite changes no other member. It is nil for
// any other entry, which the caller decodes from raw.
func rebasedMessage(entry SessionEntry, parent *string, raw json.RawMessage) FileEntry {
	message, ok := asMessage(entry)
	if !ok || (parent != nil && sessionentry.IsJSValue(*parent)) {
		return nil
	}
	if parent != nil {
		id := *parent
		parent = &id
	}
	message.ParentID = parent
	return sessionentry.WithRecord(message, raw)
}
