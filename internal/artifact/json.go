package artifact

import "encoding/json"

func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
