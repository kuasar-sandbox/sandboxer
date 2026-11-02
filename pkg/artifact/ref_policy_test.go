package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/transfer"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

var refTestKey = [32]byte{0x54, 0x87}

func refTestCodec(t *testing.T) tarstream.Codec {
	t.Helper()
	codec, err := crypto.NewTarStreamCodec(refTestKey)
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func locatedTestRef(t *testing.T, raw, location string) manifest.Ref {
	t.Helper()
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		t.Fatal(err)
	}
	ref.Location, ref.Path = location, filepath.Base(ref.Path)
	return ref
}

func testRefOverlay(t *testing.T, dir, location string, codec tarstream.Codec, data []byte) manifest.Ref {
	t.Helper()
	sink := snapshot.NewFileSink(dir, "disk", codec, codec != nil, nil)
	defer sink.Close()
	raw, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	return locatedTestRef(t, raw, location)
}

func testRefSandbox(t *testing.T, dir, parent string, codec tarstream.Codec) (string, string) {
	t.Helper()
	raw, _ := publishPortable(t, parent)
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{7}, 8192)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "e", codec, codec != nil, nil)
	defer sink.Close()
	ref, path, err := sink.AbsorbSandbox(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return ref, path
}

func testRefSnapshot(t *testing.T, dir, e string, parents []string, codec tarstream.Codec) (string, string) {
	t.Helper()
	raw, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e, FromRefs: parents})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{3}, 8192)), []byte("{}"), []byte("{}"), raw)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "s", codec, codec != nil, nil)
	defer sink.Close()
	ref, path, err := sink.AbsorbSnapshot(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return ref, path
}

func policyStorage(t *testing.T, local, ref crypto.LocalPolicy) *ProcessStorage {
	t.Helper()
	s, err := NewProcessStorageWithCustomerKey(&config.ManifestConfig{Crypto: crypto.Config{Local: local, Ref: ref}},
		func() ([32]byte, error) { return refTestKey, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRefPolicyReadRetainAndWrite(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		policy           crypto.LocalPolicy
		plain, encrypted error
		scheme           string
	}{
		{crypto.LocalOff, nil, tarstream.ErrCodecRequired, "digest"},
		{crypto.LocalAuto, nil, nil, "hmac"},
		{crypto.LocalRequired, tarstream.ErrPlaintextForbidden, nil, "hmac"},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			s := policyStorage(t, crypto.LocalOff, tc.policy)
			for _, input := range []struct {
				name  string
				codec tarstream.Codec
				want  error
			}{
				{"plain", nil, tc.plain}, {"encrypted", refTestCodec(t), tc.encrypted},
			} {
				t.Run(input.name, func(t *testing.T) {
					loc := "checkpoint-temp-external"
					directory, output := t.TempDir(), t.TempDir()
					locations := config.RefLocations{loc: directory, "out": output}
					disk := testRefOverlay(t, directory, loc, input.codec, bytes.Repeat([]byte{4}, 8192))
					if input.want != nil {
						// Only the physical carrier can tell us whether this input is encrypted.
						disk.DigestScheme = tc.scheme
					}
					opened, err := s.OpenFileWithLocations(ctx, filepath.Join(directory, disk.Path), disk, locations)
					if !errors.Is(err, input.want) {
						t.Fatalf("read = %v, want %v", err, input.want)
					}
					if opened != nil {
						opened.Close()
					}
					_, path := testRefSandbox(t, t.TempDir(), disk.String(), nil)
					pub, err := NewLocationPublisher(s, "out", output, locations, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer pub.Close()
					result, err := pub.Publish(ctx, path)
					if !errors.Is(err, input.want) {
						t.Fatalf("retain = %v, want %v", err, input.want)
					}
					if err != nil {
						return
					}
					root, err := manifest.ParseRef(result.Ref)
					if err != nil || root.DigestScheme != tc.scheme {
						t.Fatalf("result=%+v, %v", root, err)
					}
					doc, err := s.Inspect(ctx, result.Ref, locations)
					if err != nil || doc.Sandbox.Boot.Root.BaseFromRefs[0] != disk.String() {
						t.Fatalf("retained disk changed: %v, %v", doc, err)
					}
					file, err := fetch.OpenTarStream(filepath.Join(output, root.Path))
					if tc.scheme == "hmac" && !errors.Is(err, tarstream.ErrCodecRequired) || tc.scheme == "digest" && err != nil {
						t.Fatalf("output envelope: %v", err)
					}
					if file != nil {
						file.Close()
					}
				})
			}
		})
	}
}

func TestRefLazyKeyAndBorrow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	loc := config.RefLocations{"input": dir}
	plain := testRefOverlay(t, dir, "input", nil, bytes.Repeat([]byte{2}, 4096))
	encrypted := testRefOverlay(t, dir, "input", refTestCodec(t), bytes.Repeat([]byte{3}, 4096))
	for _, fail := range []bool{false, true} {
		calls := 0
		keyErr := errors.New("test resolver failure")
		t.Setenv("MANIFEST_KEY", strings.Repeat("ab", 32))
		s, err := NewProcessStorageWithCustomerKey(nil, func() ([32]byte, error) {
			calls++
			if fail && calls == 1 {
				return [32]byte{}, keyErr
			}
			return refTestKey, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		b := s.Borrow()
		local := plain
		local.Location = ""
		for _, input := range []manifest.Ref{local, plain} {
			f, err := b.OpenFileWithLocations(ctx, filepath.Join(dir, input.Path), input, loc)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		if calls != 0 {
			t.Fatalf("construction, Borrow or plaintext read resolved key %d times", calls)
		}
		for _, reader := range []*ProcessStorage{s, b} {
			f, err := reader.OpenFileWithLocations(ctx, filepath.Join(dir, encrypted.Path), encrypted, loc)
			if fail != errors.Is(err, keyErr) || (!fail && err != nil) {
				t.Fatalf("encrypted read: %v", err)
			}
			if fail && (strings.Contains(err.Error(), encrypted.Path) || strings.Contains(err.Error(), keyErr.Error())) {
				t.Fatalf("unprotected read: %v", err)
			}
			if f != nil {
				f.Close()
			}
		}
		pub, err := NewLocationPublisher(b, "out", t.TempDir(), nil, nil)
		if fail != errors.Is(err, keyErr) || (!fail && err != nil) {
			t.Fatalf("writer: %v", err)
		}
		if pub != nil {
			pub.Close()
		}
		if err := errors.Join(b.Close(), s.Close()); err != nil || calls != 1 {
			t.Fatalf("calls=%d, close=%v", calls, err)
		}
	}
}

func TestStorageRejectsInvalidRefBeforeUsingKey(t *testing.T) {
	cfg := &config.ManifestConfig{Crypto: crypto.Config{Ref: "unknown"}}
	key := func() ([32]byte, error) { t.Fatal("validated crypto.ref by fetching the key"); return [32]byte{}, nil }
	if _, err := NewProcessStorageWithCustomerKey(cfg, key); err == nil {
		t.Fatal("accepted invalid ref")
	}
	if _, err := NewProcessStorageWithLocalAccess(cfg, key, nil, false); err == nil {
		t.Fatal("explicit local access accepted invalid ref")
	}
}

func TestDirectSourceChecksRetainedDiskBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	disk := testRefOverlay(t, dir, "input", nil, bytes.Repeat([]byte{1}, 4096))
	raw, _ := publishPortable(t, disk.String())
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{2}, 4096)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	storage := policyStorage(t, crypto.LocalOff, crypto.LocalRequired)
	out := t.TempDir()
	pub, err := NewLocationPublisher(storage, "out", out, config.RefLocations{"input": dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if result, err := pub.PublishSource(ctx, RoleSandbox, source); err == nil || result.Ref != "" {
		t.Fatalf("accepted plaintext disk: %+v, %v", result, err)
	}
	files, err := os.ReadDir(out)
	if err != nil || len(files) != 0 {
		t.Fatalf("partial output = %v, %v", files, err)
	}
}

func TestRefCarrierIdentityAndBoundedReuse(t *testing.T) {
	dir := t.TempDir()
	ref := testRefOverlay(t, dir, "in", refTestCodec(t), bytes.Repeat([]byte{0x39}, 4<<20))
	path := filepath.Join(dir, ref.Path)
	locations := config.RefLocations{"in": dir}
	s := policyStorage(t, crypto.LocalOff, crypto.LocalAuto)
	ctx := context.Background()
	wrong, err := NewProcessStorageWithCustomerKey(nil, func() ([32]byte, error) { return [32]byte{1}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := wrong.OpenFileWithLocations(ctx, path, ref, locations); !errors.Is(err, tarstream.ErrAuthentication) {
		t.Fatalf("wrong key: %v", err)
	}
	bad := ref
	bad.Digest = strings.Repeat("0", 64)
	if _, err := s.OpenFileWithLocations(ctx, path, bad, locations); !errors.Is(err, tarstream.ErrDigestMismatch) {
		t.Fatalf("wrong identity: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := newPublisher(s, locations, nil, nil).operation()
	if err := p.checkLocated(ctx, ref, publishScope{}); err != nil {
		t.Fatalf("policy scanned opaque payload: %v", err)
	}
	verified, err := transfer.OpenTarstream(path, tarstream.WithCodec(refTestCodec(t), true))
	if err != nil {
		t.Fatal(err)
	}
	if err := verified.Verify(ctx); !errors.Is(err, tarstream.ErrAuthentication) {
		t.Fatalf("full payload verification: %v", err)
	}
	verified.Close()
	if err := os.Truncate(path, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenFileWithLocations(ctx, path, ref, locations); err == nil {
		t.Fatal("truncated envelope accepted")
	}
}

func TestRefPolicyOnSnapshotDependenciesAndBundles(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []crypto.LocalPolicy{crypto.LocalOff, crypto.LocalRequired} {
		t.Run(string(policy), func(t *testing.T) {
			directory := t.TempDir()
			locations := config.RefLocations{"dep": directory}
			s := policyStorage(t, crypto.LocalOff, policy)
			var allowed, forbidden tarstream.Codec
			want := tarstream.ErrCodecRequired
			if policy == crypto.LocalRequired {
				allowed, want = refTestCodec(t), tarstream.ErrPlaintextForbidden
			} else {
				forbidden = refTestCodec(t)
			}
			// An opaque memory payload has historical refs that cannot be opened.
			// They must never participate in the retained graph.
			missing := "file://missing.sandbox@digest:" + strings.Repeat("a", 64) + "@location:dep"
			missingMemory := strings.ReplaceAll(missing, ".sandbox", ".snapshot")
			for _, role := range []string{"compatible", "disk", "sandbox", "memory"} {
				t.Run(role, func(t *testing.T) {
					diskCodec, eCodec, memCodec := allowed, allowed, allowed
					switch role {
					case "disk":
						diskCodec = forbidden
					case "sandbox":
						eCodec = forbidden
					case "memory":
						memCodec = forbidden
					}
					disk := testRefOverlay(t, directory, "dep", diskCodec, bytes.Repeat([]byte{6}, 8192))
					scheme := tarstream.DigestScheme
					if policy == crypto.LocalRequired {
						scheme = tarstream.DigestSchemeHMAC
					}
					disk.DigestScheme = scheme
					eRaw, _ := testRefSandbox(t, directory, disk.String(), eCodec)
					selectedE := locatedTestRef(t, eRaw, "dep")
					selectedE.DigestScheme = scheme
					e := selectedE.String()
					mRaw, _ := testRefSnapshot(t, directory, missing, []string{missingMemory}, memCodec)
					parent := locatedTestRef(t, mRaw, "dep")
					parent.DigestScheme = scheme
					m := parent.String()
					_, path := testRefSnapshot(t, t.TempDir(), e, []string{m}, nil)
					target := newPublisher(s, locations, &recordingPublishTarget{}, nil)
					res, err := target.Publish(ctx, path)
					if role == "compatible" {
						if err != nil || res.SandboxRef != e {
							t.Fatalf("retained E: %+v, %v", res, err)
						}
					} else if !errors.Is(err, want) {
						t.Fatalf("retained %s: got %v, want %v", role, err, want)
					}

					cfg, _ := manifestPublisherFixture(t)
					cfg.Crypto.Ref = policy
					bundleReader, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return refTestKey, nil })
					if err != nil {
						t.Fatal(err)
					}
					defer bundleReader.Close()
					admission, err := cfg.WriteAdmission(ctx)
					if err != nil {
						t.Fatal(err)
					}
					dir := t.TempDir()
					sink, err := snapshot.NewPlannedBundleSink(dir, "root", cfg, bundleReader.CustomerKeyFunc(), admission, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer sink.Close()
					tail, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e, FromRefs: []string{m}})
					if err != nil {
						t.Fatal(err)
					}
					source, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{1}, 8192)), []byte("{}"), []byte("{}"), tail)
					if err != nil {
						t.Fatal(err)
					}
					root, _, err := sink.AbsorbSnapshot(ctx, source)
					if err != nil {
						t.Fatal(err)
					}
					if err := sink.CommitSnapshot(ctx, root, ""); err != nil {
						t.Fatal(err)
					}
					bundlePath := filepath.Join(dir, strings.TrimPrefix(root, "manifest://")+".bundle")
					out := t.TempDir()
					pub, err := NewLocationPublisher(bundleReader, "out", out, locations, nil)
					if err != nil {
						t.Fatal(err)
					}
					_, err = pub.Publish(ctx, bundlePath)
					pub.Close()
					if role == "compatible" && err != nil || role != "compatible" && !errors.Is(err, want) {
						t.Fatalf("Bundle %s policy: %v", role, err)
					}
				})
			}
		})
	}
}

func TestDiffCheckpointUsesLocalTemporarySourcesAndRefOutput(t *testing.T) {
	for _, tc := range []struct {
		local, ref             crypto.LocalPolicy
		key                    *[32]byte
		localScheme, refScheme string
	}{
		{crypto.LocalOff, crypto.LocalRequired, nil, "digest", "hmac"},
		{crypto.LocalRequired, crypto.LocalOff, &refTestKey, "hmac", "digest"},
	} {
		t.Run(string(tc.local)+"-"+string(tc.ref), func(t *testing.T) {
			ctx := context.Background()
			f := newDiffCheckpointFixtureWithKey(t, tc.key)
			s := policyStorage(t, tc.local, tc.ref)
			materialized := t.TempDir()
			local, err := s.MaterializeCheckpoint(ctx, f.path, nil, materialized)
			if err != nil {
				t.Fatal(err)
			}
			input, err := manifest.ParseRef(local.Ref)
			if err != nil || input.Location != "" || input.DigestScheme != tc.localScheme {
				t.Fatalf("local target = %s, %v", local.Ref, err)
			}
			out := t.TempDir()
			locations := config.RefLocations{"portable": out}
			pub, err := NewLocationPublisher(s, "portable", out, locations, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer pub.Close()
			result, err := pub.Publish(ctx, f.path)
			if err != nil {
				t.Fatal(err)
			}
			e, err := manifest.ParseRef(result.Ref)
			if err != nil || e.DigestScheme != tc.refScheme {
				t.Fatalf("published ref = %s, %v", result.Ref, err)
			}
			file, err := s.OpenFileWithLocations(ctx, filepath.Join(out, e.Path), e, locations)
			if err != nil {
				t.Fatal(err)
			}
			root, err := sandboxfile.Open(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if root.Checkpoint != nil || len(root.Portable.Boot.Disks) != 2 {
				t.Fatal("checkpoint not materialized")
			}
			data := make([]byte, 4096)
			if _, err := root.Payload.ReadAt(ctx, data, 0); err != nil || !bytes.Equal(data, bytes.Repeat([]byte{1}, len(data))) {
				t.Fatalf("root data: %v", err)
			}
			for i, disk := range root.Portable.Boot.Disks {
				r, err := manifest.ParseRef(disk.Base)
				if err != nil || r.Location != "portable" || r.DigestScheme != tc.refScheme {
					t.Fatalf("disk ref = %s, %v", disk.Base, err)
				}
				file, err := s.OpenFileWithLocations(ctx, filepath.Join(out, r.Path), r, locations)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.ReadAt(ctx, data, 0)
				if err = errors.Join(err, file.Close()); err != nil || !bytes.Equal(data, bytes.Repeat([]byte{byte(i + 2)}, len(data))) {
					t.Fatalf("disk %s data: %v", disk.Name, err)
				}
			}
		})
	}
}

func TestSingleRootBundleUsesTrustedInputLocations(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	disk := testRefOverlay(t, base, "base", nil, bytes.Repeat([]byte{1}, 4096))
	raw, _ := publishPortable(t, disk.String())
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{2}, 4096)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []crypto.LocalPolicy{crypto.LocalOff, crypto.LocalAuto, crypto.LocalRequired} {
		t.Run(string(policy), func(t *testing.T) {
			cfg, key := sourceBundleConfig(crypto.LocalOff)
			cfg.Crypto.Ref = policy
			admission, err := cfg.WriteAdmission(ctx)
			if err != nil {
				t.Fatal(err)
			}
			output := t.TempDir()
			inputs := config.RefLocations{"base": base}
			keyFn := func() ([32]byte, error) { return key, nil }
			pub, err := NewSingleRootBundlePublisherWithLocations(cfg, keyFn, admission, "release", output, inputs, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer pub.Close()
			delete(inputs, "base")
			result, err := pub.PublishSource(ctx, RoleSandbox, source)
			if policy == crypto.LocalRequired {
				if err == nil || result.Ref != "" {
					t.Fatalf("required retained plaintext disk: %+v, %v", result, err)
				}
				files, err := os.ReadDir(output)
				if err != nil || len(files) != 0 {
					t.Fatalf("failed publication left files: %v, %v", files, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ref, err := manifest.ParseRef(result.Ref)
			if err != nil || ref.Location != "release" || ref.DigestScheme != "manifest" {
				t.Fatalf("bundle: %s, %v", result.Ref, err)
			}
			storage, err := NewProcessStorageWithCustomerKey(cfg, keyFn)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			doc, err := storage.Inspect(ctx, result.Ref, config.RefLocations{"release": output, "base": base})
			if err != nil || doc.Sandbox.Boot.Root.BaseFromRefs[0] != disk.String() {
				t.Fatalf("external disk: %v, %v", doc, err)
			}
		})
	}
}
