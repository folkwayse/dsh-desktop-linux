package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseVersionIgnoresGitDescribeSuffix(t *testing.T) {
	// The build stamps `git describe --tags --always --dirty` into version, so
	// the string a shipped binary reports is not always a clean semver. Only the
	// leading numbers may take part in the comparison.
	cases := map[string][]int{
		"v0.1.3":                  {0, 1, 3},
		"0.1.3":                   {0, 1, 3},
		"v0.1.3-4-g3e1fff8":       {0, 1, 3},
		"v0.1.3-4-g3e1fff8-dirty": {0, 1, 3},
		"v2.0":                    {2, 0},
		"dirty":                   nil,
		"":                        nil,
	}
	for in, want := range cases {
		got := parseVersion(in)
		if len(got) != len(want) {
			t.Errorf("parseVersion(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseVersion(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.3", "v0.1.4", -1},
		{"v0.1.4", "v0.1.3", 1},
		{"v0.1.3", "v0.1.3", 0},
		// A local build off the tag is the same release as far as the check is
		// concerned: it must not offer to downgrade the user.
		{"v0.1.3-4-g3e1fff8-dirty", "v0.1.3", 0},
		{"0.2", "v0.2.0", 0},
		{"v0.9.9", "v0.10.0", -1},
		{"v1", "v0.99.99", 1},
		// An unstamped build ("dirty", "") must read as older than any release,
		// so development builds still see the update.
		{"dirty", "v0.1.3", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := isNewer(c.a, c.b); got != (c.want < 0) {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want < 0)
		}
	}
}

func TestReleasePicksTheLinuxBundle(t *testing.T) {
	var rel release
	rel.Tag = "v0.1.4"
	rel.Assets = append(rel.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "dsh-desktop-v0.1.4-linux-arm64.tar.gz", URL: "https://example.test/arm64"},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "dsh-desktop-v0.1.4-linux-x86_64.tar.gz", URL: "https://example.test/x86_64"},
	)

	got, ok := rel.tarball()
	if !ok {
		t.Fatal("release with a matching asset reported none")
	}
	if got != "https://example.test/x86_64" {
		t.Errorf("tarball() = %q, want the x86_64 bundle", got)
	}

	empty := release{Tag: "v9.9.9"}
	if _, ok := empty.tarball(); ok {
		t.Error("a release without any bundle reported one")
	}
}

// releaseTarball builds an archive shaped like the one `make release` produces.
func releaseTarball(t *testing.T, member string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     member,
		Mode:     0o755,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("cannot write the test tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("cannot write the test tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("cannot close the test tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("cannot close the test gzip: %v", err)
	}
	return buf.Bytes()
}

func TestInstallUpdateReplacesTheBinaryAndKeepsABackup(t *testing.T) {
	// The whole point of the rename-based install: the file at the target path
	// becomes the downloaded binary, and the one it replaced is still on disk.
	const newBytes = "#!/bin/sh\necho v0.1.4\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(releaseTarball(t, "dsh-desktop-v0.1.4-linux-x86_64/dsh-desktop", []byte(newBytes)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "dsh-desktop")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	backup, err := installUpdate(srv.URL, target, 5*time.Second)
	if err != nil {
		t.Fatalf("installUpdate returned an error: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != newBytes {
		t.Errorf("target holds %q, want the downloaded binary", got)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o100 == 0 {
		t.Errorf("installed binary mode %v is not executable", st.Mode().Perm())
	}

	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("no backup at %s: %v", backup, err)
	}
	if string(saved) != "old binary" {
		t.Errorf("backup holds %q, want the replaced binary", saved)
	}

	// No temporary files may survive a successful install.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dsh-desktop-update-") {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestInstallUpdateRefusesAnArchiveWithoutTheBinary(t *testing.T) {
	// A tarball that does not contain the binary must leave the install alone:
	// installing an empty file would brick the app.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(releaseTarball(t, "dsh-desktop-v0.1.4-linux-x86_64/README.md", []byte("docs")))
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "dsh-desktop")
	const original = "old binary"
	if err := os.WriteFile(target, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := installUpdate(srv.URL, target, 5*time.Second); err == nil {
		t.Fatal("installUpdate accepted an archive with no dsh-desktop in it")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("failed install changed the target to %q", got)
	}
}

func TestInstallUpdateReportsAFailedDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "dsh-desktop")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installUpdate(srv.URL, target, 5*time.Second); err == nil {
		t.Fatal("installUpdate accepted a 404 response")
	}
}

func TestUpdateNoticeIsEmptyUntilTheCheckFindsARelease(t *testing.T) {
	// The banner must stay out of the window on an ordinary launch, which is
	// exactly what an empty notice achieves.
	a := &app{}
	if got := a.updateNotice(); got != "" {
		t.Errorf("updateNotice() = %q before any check, want empty", got)
	}

	a.notice.Store(noticeText("v0.1.3", "v0.1.4"))
	got := a.updateNotice()
	if !strings.Contains(got, "v0.1.4") || !strings.Contains(got, "v0.1.3") {
		t.Errorf("notice %q does not name both versions", got)
	}
	// The banner only reports; the command it names is what installs.
	if !strings.Contains(got, "dsh-desktop -update") {
		t.Errorf("notice %q does not name the command that installs it", got)
	}
}

func TestUpdateBannerScriptAsksGoAndSurvivesNavigation(t *testing.T) {
	js := updateBannerJS()
	for _, want := range []string{"__dshUpdateNoticeNow", "__dshUpdateNotice", "dsh-desktop-update"} {
		if !strings.Contains(js, want) {
			t.Errorf("banner script lacks %q", want)
		}
	}
	// It is installed through Init, which runs in every document, so it must
	// also re-render after the client's own navigation replaced the boot page.
	if !strings.Contains(js, "DOMContentLoaded") {
		t.Error("banner script does not re-render when a document loads")
	}
}
