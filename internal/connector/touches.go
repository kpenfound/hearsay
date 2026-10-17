package connector

import "fmt"

// MaxTouches bounds the repositories one event may name in Payload.Touches.
const MaxTouches = 16

// Touch names a repository and optional repository-relative paths touched by
// an artifact, even when that artifact lives in another source or container.
// An entry with no paths still references the repository's code entity.
type Touch struct {
	Source  string   `json:"source"`
	Project string   `json:"project"`
	Paths   []string `json:"paths,omitempty"`
}

func (p Payload) validateTouches() error {
	if len(p.Touches) > MaxTouches {
		return fmt.Errorf("%w: payload.touches holds %d entries, the limit is %d", ErrInvalidEvent, len(p.Touches), MaxTouches)
	}
	paths := 0
	for i, touch := range p.Touches {
		if !ValidSourceID(touch.Source) {
			return fmt.Errorf("%w: payload.touches[%d].source %q is not a source id", ErrInvalidEvent, i, touch.Source)
		}
		if touch.Project == "" {
			return fmt.Errorf("%w: payload.touches[%d].project is empty", ErrInvalidEvent, i)
		}
		paths += len(touch.Paths)
		if paths > MaxPaths {
			return fmt.Errorf("%w: payload.touches holds more than %d paths in total", ErrInvalidEvent, MaxPaths)
		}
	}
	return nil
}
