package filetree

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

func deepPath(components int) string {
	return strings.Repeat("d/", components) + "f"
}

func TestComplexityDepthBoundary(t *testing.T) {
	t.Parallel()
	lim := Limits{MaxDirs: 1_000_000, MaxDepth: 256}
	ok := []codec.File{{Path: deepPath(256), Size: 1}}
	if _, err := BuildArenaLimits(context.Background(), ok, lim); err != nil {
		t.Fatalf("depth at the limit must build: %v", err)
	}
	bad := []codec.File{{Path: deepPath(257), Size: 1}}
	_, err := BuildArenaLimits(context.Background(), bad, lim)
	var ce *ComplexityError
	if !errors.As(err, &ce) || ce.Limit != LimitDepth || !errors.Is(err, ErrIndexComplexity) {
		t.Fatalf("want depth ComplexityError, got %v", err)
	}
}

func TestComplexityDirsBoundary(t *testing.T) {
	t.Parallel()
	lim := Limits{MaxDirs: 100, MaxDepth: 256}
	mk := func(n int) []codec.File {
		files := make([]codec.File, 0, n)
		for i := 0; i < n; i++ {
			files = append(files, codec.File{Path: "d" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "/f", Size: 1})
		}
		return canon(files)
	}
	if _, err := BuildArenaLimits(context.Background(), mk(100), lim); err != nil {
		t.Fatalf("exactly MaxDirs must build: %v", err)
	}
	_, err := BuildArenaLimits(context.Background(), mk(101), lim)
	var ce *ComplexityError
	if !errors.As(err, &ce) || ce.Limit != LimitDirs {
		t.Fatalf("want dirs ComplexityError, got %v", err)
	}
}

// A single codec-valid path with tens of thousands of components must be
// rejected at the depth limit, before the index grows.
func TestComplexityAdversarialRejectedEarly(t *testing.T) {
	var before, after runtime.MemStats
	files := []codec.File{{Path: strings.Repeat("a/", 32_000) + "f", Size: 1}}
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := BuildArena(context.Background(), files)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrIndexComplexity) {
		t.Fatalf("want ErrIndexComplexity, got %v", err)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
		t.Fatalf("rejection allocated %d bytes; want < 1 MiB", grown)
	}
	t.Logf("rejection allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
}

// Many files each opening its own unique deep chain: small file count, huge
// directory count.
func TestComplexityFanOutChains(t *testing.T) {
	t.Parallel()
	var files []codec.File
	for i := 0; i < 2000; i++ {
		files = append(files, codec.File{Path: "r" + string(rune(0x100+i)) + "/" + strings.Repeat("x/", 200) + "f", Size: 1})
	}
	files = canon(files)
	_, err := BuildArena(context.Background(), files) // 2000*201 > 250k dirs
	var ce *ComplexityError
	if !errors.As(err, &ce) || ce.Limit != LimitDirs {
		t.Fatalf("want dirs ComplexityError, got %v", err)
	}
}

func TestChildAndDirName(t *testing.T) {
	t.Parallel()
	files := canon([]codec.File{
		{Path: "b/x", Size: 1}, {Path: "a/y", Size: 2}, {Path: "a/y", Size: 2},
		{Path: "/e", Size: 3}, {Path: "a/\xff/z", Size: 4}, {Path: "top", Size: 5},
	})
	a, err := BuildArena(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	// Root subdirectories sorted by name bytes: "", "a", "b".
	want := []string{"", "a", "b"}
	for i, w := range want {
		d, ok := a.Child(Root, i)
		if !ok || a.DirName(d) != w {
			t.Fatalf("child %d = %q ok=%v, want %q", i, a.DirName(d), ok, w)
		}
	}
	if _, ok := a.Child(Root, 3); ok {
		t.Fatal("index == NDirs must not resolve")
	}
	if _, ok := a.Child(Root, -1); ok {
		t.Fatal("negative index must not resolve")
	}
	da, _ := a.Child(Root, 1)
	if sub, ok := a.Child(da, 0); !ok || a.DirName(sub) != "\xff" {
		t.Fatalf("invalid-UTF-8 directory lost: %q", a.DirName(sub))
	}
	if a.DirName(Root) != "" {
		t.Fatal("root name must be empty")
	}
}
