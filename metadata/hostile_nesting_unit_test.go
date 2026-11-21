package metadata

import (
	"errors"
	"testing"

	"github.com/skobkin/magnetico-web/bencode"
)

func TestUnmarshalMetainfoRejectsOverDeepNesting(t *testing.T) {
	t.Parallel()
	_, err := unmarshalMetainfo(hostilePayload("unknown-list", MaxNestingDepth-1))
	if errors.Is(err, bencode.ErrNestingTooDeep) {
		t.Errorf("total depth %d (the info dict plus %d lists) must be allowed: %v", MaxNestingDepth, MaxNestingDepth-1, err)
	}
	_, err = unmarshalMetainfo(hostilePayload("unknown-list", MaxNestingDepth))
	if !errors.Is(err, bencode.ErrNestingTooDeep) {
		t.Errorf("over-deep metadata: err = %v, want ErrNestingTooDeep", err)
	}
}
