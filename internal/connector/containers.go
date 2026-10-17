package connector

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// MaxACLContainers bounds the containers one access-list entry may defer to.
const MaxACLContainers = 16

// ContainerRef names a container in a configured source, without its display
// name or kind. NativeID is the same stable id as Container.NativeID.
type ContainerRef struct {
	Source   string `json:"source"`
	NativeID string `json:"native_id"`
}

// CanonicalContainers returns a sorted, deduplicated copy of containers and
// its key: lowercase SHA-256 hex of the compact JSON encoding of that copy.
// Ordering is byte order on (source, native_id); JSON object fields are source
// then native_id, using encoding/json's standard string escaping. The input
// is unchanged. Empty lists, more than MaxACLContainers input entries, invalid
// source ids and empty native ids are refused with ErrInvalidEvent.
//
// Both connectors emitting ACLContainers and readers resolving them use this
// function so that list order and duplicates cannot change the grant's key.
func CanonicalContainers(containers []ContainerRef) ([]ContainerRef, string, error) {
	if len(containers) == 0 || len(containers) > MaxACLContainers {
		return nil, "", fmt.Errorf("%w: containers must hold 1–%d entries", ErrInvalidEvent, MaxACLContainers)
	}
	for i, container := range containers {
		if !ValidSourceID(container.Source) {
			return nil, "", fmt.Errorf("%w: containers[%d].source %q is not a source id", ErrInvalidEvent, i, container.Source)
		}
		if container.NativeID == "" {
			return nil, "", fmt.Errorf("%w: containers[%d].native_id is empty", ErrInvalidEvent, i)
		}
	}
	canonical := slices.Clone(containers)
	slices.SortFunc(canonical, func(a, b ContainerRef) int {
		if order := cmp.Compare(a.Source, b.Source); order != 0 {
			return order
		}
		return cmp.Compare(a.NativeID, b.NativeID)
	})
	canonical = slices.Compact(canonical)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("encode canonical containers: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return canonical, hex.EncodeToString(digest[:]), nil
}
