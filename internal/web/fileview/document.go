package fileview

import (
	"context"

	"github.com/skobkin/magnetico-web/internal/web/filetree"
)

type ancestor struct{ name, path string }

const ctxCheckEvery = 2048

// encodeTree writes the file-tree v1 document:
//
//	{"version":1,"file_count":N,"size":S,"root":DIR}
//	DIR  = {["name"…,]"file_count":N,"size":S,"dirs":[DIR…],"files":[FILE…]}
//	FILE = {"name"…,"size":N,"file_index":P,"duplicate":B}
//
// dirs[i] has positional path component i; "name" is absent on the root.
func encodeTree(ctx context.Context, w *jsonBuf, idx filetree.Index) error {
	root := idx.Info(filetree.Root)
	w.raw(`{"version":1,"file_count":`)
	w.int(int64(root.Files))
	w.raw(`,"size":`)
	w.int(root.Size)
	w.raw(`,"root":`)
	n := 0
	var walk func(d filetree.DirID, named bool) error
	walk = func(d filetree.DirID, named bool) error {
		info := idx.Info(d)
		w.raw("{")
		if named {
			w.name(idx.DirName(d))
			w.raw(",")
		}
		w.raw(`"file_count":`)
		w.int(int64(info.Files))
		w.raw(`,"size":`)
		w.int(info.Size)
		w.raw(`,"dirs":[`)
		for i := 0; i < int(info.NDirs); i++ {
			if w.over {
				return nil
			}
			if i > 0 {
				w.raw(",")
			}
			c, _ := idx.Child(d, i)
			if err := walk(c, true); err != nil {
				return err
			}
		}
		w.raw(`],"files":[`)
		var buf [256]filetree.Entry
		first := true
		for off := int(info.NDirs); off < info.Total() && !w.over; {
			page := idx.Page(d, off, len(buf), buf[:0])
			for _, e := range page {
				if n++; n%ctxCheckEvery == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if !first {
					w.raw(",")
				}
				first = false
				writeFile(w, e)
			}
			off += len(page)
			if len(page) == 0 {
				break
			}
		}
		w.raw("]}")
		return nil
	}
	if err := walk(filetree.Root, false); err != nil {
		return err
	}
	w.raw("}")
	return nil
}

func writeFile(w *jsonBuf, e filetree.Entry) {
	w.raw("{")
	w.name(e.Name)
	w.raw(`,"size":`)
	w.int(e.Size)
	w.raw(`,"file_index":`)
	w.int(int64(e.Pos))
	w.raw(`,"duplicate":`)
	w.bool(e.Dup)
	w.raw("}")
}

// encodeDirPage writes the file-dir-page v1 document:
//
//	{"version":1,"path":"2_5","ancestors":[{"name"…,"path":"2"}…],
//	 "dir":{"file_count":N,"size":S,"dir_count":D,"entry_count":E},
//	 "offset":O,"limit":L,"next_offset":K|null,"entries":[ENTRY…]}
//	ENTRY = {"type":"dir","name"…,"path":P,"file_count":N,"size":S,
//	         "dir_count":D,"entry_count":E}
//	      | {"type":"file","name"…,"size":N,"file_index":P,"duplicate":B}
//
// Entries are subdirectories (by name bytes) then files (canonical order).
// next_offset is the offset of the first entry not returned, or null.
func encodeDirPage(w *jsonBuf, idx filetree.Index, d filetree.DirID, path string,
	ancestors []ancestor, info filetree.DirInfo, pg Page, entries []filetree.Entry) {
	w.raw(`{"version":1,"path":`)
	w.str(path)
	w.raw(`,"ancestors":[`)
	for i, a := range ancestors {
		if i > 0 {
			w.raw(",")
		}
		w.raw("{")
		w.name(a.name)
		w.raw(`,"path":`)
		w.str(a.path)
		w.raw("}")
	}
	w.raw(`],"dir":`)
	writeDirInfo(w, info)
	w.raw(`,"offset":`)
	w.int(int64(pg.Offset))
	w.raw(`,"limit":`)
	w.int(int64(pg.Limit))
	w.raw(`,"next_offset":`)
	if next := pg.Offset + len(entries); next < info.Total() && len(entries) > 0 {
		w.int(int64(next))
	} else {
		w.raw("null")
	}
	w.raw(`,"entries":[`)
	for i, e := range entries {
		if i > 0 {
			w.raw(",")
		}
		if !e.Dir {
			w.raw(`{"type":"file",`)
			w.name(e.Name)
			w.raw(`,"size":`)
			w.int(e.Size)
			w.raw(`,"file_index":`)
			w.int(int64(e.Pos))
			w.raw(`,"duplicate":`)
			w.bool(e.Dup)
			w.raw("}")
			continue
		}
		pos := pg.Offset + i // directories come first: row index = positional component
		child, _ := idx.Child(d, pos)
		sub := idx.Info(child)
		w.raw(`{"type":"dir",`)
		w.name(e.Name)
		w.raw(`,"path":"`)
		if path != "" {
			w.raw(path)
			w.raw("_")
		}
		w.int(int64(pos))
		w.raw(`","file_count":`)
		w.int(int64(sub.Files))
		w.raw(`,"size":`)
		w.int(sub.Size)
		w.raw(`,"dir_count":`)
		w.int(int64(sub.NDirs))
		w.raw(`,"entry_count":`)
		w.int(int64(sub.Total()))
		w.raw("}")
	}
	w.raw("]}")
}

func writeDirInfo(w *jsonBuf, info filetree.DirInfo) {
	w.raw(`{"file_count":`)
	w.int(int64(info.Files))
	w.raw(`,"size":`)
	w.int(info.Size)
	w.raw(`,"dir_count":`)
	w.int(int64(info.NDirs))
	w.raw(`,"entry_count":`)
	w.int(int64(info.Total()))
	w.raw("}")
}
