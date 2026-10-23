package story

import "encoding/json"

func encode(v any) string { b, _ := json.Marshal(v); return string(b) }
