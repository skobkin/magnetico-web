package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/synth"
)

// TestWebDemoServer serves a real web binary over disposable PostgreSQL with
// synthetic torrents for manual and headless-browser review of the UI. It is
// explicit: MAGNETICO_W5_DEMO=<directory>. The test writes the base URL to
// <directory>/addr, then runs until <directory>/stop exists (or three hours
// pass). Sign in as alice / binary-browse-password-1. No network activity
// beyond the loopback listener; nothing real is stored.
func TestWebDemoServer(t *testing.T) {
	dir := os.Getenv("MAGNETICO_W5_DEMO")
	if dir == "" {
		t.Skip("explicit: set MAGNETICO_W5_DEMO=<directory>")
	}
	w, _, dsn := webBinaryFixture(t)

	ids := map[string]int{}
	add := func(key, name string, files []codec.File) {
		ids[key] = seedBrowse(t, dsn, name, files)
	}
	add("small", "Open Documentary 1080p (12 files)", smallTorrent())
	add("standard", "Midnight Garden — complete release (86 files, nested)", standardTorrent())
	add("series", "Harbor Nights — complete series, 10 seasons (2,400 files)",
		synth.Generate(synth.Spec{Shape: synth.Typical, Files: 2400, Seed: 101, AvgName: 40}))
	add("huge", "Massive Public Archive (120,000 files)",
		synth.Generate(synth.Spec{Shape: synth.Typical, Files: 120_000, Seed: 102}))
	add("longpath", "Long Path Collection (8,000 files)",
		synth.Generate(synth.Spec{Shape: synth.LongPath, Files: 8000, Seed: 103}))
	add("adversarial", "Odd Names Sampler", synth.Generate(synth.Spec{Shape: synth.Adversarial, Files: 400, Seed: 104}))
	add("toodeep", "Too Deep To Browse", []codec.File{{Path: strings.Repeat("d/", 300) + "f", Size: 1}, {Path: "other", Size: 2}})
	for i := 0; i < 70; i++ {
		add(fmt.Sprintf("filler%02d", i), fmt.Sprintf("Sample Archive %02d — %s edition", i, []string{"alpha", "beta", "gamma", "delta"}[i%4]),
			[]codec.File{{Path: "data/part1.bin", Size: int64(1+i) << 20}, {Path: "readme.txt", Size: 512}})
	}

	var out strings.Builder
	fmt.Fprintf(&out, "http://%s\n", w.addr)
	for _, k := range []string{"small", "standard", "series", "huge", "longpath", "adversarial", "toodeep"} {
		fmt.Fprintf(&out, "%s=%d\n", k, ids[k])
	}
	if err := os.WriteFile(filepath.Join(dir, "addr"), []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Hour)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

func smallTorrent() []codec.File {
	return []codec.File{
		{Path: "Video/main-feature.mkv", Size: 554_134_540}, {Path: "Video/trailer.mp4", Size: 80_063_429},
		{Path: "Extras/Subtitles/English.srt", Size: 95_011}, {Path: "Extras/Subtitles/Français.srt", Size: 90_402},
		{Path: "Extras/Subtitles/日本語.srt", Size: 92_348}, {Path: "Extras/bonus-01.jpg", Size: 1_506_961},
		{Path: "Extras/bonus-02.jpg", Size: 1_431_998}, {Path: "Extras/bonus-03.jpg", Size: 1_388_404},
		{Path: "README.txt", Size: 3_644}, {Path: "LICENSE", Size: 1_040}, {Path: "poster.png", Size: 2_204_011},
		{Path: "checksums.sha256", Size: 612},
	}
}

func standardTorrent() []codec.File {
	var fs []codec.File
	for s := 1; s <= 3; s++ {
		for e := 1; e <= 12; e++ {
			fs = append(fs, codec.File{Path: fmt.Sprintf("Season %02d/Episode %02d.mkv", s, e), Size: int64(300+e*7+s) << 20})
		}
		fs = append(fs, codec.File{Path: fmt.Sprintf("Season %02d/Subtitles/English.srt", s), Size: 91_000},
			codec.File{Path: fmt.Sprintf("Season %02d/Subtitles/Deutsch.srt", s), Size: 88_000})
	}
	fs = append(fs,
		codec.File{Path: "Extras/Artwork/cover.jpg", Size: 410_000}, codec.File{Path: "Extras/Artwork/cover.jpg", Size: 410_000},
		codec.File{Path: "Extras/Documentation/bidi‮gnp.txt", Size: 12}, codec.File{Path: "Extras/Documentation/ctl\x01char.txt", Size: 13},
		codec.File{Path: "Extras/Documentation/bad\xffbytes.txt", Size: 14}, codec.File{Path: "Extras/Documentation//empty-component.txt", Size: 15},
		codec.File{Path: "Extras/Documentation/ünïcode — имя.txt", Size: 16}, codec.File{Path: "README.txt", Size: 2_048})
	return fs
}
