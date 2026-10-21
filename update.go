// Self-update for the desktop shell itself.
//
// The binary is installed per user as ~/.local/bin/dsh-desktop, so it can
// replace itself without sudo: download the release tarball, take the binary out
// of it, and rename it over the running one. The rename is the only mutation, so
// a failed download or a truncated file can never leave a broken install behind.
//
// Three ways in:
//
//	dsh-desktop -check-update   report the newest release and exit
//	dsh-desktop -update         install the newest release and exit
//	                            (both need no display)
//	plain launch                check in the background, show a banner in the
//	                            window when a newer release exists
//
// The background check never installs anything on its own: a self-replacing
// binary is not something to do behind the user's back. Pass -no-update-check
// (or set DSH_DESKTOP_NO_UPDATE_CHECK=1) to skip even the check.
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// How long a boot-time check may take before it is abandoned. The window is
	// already usable by then; a slow network must not hold anything up.
	bootCheckTimeout = 4 * time.Second

	// A release download is a few megabytes, so it gets more room.
	downloadTimeout = 60 * time.Second

	// The asset name is a stable contract with the Makefile's `release` target.
	// The tar holds the whole portable bundle; only the binary is installed.
	assetSuffix  = "-linux-x86_64.tar.gz"
	binaryInTar  = "dsh-desktop"
	updateEnvOpt = "DSH_DESKTOP_NO_UPDATE_CHECK"
)

// releasesURL is the GitHub API endpoint for the newest published release. A
// variable rather than a constant so the tests can point it at a local server.
var releasesURL = "https://api.github.com/repos/folkwayse/dsh-desktop-linux/releases/latest"

type release struct {
	Tag    string `json:"tag_name"`
	HTML   string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// tarball returns the download URL of the Linux x86_64 bundle.
func (r release) tarball() (string, bool) {
	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, binaryInTar) && strings.HasSuffix(a.Name, assetSuffix) {
			return a.URL, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// versions
// ---------------------------------------------------------------------------

// parseVersion returns the leading dotted numbers of a version string.
//
// Local builds carry a `git describe` suffix - "v0.1.3", "v0.1.3-4-g3e1fff8",
// "v0.1.3-4-g3e1fff8-dirty" - so only the numeric prefix is comparable. An
// unparseable string yields nothing, which compares as older than anything.
func parseVersion(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(leadingDigits(part))
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}

// compareVersions orders two version strings: -1, 0 or 1. A shorter list counts
// as zero for the missing positions, so "0.2" equals "0.2.0".
func compareVersions(a, b string) int {
	x, y := parseVersion(a), parseVersion(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var xi, yi int
		if i < len(x) {
			xi = x[i]
		}
		if i < len(y) {
			yi = y[i]
		}
		if xi != yi {
			if xi < yi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// isNewer reports whether candidate is a newer release than current.
func isNewer(current, candidate string) bool {
	return compareVersions(current, candidate) < 0
}

// ---------------------------------------------------------------------------
// the release feed
// ---------------------------------------------------------------------------

func fetchLatestRelease(timeout time.Duration) (release, error) {
	var rel release

	req, err := http.NewRequest(http.MethodGet, releasesURL, nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dsh-desktop/"+version)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 404 is the normal answer for a repository with no published release
		// yet, which is not an error worth alarming the user about.
		if resp.StatusCode == http.StatusNotFound {
			return rel, errors.New("no published release found")
		}
		return rel, fmt.Errorf("release feed returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rel, err
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return rel, fmt.Errorf("cannot read the release feed: %w", err)
	}
	return rel, nil
}

// checkForUpdate asks GitHub whether a newer release than current exists.
func checkForUpdate(current string, timeout time.Duration) (release, bool, error) {
	rel, err := fetchLatestRelease(timeout)
	if err != nil {
		return rel, false, err
	}
	if rel.Tag == "" {
		return rel, false, errors.New("the newest release has no tag")
	}
	return rel, isNewer(current, rel.Tag), nil
}

// ---------------------------------------------------------------------------
// installing
// ---------------------------------------------------------------------------

// installUpdate downloads tarballURL, extracts the dsh-desktop binary from it and
// renames it over target. It returns the path of the backup of the binary it
// replaced, so a bad release can be undone by hand.
//
// target empty means "the running binary". A symlinked path is resolved first,
// so installing over ~/.local/bin/dsh-desktop updates the real file rather than
// replacing the link with a copy.
func installUpdate(tarballURL, target string, timeout time.Duration) (string, error) {
	exe, err := installTarget(target)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)

	// The temporary file must sit next to the target: rename is only atomic
	// within one filesystem.
	tmp, err := os.CreateTemp(dir, ".dsh-desktop-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write to %s (is dsh-desktop installed there by another user?): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below has moved it

	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(tarballURL)
	if err != nil {
		tmp.Close()
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	if err := extractBinary(resp.Body, tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	backup, err := backupBinary(exe)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return backup, fmt.Errorf("cannot replace %s: %w", exe, err)
	}
	return backup, nil
}

func installTarget(target string) (string, error) {
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot locate the running binary: %w", err)
		}
		target = exe
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if _, err := os.Stat(target); err != nil {
		return "", err
	}
	return target, nil
}

// extractBinary copies the first regular tar member named dsh-desktop into w,
// streaming, so a multi-megabyte download is never held in memory.
func extractBinary(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the download is not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the download is not a readable tar archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != binaryInTar {
			continue
		}
		if _, err := io.Copy(w, tr); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("the release archive has no %s binary in it", binaryInTar)
}

// backupBinary copies the current binary aside, so an update can be undone. It
// copies instead of renaming: if the copy fails the working install is untouched.
func backupBinary(exe string) (string, error) {
	src, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer src.Close()

	backup := exe + ".backup"
	dst, err := os.OpenFile(backup, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", fmt.Errorf("cannot write the backup %s: %w", backup, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	return backup, nil
}

// ---------------------------------------------------------------------------
// command line
// ---------------------------------------------------------------------------

// runUpdateCommand serves -check-update and -update. Both print to stdout and
// exit, and neither needs a display or a Harness profile.
func runUpdateCommand(cfg config) int {
	rel, newer, err := checkForUpdate(version, 15*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dsh-desktop: cannot check for updates:", err)
		return 1
	}

	if !cfg.update {
		fmt.Printf("dsh-desktop %s (latest %s)\n", version, rel.Tag)
		if !newer {
			fmt.Println("Up to date.")
			return 0
		}
		fmt.Printf("Update available: %s — install it with: dsh-desktop -update\n", rel.Tag)
		return 0
	}

	if !newer {
		fmt.Printf("Already up to date (%s).\n", rel.Tag)
		return 0
	}
	url, ok := rel.tarball()
	if !ok {
		fmt.Fprintf(os.Stderr, "dsh-desktop: release %s has no Linux x86_64 bundle\n", rel.Tag)
		return 1
	}

	backup, err := installUpdate(url, "", downloadTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dsh-desktop: update failed:", err)
		return 1
	}
	fmt.Printf("Updated to %s. The previous binary is kept at %s\n", rel.Tag, backup)
	fmt.Printf("Reopen the app to run it: %s\n", rel.HTML)
	return 0
}

// ---------------------------------------------------------------------------
// the window banner
// ---------------------------------------------------------------------------

// checkForUpdateOnBoot runs once per launch, off the UI thread, and nudges the
// window when a newer release exists. Any failure is silent: an update check is
// a courtesy, and a broken network must not put an error in front of the user.
func (a *app) checkForUpdateOnBoot() {
	rel, newer, err := checkForUpdate(version, bootCheckTimeout)
	if err != nil {
		fmt.Fprintf(a.log, "update check: %v\n", err)
		return
	}
	if !newer {
		fmt.Fprintf(a.log, "update check: %s is the newest release\n", rel.Tag)
		return
	}

	a.notice.Store(noticeText(version, rel.Tag))
	fmt.Fprintf(a.log, "update check: %s is available (running %s)\n", rel.Tag, version)
	notifyUpdate(version, rel.Tag)

	if !a.alive.Load() {
		return
	}
	a.w.Dispatch(func() {
		if a.alive.Load() {
			a.w.Eval("window.__dshUpdateNoticeNow && window.__dshUpdateNoticeNow()")
		}
	})
}

// updateNotice is what the page reads through the __dshUpdateNotice binding.
// It answers with an empty string while there is nothing to say, which is what
// keeps the banner out of the window on every ordinary launch.
func (a *app) updateNotice() string {
	if v, ok := a.notice.Load().(string); ok {
		return v
	}
	return ""
}

// noticeText is what the banner says. It names the command that does the work,
// because the banner itself only reports.
func noticeText(current, latest string) string {
	return fmt.Sprintf("dsh-desktop %s is available (running %s) — install it with: dsh-desktop -update", latest, current)
}

// notifyUpdate also raises a desktop notification, so a user whose window is
// behind another one still finds out. notify-send may not exist; then this is a
// no-op rather than a failure.
func notifyUpdate(current, latest string) {
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	cmd := exec.Command(bin, appName, fmt.Sprintf("dsh-desktop %s is available (running %s)", latest, current))
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

// updateBannerJS returns the script that renders the update banner in whatever
// document is on screen. It is injected through Init, so it is in place before
// the boot page and before the app itself, and it survives the navigation
// between them: each new document re-runs it, asks Go for the message, and
// renders nothing while there is none.
func updateBannerJS() string {
	return `  (function(){
    var ID = 'dsh-desktop-update';
    // The banner is the shell's own widget, so it cannot borrow the client's
    // tokens: the boot page does not have them, and the app may be themed
    // differently from the desktop. It therefore follows the client's live
    // theme when there is one, and the desktop's scheme otherwise.
    function palette() {
      var body = document.body;
      var dark = body && body.hasAttribute('data-ds-dark-theme');
      if (!dark && body && typeof matchMedia === 'function') {
        dark = matchMedia('(prefers-color-scheme: dark)').matches;
      }
      return dark
        ? { bg:'#1b1b1c', fg:'#f9fafb', dim:'#adb2b8', border:'#2c2c2e', shadow:'rgba(0,0,0,.45)' }
        : { bg:'#ffffff', fg:'#0f1115', dim:'#61666b', border:'#0000001f', shadow:'rgba(0,0,0,.18)' };
    }
    function show(msg) {
      if (!msg || document.getElementById(ID)) return;
      var p = palette();
      var box = document.createElement('div');
      box.id = ID;
      box.setAttribute('role', 'status');
      var text = document.createElement('span');
      text.textContent = msg;
      var close = document.createElement('button');
      close.type = 'button';
      close.setAttribute('aria-label', 'Dismiss');
      close.textContent = '\u00d7';
      close.onclick = function(){ box.remove(); };
      box.appendChild(text);
      box.appendChild(close);
      box.style.cssText = 'position:fixed;right:16px;bottom:16px;z-index:2147483647;' +
        'display:flex;align-items:center;gap:10px;max-width:min(520px,80vw);' +
        'padding:10px 12px;border-radius:10px;border:1px solid ' + p.border + ';border-left:3px solid #4d6bfe;' +
        'background:' + p.bg + ';color:' + p.fg + ';font:13px/1.45 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;' +
        'box-shadow:0 8px 24px ' + p.shadow + ';';
      close.style.cssText = 'all:unset;cursor:pointer;color:' + p.dim + ';font-size:15px;line-height:1;padding:2px 4px;';
      (document.body || document.documentElement).appendChild(box);
    }
    window.__dshUpdateNoticeNow = function(){
      if (typeof window.__dshUpdateNotice !== 'function') return;
      window.__dshUpdateNotice().then(show).catch(function(){});
    };
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', window.__dshUpdateNoticeNow);
    } else {
      window.__dshUpdateNoticeNow();
    }
  })();`
}
