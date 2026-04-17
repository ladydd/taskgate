package vlm

import "encoding/json"

// SanitizeInput strips the vlm_api_key from the raw input before persistence.
// This is passed to the store as an InputSanitizer hook.
func SanitizeInput(input json.RawMessage) json.RawMessage {
	var m map[string]interface{}
	if err := json.Unmarshal(input, &m); err != nil {
		return input
	}

	if _, ok := m["vlm_api_key"]; ok {
		m["vlm_api_key"] = "***"
	}

	sanitized, err := json.Marshal(m)
	if err != nil {
		return input
	}
	return sanitized
}
