package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

// templateCarriers packages a prepared raw test filesystem through the real
// carrier writers. This helper is built before E2E preparation; guest cases
// never build code or substitute a hand-written tar/Bundle envelope.
func templateCarriers(raw, directory string, output io.Writer) (retErr error) {
	ctx := context.Background()
	raw, err := filepath.Abs(raw)
	if err != nil {
		return err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	var storage *artifact.ProcessStorage
	source, err := storage.OpenDiffTemplate(ctx, "file://"+raw, "", nil)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	// Public, deterministic fixture key; never taken from an operator account.
	key := [32]byte{0x52, 0x34, 0x38}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		return err
	}
	refs := map[string]string{"raw": "file://" + raw, "customer_key": hex.EncodeToString(key[:])}
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		sink := snapshot.NewFileSink(directory, name, nil, false, nil)
		if encrypted {
			name = "encrypted"
			sink = snapshot.NewFileSink(directory, name, codec, true, nil)
		}
		rawRef, _, err := sink.AbsorbOverlaySource(ctx, source)
		err = errors.Join(err, sink.Close())
		if err != nil {
			return err
		}
		ref, err := manifest.ParseRef(rawRef)
		if err != nil {
			return err
		}
		ref.Path = filepath.Join(directory, ref.Path)
		refs[name] = ref.String()
	}
	cfg := &manifest.Config{Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"}, Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes"}}
	sink, err := snapshot.NewBundleSink(ctx, directory, "template", cfg, func() ([32]byte, error) { return key, nil }, nil)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, sink.Close()) }()
	selected, _, err := sink.AbsorbOverlaySource(ctx, source)
	if err != nil {
		return err
	}
	path, err := sink.CommitDisk(ctx, []string{selected})
	if err != nil {
		return err
	}
	ref, err := manifest.ParseRef(selected)
	if err != nil {
		return err
	}
	refs["bundle"] = (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: "manifest", Digest: ref.Path}).String()
	return json.NewEncoder(output).Encode(refs)
}
