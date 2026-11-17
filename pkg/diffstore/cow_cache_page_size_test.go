package diffstore

import (
	"strings"
	"testing"
)

// Safe on 4KiB CI hosts, but checks fail-closed 16/64KiB decisions.
func TestElasticCacheRejectsUnsafeHostPageSizes(t *testing.T) {
	if err := safeElasticHostPageSize(4096); err != nil {
		t.Fatal(err)
	}
	for _, sz := range []int{8192, 16384, 65536} {
		if err := safeElasticHostPageSize(sz); err == nil || !strings.Contains(err.Error(), "4KiB") {
			t.Fatalf("unsafe host page size %d accepted: %v", sz, err)
		}
	}
}
