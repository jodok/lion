package voyager

import (
	"encoding/json"
	"fmt"
)

// Voyager responses use LinkedIn's "normalized" JSON: a top-level "data"
// object plus a flat "included" array of entities, each carrying an "entityUrn"
// and a "$type". References between entities are URN strings. These helpers
// index the included array by URN and by type so the typed decoders in each
// resource file can resolve references without re-walking the payload.

// Entity "$type" values used by the shape-drift guards in the resource
// decoders, which distinguish "the server returned nothing" from "the server
// returned entities this decoder could no longer reach".
const (
	typeMiniProfile = "com.linkedin.voyager.identity.shared.MiniProfile"
	typeUpdateV2    = "com.linkedin.voyager.feed.render.UpdateV2"
)

// normalized is the envelope shape shared by most Voyager responses.
type normalized struct {
	Data     json.RawMessage   `json:"data"`
	Included []json.RawMessage `json:"included"`
}

// entityIndex indexes included entities by URN and groups them by $type.
type entityIndex struct {
	byURN  map[string]json.RawMessage
	byType map[string][]json.RawMessage
}

// parseNormalized parses a Voyager envelope and builds an index over its
// included entities.
func parseNormalized(body []byte) (*normalized, *entityIndex, error) {
	var n normalized
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, nil, fmt.Errorf("decode voyager envelope: %w", err)
	}
	idx := &entityIndex{
		byURN:  make(map[string]json.RawMessage, len(n.Included)),
		byType: make(map[string][]json.RawMessage),
	}
	for _, raw := range n.Included {
		var meta struct {
			URN  string `json:"entityUrn"`
			Type string `json:"$type"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		if meta.URN != "" {
			idx.byURN[meta.URN] = raw
		}
		if meta.Type != "" {
			idx.byType[meta.Type] = append(idx.byType[meta.Type], raw)
		}
	}
	return &n, idx, nil
}

// get returns the raw entity for a URN, or false if absent.
func (idx *entityIndex) get(urn string) (json.RawMessage, bool) {
	r, ok := idx.byURN[urn]
	return r, ok
}

// ofType returns all included entities whose $type matches.
func (idx *entityIndex) ofType(t string) []json.RawMessage {
	return idx.byType[t]
}

// decodeInto unmarshals a raw entity into dst.
func decodeInto(raw json.RawMessage, dst any) error {
	return json.Unmarshal(raw, dst)
}
