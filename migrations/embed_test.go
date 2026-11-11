package migrations

import (
	"io/fs"
	"testing"
)

func TestFSSubsets(t *testing.T) {
	root := FS()
	for _, subset := range []string{"server", "frame"} {
		entries, err := fs.ReadDir(root, subset)
		if err != nil {
			t.Fatalf("read %s: %v", subset, err)
		}
		if len(entries) == 0 {
			t.Errorf("subset %s is empty", subset)
		}
	}
}
