package mcp

// ===========================================================================
// JSON Schema 的极简构造器
//
// tools/list 的 inputSchema 必须是合法 JSON Schema。手写 map 字面量会出现
// 大量重复（每个属性都是 {"type":..., "description":...}），而且很容易漏掉
// required 与 enum 这类**会直接影响客户端参数校验**的字段。
//
// 这里只做 alpha 版需要的子集：object / string / integer / number /
// boolean / array，加 enum 与 required。不引入任何 schema 库（依赖边界）。
// ===========================================================================

// objSchema 构造一个 object schema。
func objSchema(props map[string]any, required ...string) map[string]any {
	names := make([]any, 0, len(required))
	for _, r := range required {
		names = append(names, r)
	}
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(names) > 0 {
		out["required"] = names
	}
	return out
}

// strProp 是一个字符串属性。
func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// intProp 是一个整数属性。
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// numProp 是一个数值属性。
func numProp(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

// boolProp 是一个布尔属性。
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// enumProp 是一个带枚举取值的字符串属性。
func enumProp(desc string, values ...string) map[string]any {
	enum := make([]any, 0, len(values))
	for _, v := range values {
		enum = append(enum, v)
	}
	return map[string]any{"type": "string", "description": desc, "enum": enum}
}

// arrProp 是一个字符串数组属性。
func arrProp(desc string, items ...string) map[string]any {
	item := map[string]any{"type": "string"}
	if len(items) > 0 {
		enum := make([]any, 0, len(items))
		for _, v := range items {
			enum = append(enum, v)
		}
		item["enum"] = enum
	}
	return map[string]any{
		"type": "array", "description": desc,
		"items": item,
	}
}
