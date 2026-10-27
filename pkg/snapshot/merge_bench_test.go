package snapshot

import (
	"bytes"
	"context"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func BenchmarkValidateMergeBaseFragmented(b *testing.B) {
	const size = 1 << 30
	const page = 4096
	holes := make([]sparse.Extent, 0, size/page/2)
	for off := uint64(0); off < size; off += 2 * page {
		holes = append(holes, sparse.Extent{Offset: off, Size: page})
	}
	// Metadata only: no image payload is allocated or read. boundaryStream is
	// an existing fixture so this harness is reusable on the previous version.
	src, err := sparse.NewSource(bytes.NewReader(nil), size, holes)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := &boundaryStream{Source: src}
		err := ValidateMergeBaseWithOpener(context.Background(), "file://base.overlay", size, nil, false,
			func(context.Context, string) (fetch.Stream, error) { return s, nil })
		if err != nil || s.closes != 1 {
			b.Fatalf("preflight: err=%v closes=%d", err, s.closes)
		}
	}
}
