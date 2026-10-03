package logger

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/cronexpr"
)

const (
	gzExt  = ".gz"
	tmpExt = ".tmp"
)

// rotation holds validated rotation settings.
type rotation struct {
	spec           string
	schedule       *cronexpr.Expression
	keep           int
	keepCompressed int
}

// newRotation validates cfg and applies defaults to unset fields.
func newRotation(cfg *RotateConfig) (*rotation, error) {
	rot := &rotation{spec: cfg.Schedule, keep: DefaultKeep, keepCompressed: DefaultKeepCompressed}

	if strings.TrimSpace(rot.spec) == "" {
		rot.spec = DefaultSchedule
	}

	if cfg.Keep != nil {
		rot.keep = *cfg.Keep
	}

	if cfg.KeepCompressed != nil {
		rot.keepCompressed = *cfg.KeepCompressed
	}

	if rot.keep < 0 || rot.keepCompressed < 0 {
		return nil, fmt.Errorf("rotate.keep (%d) and rotate.keepCompressed (%d) must not be negative", rot.keep, rot.keepCompressed)
	}

	schedule, err := cronexpr.Parse(rot.spec)
	if err != nil {
		return nil, fmt.Errorf("invalid rotate.schedule %q: %w", rot.spec, err)
	}

	rot.schedule = schedule

	return rot, nil
}

// String describes the settings for log messages.
func (r *rotation) String() string {
	if r == nil {
		return "no rotation"
	}

	return fmt.Sprintf("schedule %q, keep %d, keepCompressed %d", r.spec, r.keep, r.keepCompressed)
}

// equal reports whether r and o are the same settings. Either may be nil.
func (r *rotation) equal(o *rotation) bool {
	if r == nil || o == nil {
		return r == o
	}

	return r.spec == o.spec && r.keep == o.keep && r.keepCompressed == o.keepCompressed
}

// backupName returns the name of backup index of path.
func backupName(path string, index int, compressed bool) string {
	name := path + "." + strconv.Itoa(index)
	if compressed {
		name += gzExt
	}

	return name
}

// shiftBackups renames path to <path>.1 after shifting every backup index up
// by one, deleting the backups that would exceed total. With total == 0 the
// current file is simply deleted. Backups are renamed as-is (plain or
// compressed): cleanupBackups converts them afterward.
func shiftBackups(path string, total int) []error {
	var errs []error

	if total == 0 {
		return appendErr(errs, removeIfExists(path))
	}

	for _, compressed := range []bool{false, true} {
		errs = appendErr(errs, removeIfExists(backupName(path, total, compressed)))
	}

	for i := total - 1; i >= 1; i-- {
		for _, compressed := range []bool{false, true} {
			errs = appendErr(errs, renameIfExists(backupName(path, i, compressed), backupName(path, i+1, compressed)))
		}
	}

	return appendErr(errs, renameIfExists(path, backupName(path, 1, false)))
}

// backupFiles describes the files found for one backup index.
type backupFiles struct {
	plain      bool
	compressed bool
}

// cleanupBackups brings the backups of path in line with the retention:
// indexes 1..keep are plain text (decompressing if needed), indexes
// keep+1..keep+keepCompressed are gzip-compressed (compressing if needed) and
// anything beyond is deleted, as are temporary files left by an interrupted
// conversion. The caller must hold the writer's maintenance lock.
//
// It is idempotent and resumable: conversions write a temporary file that is
// atomically renamed before the source is removed, so when both variants of
// an index exist, both are complete and either can be kept.
func cleanupBackups(path string, keep, keepCompressed int) error {
	backups, errs := scanBackups(path)
	total := keep + keepCompressed

	indexes := make([]int, 0, len(backups))
	for index := range backups {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	for _, index := range indexes {
		files := backups[index]
		plain, compressed := backupName(path, index, false), backupName(path, index, true)

		switch {
		case index > total:
			errs = appendErr(errs, removeIfExists(plain))
			errs = appendErr(errs, removeIfExists(compressed))
		case index <= keep:
			if !files.plain {
				errs = appendErr(errs, convert(compressed, plain, decompress))
			} else if files.compressed {
				errs = appendErr(errs, removeIfExists(compressed))
			}
		default:
			if !files.compressed {
				errs = appendErr(errs, convert(plain, compressed, compress))
			} else if files.plain {
				errs = appendErr(errs, removeIfExists(plain))
			}
		}
	}

	return errors.Join(errs...)
}

// scanBackups lists the backups of path by index and removes stale temporary files.
func scanBackups(path string) (map[int]*backupFiles, []error) {
	dir, base := filepath.Split(path)
	prefix := base + "."

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{err}
	}

	var errs []error

	backups := make(map[int]*backupFiles)

	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		if dirEntry.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}

		suffix, tmp := strings.CutSuffix(name[len(prefix):], tmpExt)
		digits, compressed := strings.CutSuffix(suffix, gzExt)

		index, ok := parseIndex(digits)
		if !ok {
			continue // not one of our files, e.g. access.log.old
		}

		if tmp {
			errs = appendErr(errs, removeIfExists(filepath.Join(dir, name)))
			continue
		}

		files := backups[index]
		if files == nil {
			files = &backupFiles{}
			backups[index] = files
		}

		if compressed {
			files.compressed = true
		} else {
			files.plain = true
		}
	}

	return backups, errs
}

// parseIndex parses a positive backup index made of ASCII digits only.
func parseIndex(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}

	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}

	index, err := strconv.Atoi(s)

	return index, err == nil && index > 0
}

// convert transforms src into dst through a temporary file, then removes
// src. The modification time of src is preserved.
func convert(src, dst string, transform func(io.Writer, io.Reader) error) (err error) {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	tmp := dst + tmpExt

	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()

	if err = transform(out, in); err != nil {
		return fmt.Errorf("converting %s: %w", src, err)
	}

	if err = out.Sync(); err != nil {
		return err
	}

	if err = out.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmp, dst); err != nil {
		return err
	}

	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())

	return os.Remove(src)
}

// compress gzips r into w.
func compress(w io.Writer, r io.Reader) error {
	gz := gzip.NewWriter(w)
	if _, err := io.Copy(gz, r); err != nil {
		_ = gz.Close()
		return err
	}

	return gz.Close()
}

// decompress gunzips r into w.
func decompress(w io.Writer, r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}

	if _, err := io.Copy(w, gz); err != nil { //nolint:gosec // G110: we only decompress our own backups.
		_ = gz.Close()
		return err
	}

	return gz.Close()
}

// removeIfExists removes name, ignoring a missing file.
func removeIfExists(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// renameIfExists renames src to dst, ignoring a missing src.
func renameIfExists(src, dst string) error {
	if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// appendErr appends err to errs when it is not nil.
func appendErr(errs []error, err error) []error {
	if err != nil {
		return append(errs, err)
	}

	return errs
}
