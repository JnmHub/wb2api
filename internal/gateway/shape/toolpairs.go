package shape

import "wb2api/internal/jsonx"

// Tool-call pairing repair. The upstream rejects a conversation whose tool
// calls and tool results are not paired and adjacent (code 11148) and, because
// every retry replays the same history, retires the whole conversation. These
// functions repair the shapes clients are known to produce.

func emptyContent(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}

func mergeReasoning(dst, src *jsonx.Obj) {
	rc, ok := src.Get("reasoning_content").(string)
	if !ok || rc == "" {
		return
	}
	if prev, ok := dst.Get("reasoning_content").(string); ok && prev != "" {
		dst.Set("reasoning_content", prev+"\n"+rc)
	} else {
		dst.Set("reasoning_content", rc)
	}
}

func nonEmptyList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok && len(l) > 0
}

// mergeAdjacentToolCalls merges back-to-back assistant tool_calls messages.
func mergeAdjacentToolCalls(msgs []any) ([]any, bool) {
	if len(msgs) < 2 {
		return msgs, false
	}
	out := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		o, ok := asObj(m)
		if !ok {
			out = append(out, m)
			continue
		}
		if o.Get("role") == "assistant" && len(out) > 0 {
			if prev, ok := asObj(out[len(out)-1]); ok && prev.Get("role") == "assistant" {
				tcs, hasCalls := nonEmptyList(o.Get("tool_calls"))
				prevCalls, prevHas := nonEmptyList(prev.Get("tool_calls"))
				// Form 1: this message declares calls and carries no content.
				if hasCalls && emptyContent(o.Get("content")) && prevHas {
					np := prev.Clone()
					merged := make([]any, 0, len(prevCalls)+len(tcs))
					merged = append(merged, prevCalls...)
					merged = append(merged, tcs...)
					np.Set("tool_calls", merged)
					mergeReasoning(np, o)
					out[len(out)-1] = np
					changed = true
					continue
				}
				// Form 2: plain string content folds into a content-less
				// tool_calls assistant that precedes it.
				if s, isS := o.Get("content").(string); !o.Has("tool_calls") && isS && s != "" && prevHas && emptyContent(prev.Get("content")) {
					np := prev.Clone()
					np.Set("content", s)
					mergeReasoning(np, o)
					out[len(out)-1] = np
					changed = true
					continue
				}
			}
		}
		out = append(out, m)
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

// repackToolResultBlocks keeps a tool_calls batch and its results adjacent.
func repackToolResultBlocks(msgs []any) ([]any, bool) {
	if len(msgs) < 3 {
		return msgs, false
	}
	out := make([]any, 0, len(msgs))
	changed := false
	i := 0
	for i < len(msgs) {
		m := msgs[i]
		o, ok := asObj(m)
		if !ok || o.Get("role") != "assistant" {
			out = append(out, m)
			i++
			continue
		}
		calls, has := nonEmptyList(o.Get("tool_calls"))
		if !has {
			out = append(out, m)
			i++
			continue
		}
		want := map[string]bool{}
		for _, tc := range calls {
			if t, ok := asObj(tc); ok {
				if id, ok := t.Get("id").(string); ok && id != "" {
					want[id] = true
				}
			}
		}
		out = append(out, m)
		i++
		var results, between []any
		for i < len(msgs) {
			mm, ok := asObj(msgs[i])
			if !ok {
				break
			}
			role := mm.Get("role")
			if role == "tool" {
				id, ok := mm.Get("tool_call_id").(string)
				if !ok || !want[id] {
					break
				}
				results = append(results, msgs[i])
				i++
				continue
			}
			if role == "assistant" {
				break
			}
			between = append(between, msgs[i])
			i++
		}
		if len(results) > 0 {
			out = append(out, results...)
			out = append(out, between...)
			if len(between) > 0 {
				changed = true
			}
		} else {
			out = append(out, between...)
		}
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

// cleanupOrphanToolCalls drops calls without a result and results without a call.
func cleanupOrphanToolCalls(msgs []any) ([]any, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}
	callIDs, resultIDs := map[string]bool{}, map[string]bool{}
	for _, m := range msgs {
		o, ok := asObj(m)
		if !ok {
			continue
		}
		switch o.Get("role") {
		case "tool":
			if id, ok := o.Get("tool_call_id").(string); ok && id != "" {
				resultIDs[id] = true
			}
		case "assistant":
			if calls, ok := o.Get("tool_calls").([]any); ok {
				for _, tc := range calls {
					if t, ok := asObj(tc); ok {
						if id, ok := t.Get("id").(string); ok && id != "" {
							callIDs[id] = true
						}
					}
				}
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return msgs, false
	}
	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}
	changed := false
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		o, ok := asObj(m)
		if !ok {
			out = append(out, m)
			continue
		}
		switch o.Get("role") {
		case "assistant":
			calls, has := nonEmptyList(o.Get("tool_calls"))
			if has {
				kept := make([]any, 0, len(calls))
				for _, tc := range calls {
					if t, ok := asObj(tc); ok {
						if id, ok := t.Get("id").(string); ok && keep[id] {
							kept = append(kept, tc)
						}
					}
				}
				if len(kept) != len(calls) {
					changed = true
					c := o.Clone()
					if len(kept) > 0 {
						c.Set("tool_calls", kept)
					} else {
						c.Delete("tool_calls")
					}
					out = append(out, c)
					continue
				}
			}
		case "tool":
			id, ok := o.Get("tool_call_id").(string)
			if !ok || !keep[id] {
				changed = true
				continue
			}
		}
		out = append(out, m)
	}
	if !changed {
		return msgs, false
	}
	return out, true
}
