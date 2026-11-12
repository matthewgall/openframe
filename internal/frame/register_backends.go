package frame

// Hardware display backends register themselves with the display
// factory from their init; the frame agent pulls them in here (the
// server binary never imports this package, so it stays graphics-free
// per ADR-0006).
import _ "github.com/matthewgall/openframe/internal/frame/display/drm"
