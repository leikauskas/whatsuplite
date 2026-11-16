package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// The media cache is disposable: a removed file downloads again from WhatsApp, or from the phone once
// WhatsApp's servers have dropped it. So it stays under a cap by removing the files used longest ago,
// trimming well below the cap so this doesn't run on every download.
const (
	mediaCacheMax  = 2 << 30
	mediaCacheTrim = 3 << 29
)

var pruning atomic.Bool

// expireLoop removes disappearing messages once their time is up, as the phone does.
func (a *App) expireLoop() {
	for {
		if chats := a.db.DeleteExpired(time.Now().Unix()); len(chats) > 0 {
			for _, c := range chats {
				a.db.RefreshPreview(c)
			}
			a.hub.Send(map[string]any{"t": "expired", "chats": chats})
			a.hub.Send(map[string]any{"t": "chats"})
		}
		time.Sleep(5 * time.Minute)
	}
}

func (a *App) pruneMedia() {
	if !pruning.CompareAndSwap(false, true) {
		return
	}
	defer pruning.Store(false)
	if n, freed := pruneDir(a.mediaDir, mediaCacheMax, mediaCacheTrim); n > 0 {
		logf("media cache: removed %d files, %d MB", n, freed>>20)
	}
}

// pruneDir removes the least recently used files under dir once it holds more than max bytes,
// until it holds at most trim. A file's modification time is its last use (mediaFile refreshes it).
func pruneDir(dir string, max, trim int64) (removed int, freed int64) {
	type file struct {
		path string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".part") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, file{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if total <= max {
		return 0, 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= trim {
			break
		}
		if os.Remove(f.path) != nil {
			continue // open in another program
		}
		total -= f.size
		freed += f.size
		removed++
		if parent := filepath.Dir(f.path); parent != dir {
			os.Remove(parent) // a document's own folder, removed only once empty
		}
	}
	return removed, freed
}
