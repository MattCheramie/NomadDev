package state

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// DecodeImageAttachment reads up to MaxImageBytes+1 bytes from r and
// returns a wire-ready ImageInput plus the decoded byte count. The
// MediaType is detected from the file's leading bytes using the standard
// library's http.DetectContentType, with the picked file's extension as a
// fallback when sniffing returns a generic application/octet-stream
// (older Android image providers do this for some HEIC-rejected paths).
//
// The caller passes a hint such as the user-picked filename or content
// URI so the extension fallback has something to look at. Pass "" to
// disable that fallback.
//
// Errors are wrapped so the caller can distinguish "the file is too
// large" (ErrImageTooLarge surfaces from Store.AddPendingImage) from "I
// couldn't parse the MIME type" (ErrUnsupportedMimeType).
func DecodeImageAttachment(r io.Reader, filenameHint string) (event.ImageInput, int, error) {
	// Cap the read at MaxImageBytes+1 so an oversized attachment can be
	// detected without slurping a 4 GB photo.
	limited := io.LimitReader(r, int64(MaxImageBytes)+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return event.ImageInput{}, 0, fmt.Errorf("state: read image: %w", err)
	}
	mt := detectImageMIME(raw, filenameHint)
	if !isAcceptedImageMIME(mt) {
		return event.ImageInput{}, len(raw), ErrUnsupportedMimeType
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	return event.ImageInput{MediaType: mt, Data: enc}, len(raw), nil
}

// detectImageMIME runs http.DetectContentType on the first 512 bytes and
// falls back to a filename-extension lookup when the sniffer can't pick a
// known image type — common on Android URIs that point through SAF
// content providers.
func detectImageMIME(data []byte, filenameHint string) string {
	mt := http.DetectContentType(data)
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = mt[:i]
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch mt {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return mt
	}
	// http.DetectContentType returns image/jpeg for actual JPEGs but
	// "image/x-icon" or "application/octet-stream" for some less common
	// variants — fall through to the extension lookup.
	switch extOf(filenameHint) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	}
	return mt
}

func extOf(name string) string {
	name = strings.ToLower(name)
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return name[i+1:]
}
