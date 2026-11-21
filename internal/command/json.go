package command

import "github.com/BenedictKing/mobile-bridge/internal/jsoncompat"

// jsoncompat.Object deliberately has private fields. Convert nested values
// before encoding so a receipt never hashes or stores a populated object as {}.
func nativeJSON(value any) any {
	switch v := value.(type) {
	case *jsoncompat.Object:
		if v == nil {
			return nil
		}
		result := map[string]any{}
		for _, key := range v.Keys() {
			result[key] = nativeJSON(v.MustGet(key))
		}
		return result
	case *jsoncompat.Array:
		if v == nil {
			return nil
		}
		result := make([]any, len(v.Items))
		for i, item := range v.Items {
			result[i] = nativeJSON(item)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			result[key] = nativeJSON(item)
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = nativeJSON(item)
		}
		return result
	default:
		return value
	}
}
