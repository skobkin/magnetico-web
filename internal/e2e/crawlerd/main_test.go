package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
)

func TestBigMetaHitsTargetExactly(t *testing.T) {
	for _, target := range []int{1 << 20, 3_000_001, record.MaxRecordBytes, record.MaxRecordBytes + 4096} {
		md := bigMeta(7, target)
		r, err := record.FromMetadata(&md)
		if err != nil {
			t.Fatal(err)
		}
		data, err := record.Encode(r, codec.DefaultLimits())
		if target > record.MaxRecordBytes {
			if !errors.Is(err, record.ErrRecordTooLarge) {
				t.Fatalf("target %d: err = %v, want ErrRecordTooLarge", target, err)
			}
		} else if err != nil {
			t.Fatalf("target %d: %v", target, err)
		}
		if len(data) != target {
			t.Fatalf("target %d: got %d bytes", target, len(data))
		}
		if len(md.Files) > codec.DefaultLimits().MaxFiles {
			t.Fatalf("target %d: %d files exceeds the codec file limit", target, len(md.Files))
		}
	}
}

func TestParseSpecs(t *testing.T) {
	ph, err := parsePhases("15:3600, 150:1500")
	if err != nil || !reflect.DeepEqual(ph, []phase{{15, 3600}, {150, 1500}}) {
		t.Fatalf("phases = %v, %v", ph, err)
	}
	for _, bad := range []string{"", "15", "x:1", "1:-2", "1:2:3"} {
		if _, err := parsePhases(bad); err == nil {
			t.Fatalf("parsePhases(%q) accepted", bad)
		}
	}
	b, err := parseBig("100:2,200:1")
	if err != nil || !reflect.DeepEqual(b, []int{100, 200, 100}) {
		t.Fatalf("big = %v, %v", b, err)
	}
	if b, err := parseBig(""); err != nil || b != nil {
		t.Fatalf("empty big = %v, %v", b, err)
	}
}
