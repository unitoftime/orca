package main

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/unitoftime/orca/internal/deploy"
)

// The status page is orca itself, run on the machine as `orca serve-status`,
// and the certificate job is too, as `orca serve-certs`. Until orca publishes
// an image of its own, apply ships a build of orca to their machines and each
// job mounts it into a stock image. This file is that shipping: finding a
// build that runs on the machine, and putting it there.
//
// Swapping to a published image means deleting this file and setting the
// status spec's Image instead of its Binary; nothing else changes.

const modulePath = "github.com/unitoftime/orca"

// statusBinary is a build of orca that runs on the machines: linux, amd64, and
// statically linked, since the image it is mounted into has a different C
// library from the machine that built it, or none.
type statusBinary struct {
	Local string // on this machine
	Sum   string // sha256 of its contents, hex
}

// Remote is where it lives on the machine, named for its contents.
func (b statusBinary) Remote() string { return deploy.StatusBinaryPath(DataDir, b.Sum) }

// resolveStatusBinary finds a build of orca to ship: this one, when it will
// run on the machine as it is, or else one built from the same version.
func resolveStatusBinary(ctx context.Context) (statusBinary, error) {
	self, err := os.Executable()
	if err != nil {
		return statusBinary{}, fmt.Errorf("find orca's own binary: %w", err)
	}

	local := self
	if !runsOnMachine(self) {
		if local, err = buildForMachine(ctx); err != nil {
			return statusBinary{}, err
		}
	}

	sum, err := fileSHA256(local)
	if err != nil {
		return statusBinary{}, err
	}
	return statusBinary{Local: local, Sum: sum}, nil
}

// runsOnMachine reports whether a binary can be mounted into the status image
// as it is. A dynamically linked one is asking for the C library of the
// machine that built it: an Arch laptop's glibc is newer than Debian's, and
// the binary fails to start with a symbol version error.
func runsOnMachine(file string) bool {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return false
	}
	f, err := elf.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false
		}
	}
	return true
}

// buildForMachine builds this version of orca for the machines, from the
// module cache, the same source `go install` built this binary from. Go is
// already a requirement for running orca at all, so it is there to build with.
//
// Kept in the user cache per version, so it is built once, not every apply.
func buildForMachine(ctx context.Context) (string, error) {
	version, published := buildVersion()
	if !published {
		return "", fmt.Errorf("the status page and the certificate job run orca on the machine, and this build of orca can't run there: " +
			"it is not a static linux/amd64 binary, and it was built from a checkout, so there is no published version to build one from. " +
			"On Linux, build it with `make build` and run that; anywhere else, run a published version " +
			"(go install " + modulePath + "/cmd/orca@latest)")
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find a cache directory to build orca for the machine in: %w", err)
	}
	out := filepath.Join(cache, "orca", version, "orca-linux-amd64")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	fmt.Printf("building orca %s for the machines (once per version) ...\n", version)

	var mod struct{ Dir, Error string }
	dl := exec.CommandContext(ctx, "go", "mod", "download", "-json", modulePath+"@"+version)
	dl.Env = append(os.Environ(), "GOWORK=off")
	raw, err := dl.Output()
	if jerr := json.Unmarshal(raw, &mod); jerr != nil || mod.Dir == "" {
		if mod.Error != "" {
			return "", fmt.Errorf("fetch orca %s to build it for the machines: %s", version, mod.Error)
		}
		return "", fmt.Errorf("fetch orca %s to build it for the machines: %v", version, err)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	tmp := out + ".tmp"
	build := exec.CommandContext(ctx, "go", "build", "-trimpath",
		"-ldflags", "-X main.build="+version, "-o", tmp, "./cmd/orca")
	build.Dir = mod.Dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOWORK=off")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("build orca %s for the machines: %w", version, err)
	}
	return out, os.Rename(tmp, out)
}

func fileSHA256(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", file, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// shipStatusBinary puts the build on the machine the status page runs on,
// unless it is already there. It runs on every apply that includes the
// platform, changed or not, so a binary removed by hand comes back before the
// page next restarts and finds it missing.
//
// The two most recent other builds are kept: Nomad reverts a failed rollout to
// the previous job, and that job names the previous build.
func shipStatusBinary(ctx context.Context, bin statusBinary, hosts []NodeConfig) error {
	for _, host := range hosts {
		if err := shipStatusBinaryTo(ctx, bin, host); err != nil {
			return err
		}
	}
	return nil
}

func shipStatusBinaryTo(ctx context.Context, bin statusBinary, host NodeConfig) error {
	node := Node{Host: host.Host}
	remote := bin.Remote()

	present, err := node.RunOutput(ctx, fmt.Sprintf("test -f %s && echo yes || true", shQuote(remote)))
	if err != nil {
		return err
	}
	if strings.TrimSpace(present) != "yes" {
		fmt.Printf("  ship    orca to %s ... ", host.Name)
		if err := node.RunQuiet(ctx, "mkdir -p "+shQuote(path.Dir(remote))); err != nil {
			fmt.Println("FAILED")
			return err
		}
		if err := node.UploadFile(ctx, bin.Local, remote); err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("ok")
	}

	return node.RunQuiet(ctx, pruneStatusBinariesScript(remote))
}

// pruneStatusBinariesScript removes every build but the current one and the
// two newest others.
func pruneStatusBinariesScript(current string) string {
	return fmt.Sprintf(`cd %s && ls -t orca-* | grep -vx %s | tail -n +3 | xargs -r rm -f`,
		shQuote(path.Dir(current)), shQuote(path.Base(current)))
}
