package shape

import (
	"strings"

	"wb2api/internal/jsonx"
)

var schemaTypeMap = map[string]string{
	"str": "string", "text": "string", "int": "integer", "bool": "boolean",
	"dict": "object", "list": "array", "float": "number",
}

var schemaValidTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"array": true, "object": true, "null": true,
}

func normalizeToolChoice(obj *jsonx.Obj) {
	tc, ok := obj.Lookup("tool_choice")
	if !ok {
		return
	}
	switch t := tc.(type) {
	case string:
		// "none" keeps the tool declarations (removing them used to push models
		// into printing calls as text and loop). Other strings pass through.
		if strings.ToLower(strings.TrimSpace(t)) == "none" {
			obj.Set("tool_choice", "none")
		}
	case *jsonx.Obj:
		typ := ""
		if jsonx.Truthy(t.Get("type")) {
			typ = lowerTrim(t.Get("type"))
		}
		switch typ {
		case "none":
			obj.Set("tool_choice", "none")
		case "auto", "required":
			obj.Set("tool_choice", typ)
		case "function":
			name := ""
			if fn, ok := asObj(t.Get("function")); ok && jsonx.Truthy(fn.Get("name")) {
				name, _ = fn.Get("name").(string)
			} else if jsonx.Truthy(t.Get("name")) {
				name, _ = t.Get("name").(string)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				name = "auto"
			}
			obj.Set("tool_choice", name)
		default:
			obj.Delete("tool_choice")
		}
	default:
		obj.Delete("tool_choice")
	}
}

// deepCloneObj copies nested objects and arrays so schema normalisation never
// touches the caller's request.
func deepClone(v any) any {
	switch t := v.(type) {
	case *jsonx.Obj:
		c := jsonx.NewObj()
		t.Each(func(k string, x any) { c.Set(k, deepClone(x)) })
		return c
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepClone(x)
		}
		return out
	}
	return v
}

func pyStrOf(v any) string { return jsonx.PyStr(v) }

func deepNormalizeSchema(s *jsonx.Obj) *jsonx.Obj {
	// 1. null metadata fields are rejected upstream.
	for _, k := range s.Keys() {
		if v, _ := s.Lookup(k); v == nil {
			switch k {
			case "description", "title", "format", "pattern", "items", "default":
				s.Delete(k)
			}
		}
	}
	// 2. type declarations.
	if typ, ok := s.Lookup("type"); ok {
		switch t := typ.(type) {
		case string:
			low := strings.ToLower(strings.TrimSpace(t))
			if m, ok := schemaTypeMap[low]; ok {
				s.Set("type", m)
			} else if !schemaValidTypes[low] {
				if s.Has("properties") {
					s.Set("type", "object")
				} else if s.Has("items") {
					s.Set("type", "array")
				} else {
					s.Delete("type")
				}
			} else {
				s.Set("type", low)
			}
		case []any:
			out := []any{}
			for _, x := range t {
				if !jsonx.Truthy(x) {
					continue
				}
				low := strings.ToLower(pyStrOf(x))
				if m, ok := schemaTypeMap[low]; ok {
					out = append(out, m)
				} else {
					out = append(out, low)
				}
			}
			s.Set("type", out)
		}
	}
	// 3. object types always have a properties dict.
	typVal, hasType := s.Lookup("type")
	if typVal == "object" || (s.Has("properties") && (!hasType || typVal == nil)) {
		if tv, _ := s.Lookup("type"); tv == nil {
			s.Set("type", "object")
		}
		if _, ok := asObj(s.Get("properties")); !ok {
			s.Set("properties", jsonx.NewObj())
		}
	}
	// 4. required: list of non-empty strings.
	if req, ok := s.Lookup("required"); ok {
		switch r := req.(type) {
		case []any:
			cleaned := []any{}
			for _, x := range r {
				if x != nil && strings.TrimSpace(pyStrOf(x)) != "" {
					cleaned = append(cleaned, pyStrOf(x))
				}
			}
			if len(cleaned) > 0 {
				s.Set("required", cleaned)
			} else {
				s.Delete("required")
			}
		case string:
			if strings.TrimSpace(r) != "" {
				s.Set("required", []any{strings.TrimSpace(r)})
			} else {
				s.Delete("required")
			}
		default:
			s.Delete("required")
		}
	}
	// 5. items.
	if items, ok := s.Lookup("items"); ok {
		switch it := items.(type) {
		case []any:
			var first *jsonx.Obj
			for _, x := range it {
				if o, ok := asObj(x); ok {
					first = o
					break
				}
			}
			if first != nil {
				s.Set("items", deepNormalizeSchema(first.Clone()))
			} else {
				d := jsonx.NewObj()
				d.Set("type", "string")
				s.Set("items", d)
			}
		case *jsonx.Obj:
			deepNormalizeSchema(it)
		case nil:
			s.Delete("items")
		}
	}
	// 6. properties.
	if props, ok := asObj(s.Get("properties")); ok {
		props.Each(func(_ string, v any) {
			if o, ok := asObj(v); ok {
				deepNormalizeSchema(o)
			}
		})
	}
	// 7. definitions.
	for _, k := range []string{"$defs", "definitions"} {
		if defs, ok := asObj(s.Get(k)); ok {
			defs.Each(func(_ string, v any) {
				if o, ok := asObj(v); ok {
					deepNormalizeSchema(o)
				}
			})
		}
	}
	// 8. composition keywords.
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := s.Get(k).([]any); ok {
			for _, v := range list {
				if o, ok := asObj(v); ok {
					deepNormalizeSchema(o)
				}
			}
		}
	}
	return s
}

func normalizeToolParameters(fn *jsonx.Obj) {
	params, ok := asObj(fn.Get("parameters"))
	if !ok {
		p := jsonx.NewObj()
		p.Set("type", "object")
		p.Set("properties", jsonx.NewObj())
		fn.Set("parameters", p)
		return
	}
	p := deepClone(params).(*jsonx.Obj)
	if p.Get("type") != "object" {
		p.Set("type", "object")
	}
	if _, ok := asObj(p.Get("properties")); !ok {
		p.Set("properties", jsonx.NewObj())
	}
	deepNormalizeSchema(p)
	fn.Set("parameters", p)
}

func normalizeTools(obj *jsonx.Obj) {
	if obj.Has("functions") && !obj.Has("tools") {
		funcs := obj.Get("functions")
		obj.Delete("functions")
		if list, ok := funcs.([]any); ok {
			tools := []any{}
			for _, f := range list {
				if fo, ok := asObj(f); ok {
					t := jsonx.NewObj()
					t.Set("type", "function")
					t.Set("function", fo)
					tools = append(tools, t)
				}
			}
			obj.Set("tools", tools)
		}
	}
	tools, isList := obj.Get("tools").([]any)
	if !isList || len(tools) == 0 {
		if obj.Has("tools") {
			obj.Delete("tools")
		}
		return
	}
	norm := []any{}
	for _, t := range tools {
		to, ok := asObj(t)
		if !ok {
			continue
		}
		if to.Has("name") && !to.Has("function") {
			fn := jsonx.NewObj()
			fn.Set("name", orDefault(to.Get("name"), ""))
			fn.Set("description", orDefault(to.Get("description"), ""))
			fn.Set("parameters", orDefault(to.Get("parameters"), jsonx.NewObj()))
			if to.Has("strict") {
				fn.Set("strict", to.Get("strict"))
			}
			normalizeToolParameters(fn)
			item := jsonx.NewObj()
			item.Set("type", "function")
			item.Set("function", fn)
			norm = append(norm, item)
			continue
		}
		item := to.Clone()
		if item.Get("type") != "function" {
			item.Set("type", "function")
		}
		if fo, ok := asObj(item.Get("function")); ok {
			fn := fo.Clone()
			normalizeToolParameters(fn)
			item.Set("function", fn)
		}
		norm = append(norm, item)
	}
	if len(norm) > 0 {
		obj.Set("tools", norm)
	} else {
		obj.Delete("tools")
	}
}

// orDefault is Python's `v or def`.
func orDefault(v, def any) any {
	if jsonx.Truthy(v) {
		return v
	}
	return def
}
