package sessions

import (
	"fmt"
	"sort"
	"strings"
)

// Previewers: given a Session, return its last few turns, oldest first. Each
// reads only a bounded tail window of its transcript (widening it if that
// window held no message at all), so opening the preview costs nothing
// proportional to a session's total size. Turn text keeps its line structure
// (see previewStrip) so Markdown can be rendered.

type previewer func(env *Env, s Session) []Turn

var previewers = map[string]previewer{
	"claude":   prevClaude,
	"codex":    prevCodex,
	"droid":    prevDroid,
	"opencode": prevOpencode,
	"copilot":  prevCopilot,
	"grok":     prevGrok,
	"kimi":     prevKimi,
	"pi":       prevPi,
	"omp":      prevPi, // omp keeps pi's append-only entry tree
	"gemini":   prevGemini,
	// cursor's transcript lives in an undocumented sqlite blob format we don't
	// decode, so it has no previewer.
}

// Preview returns the last turns of s; supported is false for agents without
// a previewer.
func Preview(env *Env, s Session) (turns []Turn, supported bool, err error) {
	fn, ok := previewers[s.Agent]
	if !ok {
		return nil, false, nil
	}
	defer func() {
		if r := recover(); r != nil {
			turns, err = nil, fmt.Errorf("%s preview: %v", s.Agent, r)
		}
	}()
	return fn(env, s), true, nil
}

func lastTurns(out []Turn) []Turn {
	if len(out) > maxTurns {
		return out[len(out)-maxTurns:]
	}
	return out
}

// widenTail tries successively larger tail windows until parse finds a turn.
func widenTail(path string, parse func([]byte) []Turn) []Turn {
	var found []Turn
	for _, size := range tailSizes {
		found = parse(tailBytes(path, size))
		if len(found) > 0 || size >= sizeOf(path) {
			break
		}
	}
	return found
}

// textTurn appends a turn when its content survives previewStrip.
func textTurn(out []Turn, role string, text string, ok bool) []Turn {
	if !ok {
		return out
	}
	if text = previewStrip(text); text != "" {
		out = append(out, Turn{role, text})
	}
	return out
}

func anyText(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func isChatRole(v any) (string, bool) {
	r, _ := v.(string)
	return r, r == "user" || r == "assistant"
}

func prevClaude(_ *Env, s Session) []Turn {
	return widenTail(s.Path, func(blob []byte) []Turn {
		var out []Turn
		iterLines(blob, func(e obj) bool {
			typ, _ := e["type"].(string)
			if (typ != "user" && typ != "assistant") || truthy(e["isMeta"]) {
				return true
			}
			msg := sub(e, "message")
			role, _ := msg["role"].(string)
			if role == "" {
				role = typ
			}
			text, ok := blocksToText(msg["content"])
			out = textTurn(out, role, text, ok)
			return true
		})
		return lastTurns(out)
	})
}

// codexItem extracts a chat turn from an `item_completed` event, the format
// Codex writes since 0.150 in place of user_message/agent_message events.
// Item content blocks are typed "text" (user) or "Text" (agent).
func codexItem(p obj) (role, text string, ok bool) {
	if p["type"] != "item_completed" {
		return "", "", false
	}
	item := sub(p, "item")
	switch item["type"] {
	case "UserMessage":
		role = "user"
	case "AgentMessage":
		role = "assistant"
	default:
		return "", "", false
	}
	blocks, _ := item["content"].([]any)
	var parts []string
	for _, b := range blocks {
		block, _ := b.(obj)
		if t := block["type"]; t != "text" && t != "Text" {
			continue
		}
		if s, isStr := block["text"].(string); isStr {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "", "", false
	}
	return role, strings.Join(parts, "\n"), true
}

func prevCodex(_ *Env, s Session) []Turn {
	return widenTail(s.Path, func(blob []byte) []Turn {
		// Transition-era rollouts can carry both formats for the same turns;
		// the legacy events win whenever the window holds any.
		var legacy, items []Turn
		iterLines(blob, func(e obj) bool {
			if e["type"] != "event_msg" {
				return true
			}
			p := sub(e, "payload")
			switch p["type"] {
			case "user_message":
				text, ok := anyText(p["message"])
				legacy = textTurn(legacy, "user", text, ok)
			case "agent_message":
				text, ok := anyText(p["message"])
				legacy = textTurn(legacy, "assistant", text, ok)
			default:
				if role, text, ok := codexItem(p); ok {
					items = textTurn(items, role, text, true)
				}
			}
			return true
		})
		if len(legacy) > 0 {
			return lastTurns(legacy)
		}
		return lastTurns(items)
	})
}

func prevDroid(_ *Env, s Session) []Turn {
	return widenTail(s.Path, func(blob []byte) []Turn {
		var out []Turn
		iterLines(blob, func(e obj) bool {
			if e["type"] != "message" {
				return true
			}
			msg := sub(e, "message")
			role, ok := isChatRole(msg["role"])
			if truthy(msg["hookEventName"]) || !ok {
				return true
			}
			text, ok := blocksToText(msg["content"])
			out = textTurn(out, role, text, ok)
			return true
		})
		return lastTurns(out)
	})
}

func prevOpencode(env *Env, s Session) []Turn {
	storage := pyJoin(env.Home, ".local/share/opencode/storage")
	mdir := pyJoin(pyJoin(storage, "message"), s.ID)
	partBase := pyJoin(storage, "part")
	names := listNames(mdir)
	type file struct {
		path  string
		mtime float64
	}
	files := make([]file, len(names))
	for i, n := range names {
		p := pyJoin(mdir, n)
		files[i] = file{p, mtime(p)}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime < files[j].mtime })
	if len(files) > maxTurns {
		files = files[len(files)-maxTurns:]
	}
	var out []Turn
	for _, f := range files {
		d := loadObj(f.path)
		role, ok := isChatRole(d["role"])
		if !ok {
			continue
		}
		id, _ := d["id"].(string)
		pdir := pyJoin(partBase, id)
		parts := listNames(pdir)
		sort.Strings(parts)
		var texts []string
		for _, n := range parts {
			pd := loadObj(pyJoin(pdir, n))
			if t, ok := pd["text"].(string); ok && pd["type"] == "text" {
				texts = append(texts, t)
			}
		}
		out = textTurn(out, role, strings.Join(texts, "\n"), true)
	}
	return out
}

func prevCopilot(_ *Env, s Session) []Turn {
	return widenTail(pyJoin(s.Path, "events.jsonl"), func(blob []byte) []Turn {
		var out []Turn
		iterLines(blob, func(e obj) bool {
			role := ""
			switch e["type"] {
			case "user.message":
				role = "user"
			case "assistant.message":
				role = "assistant"
			default:
				return true
			}
			text, ok := anyText(sub(e, "data")["content"])
			out = textTurn(out, role, text, ok)
			return true
		})
		return lastTurns(out)
	})
}

func prevGrok(_ *Env, s Session) []Turn {
	return widenTail(pyJoin(s.Path, "chat_history.jsonl"), func(blob []byte) []Turn {
		var out []Turn
		iterLines(blob, func(e obj) bool {
			role, ok := isChatRole(e["type"])
			if !ok {
				return true
			}
			text, ok := blocksToText(e["content"])
			out = textTurn(out, role, text, ok)
			return true
		})
		return lastTurns(out)
	})
}

func prevKimi(_ *Env, s Session) []Turn {
	return widenTail(pyJoin(s.Path, "agents/main/wire.jsonl"), func(blob []byte) []Turn {
		var out []Turn
		iterLines(blob, func(e obj) bool {
			if e["type"] != "context.append_message" {
				return true
			}
			msg := sub(e, "message")
			role, ok := isChatRole(msg["role"])
			if !ok {
				return true
			}
			text, ok := blocksToText(msg["content"])
			out = textTurn(out, role, text, ok)
			return true
		})
		return lastTurns(out)
	})
}

func prevPi(_ *Env, s Session) []Turn {
	return widenTail(s.Path, func(blob []byte) []Turn {
		var entries []obj
		byID := map[string]obj{}
		iterLines(blob, func(e obj) bool {
			if e["type"] == "session" {
				return true
			}
			entries = append(entries, e)
			if id, ok := e["id"].(string); ok {
				byID[id] = e
			}
			return true
		})
		if len(entries) == 0 {
			return nil
		}
		// Session files are append-only trees whose last entry is the active
		// leaf. Follow its parents when the bounded tail contains them so a
		// preview does not mix in abandoned branches; fall back to append
		// order for legacy v1 files without id links.
		source := entries
		if len(byID) > 0 {
			var path []obj
			seen := map[string]bool{}
			for cur := entries[len(entries)-1]; len(cur) > 0; {
				id, isStr := cur["id"].(string)
				if isStr && seen[id] {
					break
				}
				path = append(path, cur)
				if isStr {
					seen[id] = true
				}
				parent, ok := cur["parentId"].(string)
				if !ok {
					break
				}
				cur = byID[parent]
			}
			if len(path) > 0 {
				for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				source = path
			}
		}
		var out []Turn
		for _, e := range source {
			if e["type"] != "message" {
				continue
			}
			msg := sub(e, "message")
			role, ok := isChatRole(msg["role"])
			if !ok {
				continue
			}
			text, ok := blocksToText(msg["content"])
			out = textTurn(out, role, text, ok)
		}
		return lastTurns(out)
	})
}

func prevGemini(_ *Env, s Session) []Turn {
	d, blob, jsonl := geminiFile(s.Path)
	return lastTurns(geminiTurns(geminiRaw(d, blob, jsonl), previewStrip, false))
}
