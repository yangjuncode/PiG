// 中文导读：会话恢复与条目迁移：JSONL 条目 → Session 对象、版本迁移与索引重建。
// SPDX-FileCopyrightText: Copyright (c) 2025 Mario Zechner
// SPDX-License-Identifier: MIT

package codingagent

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/MichaelKinsy/PiG/internal/lazyregexp"

	"github.com/google/uuid"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
	"github.com/MichaelKinsy/PiG/internal/jsonstringify"
	"github.com/MichaelKinsy/PiG/internal/sessionentry"
)

// Ports packages/coding-agent/src/core/session-manager.ts (_loadEntries and _buildIndex).
// A header owns restored identity. Headerless entries are current-version data.
func newSessionFromEntries(cwd, id string, entries []FileEntry) (*Session, error) {
	header, err := findSessionHeader(entries)
	if err != nil {
		return nil, err
	}
	// header 版本低于当前版本 → 先整段迁移再找一次 header：
	// 迁移后的 header 是迁移函数新写的记录（ID 可能重写）。
	if header != nil && header.Version < CurrentSessionVersion {
		if err := MigrateSessionEntries(entries); err != nil {
			return nil, err
		}
		// The migrated header is the record MigrateSessionEntries wrote, found as before by its type: its version is the current one in its
		// fields and in the record it writes (a header carrying the version-2 record would report version 2 to an extension's getHeader).
		if header, err = findSessionHeader(entries); err != nil {
			return nil, err
		}
	}
	var s *Session
	if header != nil {
		// 有 header：会话身份由 header 完全接管（ID、cwd 等）。
		s = NewSession(header.ID, cwd)
		s.header = *header
	} else {
		// 无 header 视为当前版本数据：用调用方给的 id（空则
		// 生成）建会话。
		var option *string
		if id != "" {
			option = &id
		}
		s, err = newSessionWithOptions(cwd, option, "")
		if err != nil {
			return nil, err
		}
	}
	// 重建内存索引：按原始顺序回放条目，parentId 决定树结构
	// （addSlotLocked 挂到父槽），leafID 始终停在最后一条。
	for _, record := range entries {
		entry, isEntry := record.(SessionEntry)
		if !isEntry {
			continue
		}
		base := entry.Base()
		if base.Type == "session" {
			continue
		}
		s.stats.add(entry.Raw(), base.Type)
		s.noteNameLocked(entry.Raw(), base.Type)
		raw := entry.Raw()
		s.addSlotLocked(newSlot(compactionWithoutRecord(entry, raw)), base.ParentID, raw)
		s.leafID = leafOf(base.ID)
	}
	s.hasConversation = s.stats.stats.UserMessages > 0 || s.stats.stats.AssistantMessages > 0
	s.setBranchLocked()
	return s, nil
}

// findSessionHeader is the first record of type "session", nil when there is none. _loadEntries finds the header by its type alone, so a
// record of that type whose id is missing or ill-typed, which is no SessionHeader member, still owns the identity.
// 在条目序列里找 session header——版本号判定与迁移都靠它。
func findSessionHeader(entries []FileEntry) (*SessionHeader, error) {
	for _, entry := range entries {
		if entry.Raw() == nil {
			continue
		}
		switch record := entry.(type) {
		case SessionHeader:
			return &record, nil
		case SessionEntry:
			if record.Base().Type != "session" {
				continue
			}
			header, err := sessionentry.DecodeSessionHeader(record.Raw())
			if err != nil {
				return nil, err
			}
			return &header, nil
		}
	}
	return nil, nil
}

var sessionIDPattern = lazyregexp.New(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// newSessionWithOptions preserves the distinction between an omitted and an explicit session ID.
func newSessionWithOptions(cwd string, id *string, parentSession string) (*Session, error) {
	var value string
	if id == nil {
		var err error
		value, err = generateSessionID()
		if err != nil {
			return nil, err
		}
	} else {
		if !sessionIDPattern.MatchString(*id) {
			return nil, errors.New("Session id must be non-empty, contain only alphanumeric characters, '-', '_', and '.', and start and end with an alphanumeric character")
		}
		value = *id
	}
	s := NewSession(value, cwd)
	s.header.ParentSession = parentSession
	return s, nil
}

// MigrateSessionEntries applies Pi's v1 tree and v2 custom-message migrations, preserving JSON member order. The version is the first session header's `version`, 1 when the entries have no header or the header omits it (session-manager.ts migrateToCurrentVersion); the entries migrate in place and entries already at the current version stay unchanged.
//
// 两段迁移：v<2 的文件没有 id/parentId（顺序即父子链），迁移给每
// 条补发新 ID 并按出现顺序连 parentId；v1 压缩条目的
// firstKeptEntryIndex（索引）改写成对应条目的 ID。v<3 把
// role=hookMessage 的消息改写成 custom。全程只改 JSON 字段值，
// 保留成员顺序。
func MigrateSessionEntries(entries []FileEntry) error {
	version := 1
	for _, entry := range entries {
		raw := entry.Raw()
		if !isObjectLine(raw) {
			continue
		}
		var header struct {
			Type    string `json:"type"`
			Version *int   `json:"version"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.Type == "session" {
			if header.Version != nil {
				version = *header.Version
			}
			break
		}
	}
	if version >= CurrentSessionVersion {
		return nil
	}
	// v<2 的文件没有 id/parentId：顺序即父子链，previous 记录
	// 上一条的 ID，遍历时把每条挂到前驱后面。
	var previous *string
	ids := make(map[string]struct{})
	for i, entry := range entries {
		raw := entry.Raw()
		if !isObjectLine(raw) {
			// 非对象行（裸字符串/数字/布尔）在 v<2 里也要发 ID
			// 以维持链式语义——JS 里给原始值赋属性会抛 TypeError
			// （严格模式），这里复刻同样的错误。
			if version < 2 {
				if err := assignIDToNonObject(raw, ids, &previous); err != nil {
					return err
				}
			}
			continue
		}
		var probe struct {
			SessionEntryBase
			FirstKeptEntryIndex json.RawMessage `json:"firstKeptEntryIndex"`
			Message             json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return err
		}
		var err error
		if probe.Type == "session" {
			// header 条目：只把 version 提升到当前版本，其余字段原样。
			raw, err = replaceJSONField(raw, "version", CurrentSessionVersion)
			if err != nil {
				return err
			}
			entries[i] = sessionentry.DecodeFileEntry(raw)
			continue
		}
		if version < 2 {
			id, idErr := generateID(ids)
			if idErr != nil {
				return idErr
			}
			ids[id] = struct{}{}
			raw, err = replaceJSONField(raw, "id", id)
			if err != nil {
				return err
			}
			raw, err = replaceJSONField(raw, "parentId", previous)
			if err != nil {
				return err
			}
			previous = &id
			entries[i] = sessionentry.DecodeFileEntry(raw)
			// upstream: session-manager.ts migrateV1ToV2 (`typeof comp.firstKeptEntryIndex === "number"`, then `entries[index]`): another type is left alone, and a number that is no entry index (fractional, negative, past the end) only drops the member.
			// compaction 条目的 firstKeptEntryIndex（v1 用数组下标）
			// 要改写为对应条目的 ID（v2 用 ID 引用）；下标非法
			// （小数/负/越界）时该成员只被删掉、不留替代。
			var indexNumber float64
			if probe.Type == "compaction" && len(probe.FirstKeptEntryIndex) > 0 && probe.FirstKeptEntryIndex[0] != '"' && string(probe.FirstKeptEntryIndex) != "null" && json.Unmarshal(probe.FirstKeptEntryIndex, &indexNumber) == nil {
				//portlint:allow numbers float64(index) == indexNumber and the range check admit only an exact entry index, as Pi's entries[index] is undefined for any other number (session-manager.ts migrateV1ToV2)
				if index := int(indexNumber); float64(index) == indexNumber && index >= 0 && index < len(entries) {
					var target SessionEntryBase
					if err := json.Unmarshal(entries[index].Raw(), &target); err != nil {
						return err
					}
					if target.Type != "session" && target.ID != "" {
						raw, err = replaceJSONField(raw, "firstKeptEntryId", target.ID)
						if err != nil {
							return err
						}
					}
				}
				raw, err = rewriteJSONField(raw, "firstKeptEntryIndex", nil, true)
				if err != nil {
					return err
				}
			}
		}
		// v<3 迁移：hookMessage 角色的消息降级为 custom——上游把
		// hook 注入消息从一等角色改成扩展自定义消息。
		if version < 3 && probe.Type == "message" && len(probe.Message) != 0 {
			var message struct {
				Role string `json:"role"`
			}
			if err := json.Unmarshal(probe.Message, &message); err != nil {
				return err
			}
			if message.Role == "hookMessage" {
				messageRaw, err := replaceJSONField(probe.Message, "role", "custom")
				if err != nil {
					return err
				}
				raw, err = replaceJSONField(raw, "message", json.RawMessage(messageRaw))
				if err != nil {
					return err
				}
			}
		}
		entries[i] = sessionentry.DecodeFileEntry(raw)
	}
	return nil
}

// assignIDToNonObject is migrateV1ToV2's `entry.id = generateId(ids); entry.parentId = prevId` on a record that is not an object. An array takes
// the id as a property that JSON.stringify never writes, so the record is unchanged and the chain goes through it; a number, string or boolean
// is a primitive, and the write throws a TypeError in strict mode.
// 给非对象形态的原始行补发条目 ID（迁移：无法解析的记录也要有 ID 才能挂进父子链）。
func assignIDToNonObject(raw json.RawMessage, ids map[string]struct{}, previous **string) error {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		id, err := generateID(ids)
		if err != nil {
			return err
		}
		ids[id] = struct{}{}
		*previous = &id
		return nil
	}
	var kind, text string
	switch {
	case len(trimmed) > 0 && trimmed[0] == '"':
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		kind, text = "string", value
	case len(trimmed) > 0 && (trimmed[0] == 't' || trimmed[0] == 'f'):
		kind, text = "boolean", string(bytes.TrimSpace(raw))
	default:
		canonical, err := jsonstringify.Canonicalize(raw)
		if err != nil {
			return err
		}
		kind, text = "number", string(canonical)
	}
	return fmt.Errorf("Cannot create property 'id' on %s '%s'", kind, text)
}

func generateID(ids map[string]struct{}) (string, error) {
	for range 100 {
		id, err := uuid.NewRandom()
		if err != nil {
			return "", err
		}
		short := id.String()[:8]
		if _, found := ids[short]; !found {
			return short, nil
		}
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
