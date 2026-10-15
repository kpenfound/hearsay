package connector

// State is an artifact's state, stated by its source rather than inferred.
// Sources map their own states onto the fixed core vocabulary and may keep
// their original label in Native.
type State struct {
	Core CoreState `json:"core"`
	// Native is the source's label, kept verbatim. Nil means absent; a present
	// label must be 1–64 bytes of valid UTF-8, including any whitespace.
	Native *string `json:"native,omitempty"`
}

// MaxStateNativeLen is the longest source state label, in bytes.
const MaxStateNativeLen = 64

// CoreState is one of five fixed artifact states. It has no extension mechanism.
type CoreState string

// The core state vocabulary.
const (
	CoreStateProposed  CoreState = "proposed"  // described but not started
	CoreStateStarted   CoreState = "started"   // being worked on
	CoreStateBlocked   CoreState = "blocked"   // started but waiting on something
	CoreStateCompleted CoreState = "completed" // done
	CoreStateAbandoned CoreState = "abandoned" // stopped and not done
)

// Valid reports whether s is one of the five core states.
func (s CoreState) Valid() bool {
	switch s {
	case CoreStateProposed, CoreStateStarted, CoreStateBlocked, CoreStateCompleted, CoreStateAbandoned:
		return true
	default:
		return false
	}
}
