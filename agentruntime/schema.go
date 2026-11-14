package agentruntime

import (
	"fmt"
	"math"
	"strings"
)

// validateSchema validates a JSON value (as decoded by encoding/json into
// any) against a JSON Schema subset: type, required, properties,
// additionalProperties, enum, items, minLength, maxLength, minItems,
// maxItems, minimum, maximum. A nil schema accepts anything.
func validateSchema(schema map[string]any, v any) error {
	var errs []string
	validateAt(schema, v, "$", &errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func validateAt(schema map[string]any, v any, path string, errs *[]string) {
	if schema == nil {
		return
	}
	if t, ok := schema["type"]; ok {
		if !matchesType(t, v) {
			*errs = append(*errs, fmt.Sprintf("%s: expected type %v", path, t))
			return
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if jsonEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			*errs = append(*errs, fmt.Sprintf("%s: value not in enum %v", path, enum))
		}
	} else if enumS, ok := schema["enum"].([]string); ok {
		s, isStr := v.(string)
		found := false
		for _, e := range enumS {
			if isStr && e == s {
				found = true
				break
			}
		}
		if !found {
			*errs = append(*errs, fmt.Sprintf("%s: value not in enum %v", path, enumS))
		}
	}
	switch val := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for _, r := range toStrings(schema["required"]) {
			if _, ok := val[r]; !ok {
				*errs = append(*errs, fmt.Sprintf("%s: missing required property %q", path, r))
			}
		}
		for k, child := range val {
			if ps, ok := props[k].(map[string]any); ok {
				validateAt(ps, child, path+"."+k, errs)
				continue
			}
			switch ap := schema["additionalProperties"].(type) {
			case bool:
				if !ap {
					*errs = append(*errs, fmt.Sprintf("%s: unexpected property %q", path, k))
				}
			case map[string]any:
				validateAt(ap, child, path+"."+k, errs)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, it := range val {
				validateAt(items, it, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
		if n, ok := num(schema["minItems"]); ok && float64(len(val)) < n {
			*errs = append(*errs, fmt.Sprintf("%s: fewer than %v items", path, n))
		}
		if n, ok := num(schema["maxItems"]); ok && float64(len(val)) > n {
			*errs = append(*errs, fmt.Sprintf("%s: more than %v items", path, n))
		}
	case string:
		if n, ok := num(schema["minLength"]); ok && float64(len([]rune(val))) < n {
			*errs = append(*errs, fmt.Sprintf("%s: shorter than %v", path, n))
		}
		if n, ok := num(schema["maxLength"]); ok && float64(len([]rune(val))) > n {
			*errs = append(*errs, fmt.Sprintf("%s: longer than %v", path, n))
		}
	case float64:
		if n, ok := num(schema["minimum"]); ok && val < n {
			*errs = append(*errs, fmt.Sprintf("%s: below minimum %v", path, n))
		}
		if n, ok := num(schema["maximum"]); ok && val > n {
			*errs = append(*errs, fmt.Sprintf("%s: above maximum %v", path, n))
		}
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func matchesType(t any, v any) bool {
	switch tt := t.(type) {
	case string:
		return matchesOne(tt, v)
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok && matchesOne(s, v) {
				return true
			}
		}
		return false
	case []string:
		for _, s := range tt {
			if matchesOne(s, v) {
				return true
			}
		}
		return false
	}
	return true
}

func matchesOne(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}

func jsonEqual(a, b any) bool {
	if fa, ok := num(a); ok {
		fb, ok2 := num(b)
		return ok2 && fa == fb
	}
	switch x := a.(type) {
	case string, bool, nil:
		return a == b
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			if !jsonEqual(xv, y[k]) {
				return false
			}
		}
		return true
	}
	return false
}
