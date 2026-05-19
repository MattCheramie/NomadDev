package wsserver

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// allowedImageMediaTypes restricts user.intent attachments to formats every
// supported provider accepts. Anthropic explicitly lists this set; OpenAI
// and Gemini accept more but we intersect for portability.
var allowedImageMediaTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/gif":  {},
	"image/webp": {},
}

// decodeIntentImages validates and decodes the base64 image attachments on
// one user.intent envelope. Caps are read from MiddlewareConfig and the
// helper signals every rejection reason verbatim so the mobile UI can show
// a useful toast.
//
// Returns nil on success when there are no images (zero-allocation fast
// path). On any validation failure, returns a typed error the dispatch
// layer maps to a bad_envelope event.
func decodeIntentImages(in []event.ImageInput, maxCount, maxBytes int) ([]middleware.ImageData, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if maxCount <= 0 || maxBytes <= 0 {
		return nil, errors.New("image attachments are disabled (NOMADDEV_USER_INTENT_MAX_IMAGES or _MAX_IMAGE_BYTES is 0)")
	}
	if len(in) > maxCount {
		return nil, fmt.Errorf("too many image attachments: got %d, max %d", len(in), maxCount)
	}
	out := make([]middleware.ImageData, 0, len(in))
	for i, img := range in {
		if _, ok := allowedImageMediaTypes[img.MediaType]; !ok {
			return nil, fmt.Errorf("image[%d]: unsupported media_type %q (allowed: image/jpeg, image/png, image/gif, image/webp)", i, img.MediaType)
		}
		// Reject early on encoded length so we don't allocate ~4/3 of the
		// declared budget just to discover the image is oversized — base64
		// inflates by ~33%, so encoded > maxBytes*2 is comfortably over.
		if len(img.Data) > maxBytes*2 {
			return nil, fmt.Errorf("image[%d]: encoded size %d exceeds cap %d", i, len(img.Data), maxBytes)
		}
		raw, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil {
			return nil, fmt.Errorf("image[%d]: invalid base64: %w", i, err)
		}
		if len(raw) > maxBytes {
			return nil, fmt.Errorf("image[%d]: decoded size %d exceeds cap %d", i, len(raw), maxBytes)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("image[%d]: empty payload", i)
		}
		out = append(out, middleware.ImageData{
			MediaType: img.MediaType,
			Data:      raw,
		})
	}
	return out, nil
}
