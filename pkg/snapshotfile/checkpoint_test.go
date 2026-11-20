package snapshotfile

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tailzip"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

func localTestDescriptor() memorycheckpoint.Descriptor {
	return memorycheckpoint.Descriptor{Version: memorycheckpoint.Version, MemoryID: "memory-one", CheckpointID: "checkpoint-one",
		Size: 8192, MapRef: "file://map.image@digest:" + strings.Repeat("a", 64), WorkingSet: true}
}

func TestLocalCheckpointMetadataAndStrictProfile(t *testing.T) {
	ctx := context.Background()
	descriptor := localTestDescriptor()
	source, err := BuildLocalSource([]byte(`{"memory":{"size":8192}}`), []byte(`{"cpu":1}`), []byte("version: 1\n"), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	stream := &configReadStream{Source: source}
	metadata, err := ReadMetadata(ctx, stream)
	if err != nil || metadata.ArchiveBase != 0 || metadata.LocalCheckpoint == nil {
		t.Fatalf("local metadata: %+v, %v", metadata, err)
	}
	if metadata.LocalCheckpoint.MemoryID != descriptor.MemoryID || metadata.MemoryDirty != nil {
		t.Fatal("descriptor changed or acquired ordinary dirty semantics")
	}
	if _, err := Open(ctx, stream); err == nil {
		t.Fatal("unresolved descriptor exposed as portable memory")
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(encoded, '\n'), bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(encoded, []byte(`"size":8192`), []byte(`"size":8193`), 1)} {
		tail, err := tailzip.EncodeCanonical([]tailzip.Entry{{Name: ConfigJSONName, Body: []byte("{}")}, {Name: StateJSONName, Body: []byte("{}")},
			{Name: SnapshotCfgName, Body: []byte("version: 1\n")}, {Name: MemoryCheckpointName, Body: bad}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(ctx, &configReadStream{Source: sparse.Dense(bytes.NewReader(tail), uint64(len(tail)))}); err == nil {
			t.Fatal("noncanonical/invalid local descriptor accepted by metadata-only reader")
		}
	}
}

func TestLocalDescriptorSkipsExecutionBodies(t *testing.T) {
	body := []byte(`{"state":"` + strings.Repeat("x", 1<<20) + `"}`)
	source, err := BuildLocalSource(body, body, []byte("version: 1\n"), localTestDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	start := uint64(localHeaderSize + len(ConfigJSONName))
	stream := &configReadStream{Source: source, forbiddenStart: start, forbiddenEnd: start + uint64(len(body))}
	descriptor, err := ReadLocalDescriptor(context.Background(), stream)
	if err != nil || descriptor == nil || descriptor.MemoryID != "memory-one" {
		t.Fatalf("descriptor-only read: %+v, %v", descriptor, err)
	}
	if stream.readBytes > 2048 {
		t.Fatalf("descriptor-only read consumed %d bytes", stream.readBytes)
	}
}
