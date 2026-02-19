package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// pathLocker provides per-path mutual exclusion.
type pathLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newPathLocker() *pathLocker {
	return &pathLocker{locks: make(map[string]*sync.Mutex)}
}

func (pl *pathLocker) Lock(path string) {
	pl.mu.Lock()
	l, ok := pl.locks[path]
	if !ok {
		l = &sync.Mutex{}
		pl.locks[path] = l
	}
	pl.mu.Unlock()
	l.Lock()
}

func (pl *pathLocker) Unlock(path string) {
	pl.mu.Lock()
	l, ok := pl.locks[path]
	if !ok {
		pl.mu.Unlock()
		return
	}
	delete(pl.locks, path)
	pl.mu.Unlock()
	l.Unlock()
}

// debouncer coalesces rapid event bursts into a single callback per file.
type debouncer struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
	delay  time.Duration
	onFire func(path string)
}

func newDebouncer(delay time.Duration, onFire func(path string)) *debouncer {
	return &debouncer{
		timers: make(map[string]*time.Timer),
		delay:  delay,
		onFire: onFire,
	}
}

func (d *debouncer) trigger(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[path]; ok {
		t.Reset(d.delay)
		return
	}
	d.timers[path] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		delete(d.timers, path)
		d.mu.Unlock()
		d.onFire(path)
	})
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for path, t := range d.timers {
		t.Stop()
		delete(d.timers, path)
	}
}

func runWatchMode(cfg *Config, noBg bool) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer w.Close()

	for _, dir := range cfg.Watch.InputDirs() {
		if err := watchRecursive(w, dir); err != nil {
			return fmt.Errorf("watching %s: %w", dir, err)
		}
		fmt.Printf("Watching: %s\n", dir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()

	outLock := newPathLocker()

	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup

	db := newDebouncer(500*time.Millisecond, func(path string) {
		j := classifyEvent(path, cfg)
		if j == nil {
			return
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			outLock.Lock(j.output)
			defer outLock.Unlock(j.output)
			if recheck := classifyEvent(path, cfg); recheck == nil {
				return
			}
			convertJob(*j, noBg, cfg)
		}()
	})
	defer db.stop()

	initialScan(cfg, noBg, outLock)

	fmt.Println("Daemon ready. Waiting for file changes...")

	// Polling fallback for network/virtual filesystems where kqueue doesn't fire
	go pollLoop(ctx, cfg, cfg.Watch.PollDuration(), func(path string) {
		db.trigger(path)
	}, func(path string) {
		handleDeletion(path, cfg)
	})

	eventLoop(ctx, w, db, cfg)

	fmt.Println("Waiting for in-flight conversions...")
	wg.Wait()
	fmt.Println("Shutdown complete.")
	return nil
}

func watchRecursive(w *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}

// initialScan processes stale files in watched directories.
// Jobs are deduplicated by output path to prevent concurrent writes.
func initialScan(cfg *Config, noBg bool, outLock *pathLocker) {
	syncOrphanedOutputs(cfg)

	jobs := make(map[string]convJob)

	for _, dir := range cfg.Watch.InputDirs() {
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".note") && !strings.HasSuffix(path, ".mark") {
				return nil
			}
			if j := classifyEvent(path, cfg); j != nil {
				jobs[j.output] = *j
			}
			return nil
		})
	}

	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			outLock.Lock(j.output)
			defer outLock.Unlock(j.output)
			convertJob(j, noBg, cfg)
		}()
	}
	wg.Wait()
}

func eventLoop(ctx context.Context, w *fsnotify.Watcher, db *debouncer, cfg *Config) {
	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Remove) {
				if strings.HasSuffix(ev.Name, ".note") || strings.HasSuffix(ev.Name, ".mark") {
					handleDeletion(ev.Name, cfg)
				}
				continue
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					watchRecursive(w, ev.Name)
					continue
				}
			}
			// Atomic file replacement (common on macOS/kqueue): verify the
			// renamed path still exists and re-add parent for inode tracking.
			if ev.Has(fsnotify.Rename) {
				if _, err := os.Stat(ev.Name); err != nil {
					continue
				}
				w.Add(filepath.Dir(ev.Name))
			}
			db.trigger(ev.Name)

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "Watcher error: %v\n", err)
		}
	}
}

// pollLoop walks input directories at a fixed interval to detect mtime changes
// on network/virtual filesystems (WebDAV, Supernote Private Cloud).
func pollLoop(ctx context.Context, cfg *Config, interval time.Duration, onChanged func(path string), onDeleted func(path string)) {
	mtimes := make(map[string]time.Time)
	prevSources := make(map[string]bool)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		seen := make(map[string]bool)
		sources := make(map[string]bool)
		for _, dir := range cfg.Watch.InputDirs() {
			filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				ext := strings.ToLower(filepath.Ext(path))
				if ext != ".note" && ext != ".mark" && ext != ".pdf" {
					return nil
				}
				seen[path] = true
				if ext == ".note" || ext == ".mark" {
					sources[path] = true
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				mt := info.ModTime()
				if prev, ok := mtimes[path]; !ok || !mt.Equal(prev) {
					mtimes[path] = mt
					onChanged(path)
				}
				return nil
			})
		}

		for path := range prevSources {
			if !sources[path] {
				onDeleted(path)
			}
		}
		prevSources = sources

		for path := range sources {
			out := outputPathForSource(path, cfg)
			if out == "" {
				continue
			}
			if _, err := os.Stat(out); err != nil {
				onChanged(path)
			}
		}

		for path := range mtimes {
			if !seen[path] {
				delete(mtimes, path)
			}
		}
	}
}

func classifyEvent(path string, cfg *Config) *convJob {
	srcDir := sourceDir(path, cfg)
	if srcDir == "" {
		return nil
	}
	outDir := cfg.Watch.Location

	switch {
	case strings.HasSuffix(path, ".note"):
		out := outputPath(path, srcDir, outDir, ".note", ".pdf")
		if isUpToDate(path, out) {
			return nil
		}
		return &convJob{input: path, output: out}

	case strings.HasSuffix(path, ".mark"):
		companionPDF := strings.TrimSuffix(path, ".mark")
		if _, err := os.Stat(companionPDF); err != nil {
			fmt.Printf("Skipping '%s': companion PDF not found (will retry when PDF arrives)\n", filepath.Base(path))
			return nil
		}
		out := outputPath(path, srcDir, outDir, ".mark", "")
		if isMarkUpToDate(path, companionPDF, out) {
			return nil
		}
		return &convJob{input: path, output: out, companionPDF: companionPDF}

	// .pdf arriving — retry for late-arriving companion PDFs
	case strings.HasSuffix(path, ".pdf"):
		markPath := path + ".mark"
		if _, err := os.Stat(markPath); err != nil {
			return nil
		}
		out := outputPath(markPath, srcDir, outDir, ".mark", "")
		if isMarkUpToDate(markPath, path, out) {
			return nil
		}
		return &convJob{input: markPath, output: out, companionPDF: path}

	default:
		return nil
	}
}

func convertJob(j convJob, noBg bool, cfg *Config) {
	if dir := filepath.Dir(j.output); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating directory '%s': %v\n", dir, err)
			return
		}
	}

	start := time.Now()
	var err error
	if j.companionPDF != "" {
		err = ConvertMarkToPDFVector(j.input, j.companionPDF, j.output, false, cfg)
	} else {
		err = ConvertNoteToPDFVector(j.input, j.output, noBg, false, cfg)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error converting '%s': %v\n", j.input, err)
		return
	}
	fmt.Printf("Converted '%s' -> '%s' (%.2fs)\n", filepath.Base(j.input), filepath.Base(j.output), time.Since(start).Seconds())
}

func sourceDir(path string, cfg *Config) string {
	for _, dir := range cfg.Watch.InputDirs() {
		if isUnderDir(path, dir) {
			return dir
		}
	}
	return ""
}

func outputPath(path, srcDir, outDir, oldExt, newExt string) string {
	rel, _ := filepath.Rel(srcDir, path)
	return filepath.Join(outDir, strings.TrimSuffix(rel, oldExt)+newExt)
}

func isUnderDir(path, dir string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	return strings.HasPrefix(absPath, absDir+string(filepath.Separator)) || absPath == absDir
}

func outputPathForSource(path string, cfg *Config) string {
	srcDir := sourceDir(path, cfg)
	if srcDir == "" {
		return ""
	}
	outDir := cfg.Watch.Location
	switch {
	case strings.HasSuffix(path, ".note"):
		return outputPath(path, srcDir, outDir, ".note", ".pdf")
	case strings.HasSuffix(path, ".mark"):
		return outputPath(path, srcDir, outDir, ".mark", "")
	default:
		return ""
	}
}

// handleDeletion removes the output PDF for a deleted source file
// and cleans up empty parent directories up to the output root.
func handleDeletion(path string, cfg *Config) {
	out := outputPathForSource(path, cfg)
	if out == "" {
		return
	}
	if _, err := os.Stat(out); err != nil {
		return
	}
	if err := os.Remove(out); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing output '%s': %v\n", out, err)
		return
	}
	fmt.Printf("Removed output '%s' (source deleted)\n", filepath.Base(out))
	removeEmptyParents(filepath.Dir(out), cfg.Watch.Location)
}

func removeEmptyParents(dir, stopDir string) {
	absStop, err := filepath.Abs(stopDir)
	if err != nil {
		return
	}
	for {
		absDir, err := filepath.Abs(dir)
		if err != nil || absDir == absStop {
			return
		}
		if !strings.HasPrefix(absDir, absStop+string(filepath.Separator)) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func syncOrphanedOutputs(cfg *Config) {
	outDir := cfg.Watch.Location
	if outDir == "" {
		return
	}
	filepath.WalkDir(outDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".pdf") {
			return nil
		}
		if !hasSourceFile(path, cfg) {
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing orphaned output '%s': %v\n", path, err)
			} else {
				fmt.Printf("Removed orphaned output '%s'\n", filepath.Base(path))
				removeEmptyParents(filepath.Dir(path), outDir)
			}
		}
		return nil
	})
}

func hasSourceFile(outputPDF string, cfg *Config) bool {
	outDir := cfg.Watch.Location
	rel, err := filepath.Rel(outDir, outputPDF)
	if err != nil {
		return false
	}
	for _, dir := range cfg.Watch.InputDirs() {
		noteSource := filepath.Join(dir, strings.TrimSuffix(rel, ".pdf")+".note")
		if _, err := os.Stat(noteSource); err == nil {
			return true
		}
		markSource := filepath.Join(dir, rel+".mark")
		if _, err := os.Stat(markSource); err == nil {
			return true
		}
	}
	return false
}
