package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	updateModulePath    = "github.com/gabe4coding/claude-profiles"
	updateModuleInstall = "github.com/gabe4coding/claude-profiles/cmd/claude-profiles"
	updateCheckInterval = 24 * time.Hour
)

type updateCheckState struct {
	LastCheck     time.Time `json:"last_check"`
	LatestVersion string    `json:"latest_version"`
}

func updateCheckPath() string {
	return filepath.Join(profilesRoot(), "update-check.json")
}

func loadUpdateCheckState() updateCheckState {
	data, err := os.ReadFile(updateCheckPath())
	if err != nil {
		return updateCheckState{}
	}
	var s updateCheckState
	_ = json.Unmarshal(data, &s)
	return s
}

func saveUpdateCheckState(s updateCheckState) {
	data, _ := json.Marshal(s)
	_ = os.WriteFile(updateCheckPath(), data, 0o644)
}

// fetchLatestVersion uses `go list` to resolve the latest published version.
// This works for private modules because it uses the user's existing git credentials.
func fetchLatestVersion() (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("go not found on PATH")
	}
	out, err := exec.Command(goBin, "list", "-m", "-json", updateModulePath+"@latest").Output()
	if err != nil {
		return "", fmt.Errorf("go list failed: %v", err)
	}
	var info struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("cannot parse go list output: %v", err)
	}
	if info.Version == "" {
		return "", fmt.Errorf("empty version from go list")
	}
	return info.Version, nil
}

// semverGT returns true if a is strictly greater than b.
// Pre-release versions (containing a dash) are never considered greater so
// auto-update never installs them. Build metadata ("+dirty", "+something") is
// stripped before comparison.
func semverGT(a, b string) bool {
	if strings.Contains(a, "-") {
		return false
	}
	parse := func(v string) [3]int {
		v = strings.TrimPrefix(v, "v")
		v = strings.SplitN(v, "+", 2)[0] // strip build metadata
		v = strings.SplitN(v, "-", 2)[0] // strip pre-release suffix
		parts := strings.SplitN(v, ".", 3)
		var r [3]int
		for i := 0; i < 3 && i < len(parts); i++ {
			r[i], _ = strconv.Atoi(parts[i])
		}
		return r
	}
	va, vb := parse(a), parse(b)
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

// goEnvBinDir returns the directory where `go install` writes binaries.
func goEnvBinDir(goBin string) (string, error) {
	out, err := exec.Command(goBin, "env", "GOBIN").Output()
	if err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d, nil
		}
	}
	out, err = exec.Command(goBin, "env", "GOPATH").Output()
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(string(out)), "bin"), nil
}

// installUpdate runs `go install` for the given version and atomically replaces
// the currently-running binary with the freshly built one.
func installUpdate(latest string) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("go not found on PATH — run manually: go install %s@%s", updateModuleInstall, latest)
	}

	binDir, err := goEnvBinDir(goBin)
	if err != nil {
		return fmt.Errorf("cannot determine Go bin dir: %v", err)
	}

	// GONOSUMDB=* allows installing private modules whose checksums are not
	// in the public sum database.
	env := append(os.Environ(), "GONOSUMDB=*")
	cmd := exec.Command(goBin, "install", updateModuleInstall+"@"+latest)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go install failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}

	newBin := filepath.Join(binDir, "claude-profiles")

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve current binary path: %v", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("cannot evaluate symlinks for current binary: %v", err)
	}

	// go install already wrote to the correct location.
	if filepath.Clean(newBin) == filepath.Clean(exePath) {
		return nil
	}

	// Atomic replace: copy to .new then rename over the target.
	tmpPath := exePath + ".new"
	if err := copyFile(newBin, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("cannot copy new binary to %s: %v", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("cannot chmod new binary: %v", err)
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("cannot replace binary at %s: %v", exePath, err)
	}
	// On macOS the linker-signed signature does not survive copy+rename — the
	// kernel SIGKILLs the new binary at launch. Re-apply an ad-hoc signature.
	if runtime.GOOS == "darwin" {
		if err := adhocSign(exePath); err != nil {
			return fmt.Errorf("re-sign failed: %v", err)
		}
	}
	return nil
}

// adhocSign re-signs a Mach-O binary with an ad-hoc signature using the system
// `codesign` tool. Required after writing a Mach-O binary via os.Rename on macOS.
func adhocSign(path string) error {
	cs, err := exec.LookPath("codesign")
	if err != nil {
		return fmt.Errorf("codesign not on PATH")
	}
	out, err := exec.Command(cs, "--force", "--sign", "-", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// kickAutoUpdate checks for a newer version in the background and installs it.
// No-op when version=="dev", CLAUDE_PROFILES_NO_UPDATE is set, or the last
// check was less than 24 h ago.
func kickAutoUpdate() {
	if version == "dev" || os.Getenv("CLAUDE_PROFILES_NO_UPDATE") != "" {
		return
	}
	state := loadUpdateCheckState()
	if time.Since(state.LastCheck) < updateCheckInterval {
		return
	}
	go func() {
		defer func() { recover() }() //nolint:errcheck

		latest, err := fetchLatestVersion()
		if err != nil {
			return
		}
		state.LastCheck = time.Now()
		state.LatestVersion = latest
		saveUpdateCheckState(state)

		if !semverGT(latest, version) {
			return
		}
		_ = installUpdate(latest)
	}()
}

// cmdUpdate is the foreground `claude-profiles update` subcommand.
func cmdUpdate() {
	if version == "dev" {
		fmt.Fprintln(os.Stderr, "Running dev build — skipping update check.")
		return
	}
	if os.Getenv("CLAUDE_PROFILES_NO_UPDATE") != "" {
		fmt.Fprintln(os.Stderr, "CLAUDE_PROFILES_NO_UPDATE is set — updates disabled.")
		return
	}

	fmt.Fprintf(os.Stderr, "Current version:  %s\n", version)
	fmt.Fprint(os.Stderr, "Checking for updates... ")

	latest, err := fetchLatestVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed\n%v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "latest is %s\n", latest)

	if !semverGT(latest, version) {
		fmt.Fprintln(os.Stderr, "Already up to date.")
		return
	}

	fmt.Fprintf(os.Stderr, "Updating to %s...\n", latest)
	if err := installUpdate(latest); err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Updated successfully. Restart to use %s.\n", latest)
}
