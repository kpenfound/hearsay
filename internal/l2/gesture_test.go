package l2_test

import (
	"errors"
	"testing"

	"github.com/kpenfound/hearsay/internal/l2"
)

func TestGestureRequestValidate(t *testing.T) {
	const event, earlier = "evt:discord:reaction-2", "evt:discord:reaction-1"
	docs := []string{"l1:discord:thread"}
	tests := []struct {
		name string
		req  l2.GestureRequest
		ok   bool
	}{
		{name: "a ratify", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureRatify, Documents: docs}, ok: true},
		{name: "a demote", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureDemote, Documents: docs}, ok: true},
		{name: "a pin", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GesturePin, Documents: docs}, ok: true},
		{name: "an undo", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureUndo, Undoes: earlier}, ok: true},
		{name: "no event", req: l2.GestureRequest{Principal: "kyle", Action: l2.GestureRatify, Documents: docs}},
		{name: "an event that is not an event id", req: l2.GestureRequest{Event: "reaction-2", Principal: "kyle", Action: l2.GestureRatify, Documents: docs}},
		{name: "an unknown action", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: "merge", Documents: docs}},
		{name: "a ratify of nothing", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureRatify}},
		{name: "a pin of something that is no document", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GesturePin, Documents: []string{"topic:x"}}},
		{name: "a ratify that undoes", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureRatify, Documents: docs, Undoes: earlier}},
		{name: "an undo of nothing", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureUndo}},
		{name: "an undo of itself", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureUndo, Undoes: event}},
		{name: "an undo naming documents", req: l2.GestureRequest{Event: event, Principal: "kyle", Action: l2.GestureUndo, Undoes: earlier, Documents: docs}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, l2.ErrInvalid)) {
				t.Errorf("Validate() = %v, want ok %v", err, tt.ok)
			}
		})
	}
}
