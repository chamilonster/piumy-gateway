// Package mediautil: small, vendor-agnostic media helpers shared across
// adapters/tools — first tenant is DecodeDataURL, moved out of
// internal/openwa (ST-E, ct-2026-07-11-1444, before that package was
// deleted) so mcpserver's set_group_icon can decode a data: URL into bytes
// without depending on a specific messaging vendor's package.
package mediautil

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// DecodeDataURL parses "data:<mime>;base64,<payload>" into its raw bytes
// and mime type.
func DecodeDataURL(dataURL string) (data []byte, mime string, err error) {
	const prefix = "data:"
	if !strings.HasPrefix(dataURL, prefix) {
		return nil, "", fmt.Errorf("mediautil: not a data URL")
	}
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("mediautil: malformed data URL, no comma")
	}
	meta := strings.TrimSuffix(dataURL[len(prefix):comma], ";base64")
	payload, err := base64.StdEncoding.DecodeString(dataURL[comma+1:])
	if err != nil {
		return nil, "", err
	}
	return payload, meta, nil
}
