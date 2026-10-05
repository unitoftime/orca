package images

import (
	"os"
	"regexp"
	"testing"
)

// Apply never resolves these, so one without a digest would run whatever its
// tag points at on the day each machine pulls it.
func TestEveryImageIsPinned(t *testing.T) {
	src, err := os.ReadFile("images.go")
	if err != nil {
		t.Fatal(err)
	}
	consts := regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(consts) == 0 {
		t.Fatal("found no images")
	}
	pinned := regexp.MustCompile(`^[^@\s]+:[^@\s]+@sha256:[0-9a-f]{64}$`)
	for _, c := range consts {
		if !pinned.MatchString(c[2]) {
			t.Errorf("%s = %q is not repo:tag@sha256:<digest>; run make pin-images", c[1], c[2])
		}
	}
}
