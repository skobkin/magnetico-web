// Package devseed generates the deterministic synthetic torrent dataset used
// for manual testing of the web UI (`magnetico dev seed-torrents`) and writes
// it through the production persistence API.
//
// Generating the dataset needs no network at all: infohashes are derived from
// fixed keys, every name, path, size and timestamp comes from a fixed-seed
// generator, and no torrent metadata is ever downloaded. (Writing it needs
// PostgreSQL access only; see Seed.) The dataset is bit-for-bit stable for a
// given Version; any change to the generator must bump Version.
package devseed

import (
	"crypto/sha1" //nolint:gosec // infohash derivation only; SHA-1 is the v1 infohash function
	"fmt"
	"strings"

	"github.com/skobkin/magnetico-web/persistence"
)

// Version identifies the dataset generator. It is part of every infohash
// key, so changing the dataset requires a new version (and new infohashes).
const Version = "magnetico-dev-fixtures-v1"

// Dataset shape (the task's default dataset).
const (
	SingleFileTorrents = 40
	MultiFileTorrents  = 60
	LargeTorrentFiles  = 10_000
	TotalTorrents      = SingleFileTorrents + MultiFileTorrents + 1

	multiMinFiles = 10
	multiMaxFiles = 100
)

// Kind classifies a fixture for reports and tests.
type Kind string

const (
	KindSingle Kind = "single"
	KindMulti  Kind = "multi"
	KindLarge  Kind = "large"
)

// Fixture is one synthetic torrent: its identity key, the metadata and the
// file list exactly as they are handed to persistence.Store.AddTorrent.
type Fixture struct {
	Key     string
	Kind    Kind
	Torrent persistence.Torrent
	Files   []persistence.File
}

// InfoHash derives the stable 20-byte infohash of a fixture key. The key
// carries the dataset Version, so a different dataset never reuses a hash.
func InfoHash(key string) []byte {
	sum := sha1.Sum([]byte(Version + "/" + key)) //nolint:gosec // see import
	return sum[:]
}

// rng is SplitMix64: a tiny generator whose output sequence is fixed by its
// algorithm, independent of the Go version (math/rand sequences are not).
type rng struct{ s uint64 }

func (r *rng) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// intn returns a value in [0, n).
func (r *rng) intn(n int) int { return int(r.next() % uint64(n)) }

// between returns a value in [lo, hi].
func (r *rng) between(lo, hi int) int { return lo + r.intn(hi-lo+1) }

func (r *rng) pick(list []string) string { return list[r.intn(len(list))] }

// Discovery timestamps fall between these Unix times (2019-01-01 and
// 2026-11-01 UTC).
const (
	discoveredMin = 1_546_300_800
	discoveredMax = 1_793_491_200
)

// Dataset returns the full deterministic dataset in a stable order: the
// single-file torrents, then the multi-file torrents, then the large one.
// Every call returns freshly allocated values.
func Dataset() []Fixture {
	r := &rng{s: 0x6d61676e65746963} // "magnetic"
	out := make([]Fixture, 0, TotalTorrents)

	var lastTS int64
	timestamp := func(i int) int64 {
		// Every seventh torrent shares its predecessor's timestamp, so
		// ordering ties (broken by id) are exercised.
		if i%7 == 6 && lastTS != 0 {
			return lastTS
		}
		lastTS = int64(discoveredMin + r.intn(discoveredMax-discoveredMin))
		return lastTS
	}

	for i := 0; i < SingleFileTorrents; i++ {
		out = append(out, singleFixture(r, i, timestamp(len(out))))
	}
	for i := 0; i < MultiFileTorrents; i++ {
		out = append(out, multiFixture(r, i, timestamp(len(out))))
	}
	out = append(out, largeFixture(r))
	return out
}

func finish(key string, kind Kind, name string, ts int64, files []persistence.File) Fixture {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return Fixture{
		Key:  key,
		Kind: kind,
		Torrent: persistence.Torrent{
			InfoHash:     InfoHash(key),
			Name:         name,
			TotalSize:    total,
			DiscoveredOn: ts,
			FileCount:    int32(len(files)),
		},
		Files: files,
	}
}

// ---- names ----------------------------------------------------------------

var (
	asciiNames = []string{
		"Ubuntu 24.04 Desktop amd64", "Public Domain Film Archive 1920s", "Open Textbook Collection",
		"Sample Raw Photos Pack", "Field Recordings Vol", "Linux Kernel Source Mirror",
		"Retro Game Soundtracks", "Space Probe Image Gallery", "Creative Commons Podcast Season",
		"Open Source Fonts Bundle", "Chess Tournament Games", "Mountain Hiking Documentary",
	}
	cyrillicNames = []string{
		"Классическая музыка — сборник", "Русские народные сказки", "Документальный фильм о тайге",
		"Лекции по математике", "Старые советские мультфильмы", "Фотографии Байкала",
		"Аудиокниги: Чехов", "Энциклопедия растений",
	}
	japaneseNames = []string{
		"日本の民話 全集", "桜の写真集", "クラシック音楽コレクション", "アニメ主題歌ベスト",
		"京都の旅 ドキュメンタリー", "俳句と短歌 朗読", "プログラミング入門 講義", "星空タイムラプス",
	}
	// specialNames exercise escaping and unusual-but-valid names.
	specialNames = []string{
		`Tom & Jerry <Collection> "Remastered"`, "100% Legal [Sample] (2021) #1?", "emoji 🎬🎵 mix ✓",
		"a'b\"c;d:e|f", "Ünïcödé Nàmes — ñandú", "  padded name  ", "dots...and...more..",
	}
)

// nameFor builds the display name of fixture i; the numeric suffix keeps
// every name distinct and searchable by number.
func nameFor(r *rng, group string, i int) string {
	var base string
	switch i % 4 {
	case 0:
		base = r.pick(asciiNames)
	case 1:
		base = r.pick(cyrillicNames)
	case 2:
		base = r.pick(japaneseNames)
	default:
		base = r.pick(specialNames)
	}
	return fmt.Sprintf("%s %s-%03d", base, group, i)
}

// longName is a deliberately long (but valid) torrent name.
func longName(i int) string {
	return fmt.Sprintf("Long name %03d %s", i, strings.Repeat("segment-абв-あいう ", 18))
}

// ---- single-file torrents --------------------------------------------------

var extensions = []string{".mkv", ".iso", ".flac", ".pdf", ".zip", ".mp4", ".epub", ".tar.xz"}

func singleFixture(r *rng, i int, ts int64) Fixture {
	key := fmt.Sprintf("single/%03d", i)
	name := nameFor(r, "single", i)
	if i == 5 {
		name = longName(i)
	}

	// Sizes are spread over twelve orders of magnitude; the first and last
	// fixtures pin the extremes (1 byte, ~3 TiB).
	var size int64
	switch i {
	case 0:
		size = 1
	case SingleFileTorrents - 1:
		size = 3 << 40
	default:
		exp := r.between(1, 12)
		size = int64(1+r.intn(999)) * pow10(exp-1)
	}

	path := name + extensions[r.intn(len(extensions))]
	switch i % 10 {
	case 3:
		// Atypical single file: its path is not the torrent name, so the
		// store keeps a file-list blob for it (no-blob eligibility needs
		// path == name).
		path = "Samples/readme-" + fmt.Sprintf("%03d", i) + ".txt"
	case 7:
		path = name // the intentional no-blob shape
	}
	return finish(key, KindSingle, name, ts, []persistence.File{{Path: path, Size: size}})
}

func pow10(n int) int64 {
	v := int64(1)
	for ; n > 0; n-- {
		v *= 10
	}
	return v
}

// ---- multi-file torrents ---------------------------------------------------

const layoutCount = 10

// multiFixture builds multi-file torrent i (0..59) with 10-100 files. The
// layout cycles through layoutCount shapes, so each is present six times.
func multiFixture(r *rng, i int, ts int64) Fixture {
	key := fmt.Sprintf("multi/%03d", i)
	layout, rep := i%layoutCount, i/layoutCount
	name := nameFor(r, "multi", i)
	if layout == 6 {
		name = longName(i)
	}
	n := r.between(multiMinFiles, multiMaxFiles)
	// Pin the bounds so the 10..100 range is exercised exactly.
	switch i {
	case 0:
		n = multiMinFiles
	case 1:
		n = multiMaxFiles
	}

	g := &builder{r: r, root: name, n: n}
	switch layout {
	case 0:
		g.flat()
	case 1:
		g.seasons()
	case 2:
		g.deepChain()
	case 3:
		g.wideFan()
	case 4:
		g.unicodeNames()
	case 5:
		g.specialChars()
	case 6:
		g.longComponents()
	case 7:
		g.duplicates()
	case 8:
		g.collisions(rep == 0)
	default:
		g.zeroAndMixed()
	}
	return finish(key, KindMulti, name, ts, g.finish())
}

// builder accumulates exactly n files under a root directory.
type builder struct {
	r     *rng
	root  string
	n     int
	files []persistence.File
}

// add appends a file at path (verbatim) with a generated size.
func (b *builder) add(path string) { b.addSized(path, b.size()) }

func (b *builder) addSized(path string, size int64) {
	if len(b.files) < b.n {
		b.files = append(b.files, persistence.File{Path: path, Size: size})
	}
}

func (b *builder) size() int64 { return int64(1+b.r.intn(9_999)) * pow10(b.r.intn(6)) }

func (b *builder) full() bool { return len(b.files) >= b.n }

// finish pads to exactly n files (a flat tail directory) and guarantees a
// positive total, which the store requires.
func (b *builder) finish() []persistence.File {
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/extra/file-%03d.bin", b.root, i))
	}
	var total int64
	for _, f := range b.files {
		total += f.Size
	}
	if total == 0 {
		b.files[0].Size = 1
	}
	return b.files
}

func (b *builder) flat() {
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/track-%03d.flac", b.root, i))
	}
}

func (b *builder) seasons() {
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/Season %02d/Episode %02d.mkv", b.root, i/12+1, i%12+1))
	}
}

func (b *builder) deepChain() {
	depth := b.r.between(12, 20)
	dir := b.root
	for d := 0; d < depth; d++ {
		dir += fmt.Sprintf("/level-%02d", d)
		b.add(dir + "/note.txt")
	}
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/leaf-%03d.dat", dir, i))
	}
}

func (b *builder) wideFan() {
	for i := 0; !b.full(); i++ {
		dirs := b.r.between(1, 3)
		for j := 0; j < dirs; j++ {
			b.add(fmt.Sprintf("%s/dir-%03d/item-%d.jpg", b.root, i, j))
		}
	}
}

func (b *builder) unicodeNames() {
	names := []string{
		"Глава первая.txt", "第二章 — 桜.txt", "ファイル名.mp3", "naïve café.pdf",
		"école.txt", "🎵 song.mp3", "עברית.txt", "العربية.txt", "ελληνικά.txt",
	}
	dirs := []string{"Музыка", "写真", "Документы", "文書", "plain"}
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/%s/%02d %s", b.root, dirs[i%len(dirs)], i, names[i%len(names)]))
	}
}

func (b *builder) specialChars() {
	names := []string{
		"a file with spaces.txt", "hash#tag.txt", "question?mark.txt", "percent%20encoded.txt",
		"amp&ersand.txt", "[brackets].txt", "(parens).txt", "quote'single.txt", `quote"double.txt`,
		"semi;colon.txt", "colon:name.txt", "pipe|name.txt", "<angle>.txt", ".hidden", "trailing space ",
		"back\\slash.txt", "tab\tname.txt", "plus+sign.txt", "equals=sign.txt", "tilde~name.txt",
	}
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/special %d/%s", b.root, i/len(names), names[i%len(names)]))
	}
}

func (b *builder) longComponents() {
	// A 240-byte component stays below the usual 255-byte filesystem limit;
	// the chained path is well over 1 KiB.
	long := strings.Repeat("longname-", 26) + "end" // 237 bytes
	dir := b.root
	for d := 0; d < 4; d++ {
		dir += "/" + fmt.Sprintf("%02d-", d) + long
		b.add(dir + "/" + long + ".txt")
	}
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/%s-%03d.bin", dir, long, i))
	}
}

func (b *builder) duplicates() {
	p := b.root + "/D/a.txt"
	// Repeated (path, size) pairs and the same path with different sizes
	// are all valid and preserved (wire-format-v1 §2 and §10 case D).
	b.addSized(p, 0)
	b.addSized(p, 0)
	b.addSized(p, 7)
	b.addSized(p, 7)
	b.addSized(b.root+"/D/b.txt", 100)
	b.addSized(b.root+"/D/b.txt", 100)
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/D/c-%03d.txt", b.root, i%5)) // repeats names with distinct sizes
	}
}

// collisions covers file/directory name clashes, literal "." and ".."
// components and, once (withSeparators), the path shapes the trie cannot
// represent (leading, trailing and doubled separators), which force the
// encoder onto its flat or prefix fallback without altering any path.
func (b *builder) collisions(withSeparators bool) {
	b.add(b.root + "/a")   // a file ...
	b.add(b.root + "/a/b") // ... and a directory with the same name
	b.add(b.root + "/a/b/c")
	b.add(b.root + "/./dot.txt")
	b.add(b.root + "/../dotdot.txt")
	b.add(b.root + "/dir/../x.txt")
	if withSeparators {
		b.add("/absolute/leading-slash.txt")
		b.add(b.root + "/trailing-slash/")
		b.add(b.root + "//double//slash.txt")
	}
	for i := 0; !b.full(); i++ {
		b.add(fmt.Sprintf("%s/a/file-%03d.txt", b.root, i))
	}
}

func (b *builder) zeroAndMixed() {
	for i := 0; !b.full(); i++ {
		dir := b.root
		if i%3 != 0 {
			dir += fmt.Sprintf("/mixed-%d", i%4)
		}
		if i%4 == 0 {
			b.addSized(fmt.Sprintf("%s/empty-%03d.marker", dir, i), 0)
		} else {
			b.add(fmt.Sprintf("%s/data-%03d.bin", dir, i))
		}
	}
}

// ---- the large torrent -----------------------------------------------------

// LargeKey is the stable key of the 10,000-file torrent.
const LargeKey = "large/10000"

// largeFixture has exactly LargeTorrentFiles files in three regions:
//
//   - 80 directories of 100 files each (8,000 files);
//   - one flat directory of 1,500 files (several pages at the page-size cap);
//   - 500 files spread over a 6-level nested hierarchy.
//
// 10,000 files exceed the full-tree file-count limit, so the web UI serves it in
// the paginated directory mode. Whether a smaller torrent gets the full tree
// also depends on the estimate and serialized-size limits (fileview.Decide),
// not on its file count alone.
func largeFixture(r *rng) Fixture {
	const root = "Large archive 10000 files — Большой архив — 大規模アーカイブ"
	files := make([]persistence.File, 0, LargeTorrentFiles)
	add := func(path string) {
		files = append(files, persistence.File{Path: path, Size: int64(1+r.intn(999)) * pow10(r.intn(7))})
	}
	for d := 0; d < 80; d++ {
		for f := 0; f < 100; f++ {
			add(fmt.Sprintf("%s/part-%03d/file-%03d.dat", root, d, f))
		}
	}
	for f := 0; f < 1500; f++ {
		add(fmt.Sprintf("%s/flat-dump/entry-%04d.txt", root, f))
	}
	for f := 0; f < 500; f++ {
		add(fmt.Sprintf("%s/nested/n%d/n%d/n%d/n%d/n%d/leaf-%03d.bin", root, f%2, f%3, f%5, f%7, f%4, f))
	}
	return finish(LargeKey, KindLarge, root, 1_792_000_000, files)
}
