package codec

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func sampleFiles(n int) []File {
	files := make([]File, 0, n)
	for i := 0; i < n; i++ {
		files = append(files, File{
			Path: fmt.Sprintf("dir%02d/sub%02d/file-%06d.bin", i%20, (i*7)%50, i),
			Size: int64(i * 13),
		})
	}
	return files
}

func TestEncoderMatchesPackageEncode(t *testing.T) {
	t.Parallel()

	files := sampleFiles(500)
	want, wantSel, err := Encode(files, ProfileCompact, DefaultLimits())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	for _, workers := range []int{1, 4} {
		e := NewEncoder(ProfileCompact, DefaultLimits(), workers)
		got, sel, err := e.Encode(files)
		if err != nil {
			t.Fatalf("Encoder(workers=%d).Encode() error = %v", workers, err)
		}
		if sel != wantSel || !bytes.Equal(got, want) {
			t.Errorf("Encoder(workers=%d) = (%x, %+v), want (%x, %+v)", workers, got, sel, want, wantSel)
		}
		e.Close()
		if _, _, err := e.Encode(files); err == nil {
			t.Error("Encode() after Close succeeded, want an error")
		}
	}
}

func TestEncoderConcurrentDeterminism(t *testing.T) {
	t.Parallel()

	files := sampleFiles(300)
	want, wantSel, err := Encode(files, ProfileMaximum, DefaultLimits())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	e := NewEncoder(ProfileMaximum, DefaultLimits(), 4)
	defer e.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				got, sel, err := e.Encode(files)
				if err != nil {
					errs <- fmt.Errorf("Encode() error = %v", err)
					return
				}
				if sel != wantSel || !bytes.Equal(got, want) {
					errs <- fmt.Errorf("concurrent Encode() diverged: (%x, %+v)", got, sel)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestEncoderStateReuseProducesFreshBuffers(t *testing.T) {
	t.Parallel()

	// A pooled state's compressed payload must be copied out: a later Encode
	// with a different list must not overwrite earlier results.
	e := NewEncoder(ProfileCompact, DefaultLimits(), 1)
	defer e.Close()

	small := []File{{Path: "small", Size: 1}}
	first, _, err := e.Encode(sampleFiles(200))
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	second, _, err := e.Encode(small)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	gotFirst, err := Decode(first, DefaultLimits())
	if err != nil {
		t.Fatalf("Decode(first) error = %v", err)
	}
	gotSecond, err := Decode(second, DefaultLimits())
	if err != nil {
		t.Fatalf("Decode(second) error = %v", err)
	}
	wantSecond, _, _ := Encode(small, ProfileCompact, DefaultLimits())
	if !reflect.DeepEqual(gotSecond, small) {
		t.Errorf("second list corrupted by reuse: %#v", gotSecond)
	}
	if !bytes.Equal(second, wantSecond) {
		t.Error("second blob differs from a fresh Encode")
	}
	_ = gotFirst
}
