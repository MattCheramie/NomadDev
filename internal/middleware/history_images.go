//go:build gemini || openai || anthropic

package middleware

import "encoding/base64"

// extractHistoryImages reads the optional "images" array from one
// persisted history Turn's parsed JSON, decoding each base64 entry into
// an ImageData. Empty or malformed entries are skipped silently — same
// safe-degradation pattern the translators use for the text field.
//
// Shared across the Gemini, OpenAI, and Anthropic translators so the
// history wire shape stays consistent: {text, images: [{media_type, data}]}.
func extractHistoryImages(raw map[string]any) []ImageData {
	rawImgs, _ := raw["images"].([]any)
	if len(rawImgs) == 0 {
		return nil
	}
	out := make([]ImageData, 0, len(rawImgs))
	for _, entry := range rawImgs {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		mt, _ := obj["media_type"].(string)
		data, _ := obj["data"].(string)
		if mt == "" || data == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil || len(decoded) == 0 {
			continue
		}
		out = append(out, ImageData{MediaType: mt, Data: decoded})
	}
	return out
}
