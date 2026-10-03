package logger

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// assertBackups checks that the backups of file are exactly want, mapping
// suffixes (".1", ".3.gz") to their decompressed content.
func assertBackups(t *testing.T, file string, want map[string]string) {
	t.Helper()

	matches, err := filepath.Glob(file + ".*")
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(matches))
	for _, match := range matches {
		got = append(got, strings.TrimPrefix(match, file))
	}

	wantSuffixes := make([]string, 0, len(want))
	for suffix := range want {
		wantSuffixes = append(wantSuffixes, suffix)
	}

	sort.Strings(got)
	sort.Strings(wantSuffixes)

	if strings.Join(got, ",") != strings.Join(wantSuffixes, ",") {
		t.Fatalf("backups = %v, want %v", got, wantSuffixes)
	}

	for suffix, content := range want {
		if got := readBackup(t, file+suffix); got != content {
			t.Errorf("%s contains %q, want %q", suffix, got, content)
		}
	}
}

// readBackup returns the (decompressed) content of a backup.
func readBackup(t *testing.T, name string) string {
	t.Helper()

	data := readFile(t, name)
	if !strings.HasSuffix(name, gzExt) {
		return data
	}

	gz, err := gzip.NewReader(strings.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	return string(plain)
}

// writeBackup creates a backup with content, gzipped when name ends in .gz.
func writeBackup(t *testing.T, name, content string) {
	t.Helper()

	data := []byte(content)

	if strings.HasSuffix(name, gzExt) {
		var buf bytes.Buffer
		if err := compress(&buf, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}

		data = buf.Bytes()
	}

	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRotation(t *testing.T) {
	tests := []struct {
		name           string
		keep           int
		keepCompressed int
		want           map[string]string
	}{
		{
			name: "plain and compressed",
			keep: 2, keepCompressed: 2,
			want: map[string]string{".1": "/5\n", ".2": "/4\n", ".3.gz": "/3\n", ".4.gz": "/2\n"},
		},
		{
			name: "defaults",
			keep: DefaultKeep, keepCompressed: DefaultKeepCompressed,
			want: map[string]string{".1": "/5\n"},
		},
		{
			name: "compressed only",
			keep: 0, keepCompressed: 2,
			want: map[string]string{".1.gz": "/5\n", ".2.gz": "/4\n"},
		},
		{
			name: "no backups",
			keep: 0, keepCompressed: 0,
			want: map[string]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "access.log")
			m := newTestMiddleware(t, &Config{
				File:   file,
				Format: "{.Path}",
				Rotate: &RotateConfig{Keep: intPtr(test.keep), KeepCompressed: intPtr(test.keepCompressed)},
			}, nil)

			for _, path := range []string{"/1", "/2", "/3", "/4", "/5"} {
				serve(m, path)
				flushNow(m.writer)
				rotateNow(m.writer)
			}

			serve(m, "/current")
			flushNow(m.writer)

			if got := readFile(t, file); got != "/current\n" {
				t.Errorf("current file contains %q", got)
			}

			assertBackups(t, file, test.want)
		})
	}
}

func TestRotation_SkipsEmptyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Rotate: &RotateConfig{}}, nil)

	rotateNow(m.writer)

	assertBackups(t, file, map[string]string{})
}

func TestRotation_PreservesModTime(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	writeBackup(t, file+".1", "old\n")

	modTime := time.Date(2020, time.May, 4, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(file+".1", modTime, modTime); err != nil {
		t.Fatal(err)
	}

	if err := cleanupBackups(file, 0, 1); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(file + ".1.gz")
	if err != nil {
		t.Fatal(err)
	}

	if !info.ModTime().Equal(modTime) {
		t.Errorf("mod time = %v, want %v", info.ModTime(), modTime)
	}
}

func TestCleanupBackups(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")

	writeBackup(t, file, "current\n")
	writeBackup(t, file+".1.gz", "one\n")      // plain zone: decompress
	writeBackup(t, file+".2", "two\n")         // plain zone: keep
	writeBackup(t, file+".2.gz", "two\n")      // duplicate from an interrupted conversion
	writeBackup(t, file+".3", "three\n")       // compressed zone: compress
	writeBackup(t, file+".3.gz.tmp", "junk")   // interrupted conversion leftover
	writeBackup(t, file+".4.gz", "four\n")     // compressed zone: keep
	writeBackup(t, file+".5", "five\n")        // beyond retention: delete
	writeBackup(t, file+".6.gz", "six\n")      // beyond retention: delete
	writeBackup(t, file+".old", "unrelated\n") // not a backup: ignore
	writeBackup(t, file+".0", "zero\n")        // not a valid index: ignore

	if err := cleanupBackups(file, 2, 2); err != nil {
		t.Fatal(err)
	}

	assertBackups(t, file, map[string]string{
		".1": "one\n", ".2": "two\n", ".3.gz": "three\n", ".4.gz": "four\n",
		".old": "unrelated\n", ".0": "zero\n",
	})

	if got := readFile(t, file); got != "current\n" {
		t.Errorf("current file modified: %q", got)
	}

	// Cleanup is idempotent.
	if err := cleanupBackups(file, 2, 2); err != nil {
		t.Fatal(err)
	}

	assertBackups(t, file, map[string]string{
		".1": "one\n", ".2": "two\n", ".3.gz": "three\n", ".4.gz": "four\n",
		".old": "unrelated\n", ".0": "zero\n",
	})
}

func TestCleanupBackups_CorruptArchive(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	writeBackup(t, file+".1.gz.notreally", "x")

	if err := os.WriteFile(file+".1.gz", []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeBackup(t, file+".2", "two\n")

	if err := cleanupBackups(file, 1, 1); err == nil {
		t.Fatal("expected an error for a corrupt archive")
	}

	// The corrupt archive is kept for inspection, no temporary file is left
	// behind and other backups are still processed.
	matches, _ := filepath.Glob(file + ".*")
	sort.Strings(matches)

	want := []string{file + ".1.gz", file + ".1.gz.notreally", file + ".2.gz"}
	if strings.Join(matches, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", matches, want)
	}
}

func TestStartupCleanup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	writeBackup(t, file+".1.gz", "one\n")
	writeBackup(t, file+".2.gz", "two\n")
	writeBackup(t, file+".3", "three\n")

	newTestMiddleware(t, &Config{File: file, Rotate: &RotateConfig{Keep: intPtr(1), KeepCompressed: intPtr(1)}}, nil)

	assertBackups(t, file, map[string]string{".1": "one\n", ".2.gz": "two\n"})
}

func TestStartupWithoutRotationKeepsBackups(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	writeBackup(t, file+".1.gz", "one\n")
	writeBackup(t, file+".7", "seven\n")

	newTestMiddleware(t, &Config{File: file}, nil)

	assertBackups(t, file, map[string]string{".1.gz": "one\n", ".7": "seven\n"})
}

func TestParseIndex(t *testing.T) {
	tests := map[string]bool{
		"1": true, "42": true, "0": false, "": false, "-1": false, "+1": false,
		"1a": false, "1234567890": false,
	}

	for in, valid := range tests {
		if _, ok := parseIndex(in); ok != valid {
			t.Errorf("parseIndex(%q) valid = %v, want %v", in, ok, valid)
		}
	}
}

func TestRotationEqual(t *testing.T) {
	a := &rotation{spec: "@daily", keep: 1}
	b := &rotation{spec: "@daily", keep: 1}
	c := &rotation{spec: "@daily", keep: 2}

	switch {
	case !a.equal(b):
		t.Error("identical settings are not equal")
	case a.equal(c):
		t.Error("different settings are equal")
	case a.equal(nil), (*rotation)(nil).equal(a):
		t.Error("settings equal to nil")
	case !(*rotation)(nil).equal(nil):
		t.Error("nil not equal to nil")
	}
}
