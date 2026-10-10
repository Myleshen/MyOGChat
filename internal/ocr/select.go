package ocr

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// ErrUnavailable marks an engine that cannot run right now (Docker or the
// Tesseract image is missing, or the image format is one the engine cannot
// handle). FallbackEngine retries on it; ordinary OCR failures (a blank image,
// a bad model reply) do not trigger a fallback so we never pay for vision just
// because Tesseract found no text.
var ErrUnavailable = errors.New("OCR engine unavailable")

// FallbackEngine runs Primary and, when Primary reports ErrUnavailable, retries
// with Secondary. WithKey propagates the per-request key to keyed children.
type FallbackEngine struct {
	Primary   Engine
	Secondary Engine
}

// WithKey returns a copy with the key bound on both children (non-keyed
// children are passed through unchanged).
func (f *FallbackEngine) WithKey(apiKey, sessionID string) Engine {
	return &FallbackEngine{
		Primary:   withKey(f.Primary, apiKey, sessionID),
		Secondary: withKey(f.Secondary, apiKey, sessionID),
	}
}

// withKey binds key to e when e is a KeyedEngine.
func withKey(e Engine, apiKey, sessionID string) Engine {
	if ke, ok := e.(KeyedEngine); ok {
		return ke.WithKey(apiKey, sessionID)
	}
	return e
}

// OCR runs Primary, falling back to Secondary only on ErrUnavailable.
func (f *FallbackEngine) OCR(ctx context.Context, image []byte) (string, error) {
	if f.Primary == nil {
		if f.Secondary == nil {
			return "", ErrUnavailable
		}
		return f.Secondary.OCR(ctx, image)
	}
	text, err := f.Primary.OCR(ctx, image)
	if err == nil {
		return text, nil
	}
	if errors.Is(err, ErrUnavailable) && f.Secondary != nil {
		return f.Secondary.OCR(ctx, image)
	}
	return "", err
}

// SelectEngine resolves the configured MYOGCHAT_OCR_ENGINE mode to one engine:
//
//	tesseract -> tess
//	vision    -> vision
//	auto      -> tess when it was preflighted as available, else vision; when
//	             both exist, tess runs first with vision as a fallback
//
// A nil engine means OCR is unavailable; the caller decides how to report it.
func SelectEngine(mode string, tess, vision Engine) Engine {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "vision":
		return vision
	case "auto":
		switch {
		case tess != nil && vision != nil:
			return &FallbackEngine{Primary: tess, Secondary: vision}
		case tess != nil:
			return tess
		default:
			return vision
		}
	default: // "tesseract"
		return tess
	}
}

// DockerAvailable reports whether the docker binary exists, the daemon is
// reachable, and the given image is present. It is used once at startup to
// decide `auto`; a failure means fall back to vision (or nothing).
func DockerAvailable(ctx context.Context, docker, image string) bool {
	if strings.TrimSpace(docker) == "" || strings.TrimSpace(image) == "" {
		return false
	}
	if _, err := exec.LookPath(docker); err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(cctx, docker, "info").Run(); err != nil {
		return false
	}
	if err := exec.CommandContext(cctx, docker, "image", "inspect", image).Run(); err != nil {
		return false
	}
	return true
}
