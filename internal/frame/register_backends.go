//go:build linux

package frame

// Hardware display backends register themselves with the display
// factory via blank import (the backend package's init calls
// display.Register). The DRM backend is Linux-only (DRM/KMS), so the
// registration itself is gated to Linux; other platforms fall back to
// the headless dev renderer (factory "auto" mode).

import _ "github.com/matthewgall/openframe/internal/frame/display/drm"
