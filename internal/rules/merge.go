package rules

import "encoding/json"

// mergeJSON deep-merges override into base. Objects merge key by key; any
// other value (including arrays) in override replaces the base value. A key set
// to null in override deletes it.
func mergeJSON(base, override []byte) ([]byte, error) {
	var b, o any
	if err := json.Unmarshal(base, &b); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(override, &o); err != nil {
		return nil, err
	}
	return json.Marshal(mergeValue(b, o))
}

func mergeValue(base, override any) any {
	bm, bok := base.(map[string]any)
	om, ook := override.(map[string]any)
	if !bok || !ook {
		return override
	}
	out := make(map[string]any, len(bm)+len(om))
	for k, v := range bm {
		out[k] = v
	}
	for k, v := range om {
		if v == nil {
			delete(out, k)
			continue
		}
		if existing, ok := out[k]; ok {
			out[k] = mergeValue(existing, v)
		} else {
			out[k] = v
		}
	}
	return out
}
