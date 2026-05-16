package log

import "encoding/json"

// jsonMarshal is a thin wrapper that exists so tests can stub it if needed.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
