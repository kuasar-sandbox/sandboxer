// artifactref reads the declared identity of a local plaintext test artifact.
// It uses the product's canonical sparse-carrier parser, not tar member listing.
// This validates carrier metadata; it does not rehash every payload byte.
package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"syscall"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func reference(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("artifact must be a regular file")
	}
	source, _, err := tarstream.SourceAt(f, info.Size(), "")
	if err != nil {
		return "", err
	}
	digester, ok := source.(tarstream.Digester)
	if !ok {
		return "", fmt.Errorf("artifact has no canonical digest marker")
	}
	scheme, digest := digester.Digest()
	if scheme != tarstream.DigestScheme || !hexDigest.MatchString(digest) {
		return "", fmt.Errorf("artifact must have a plaintext carrier digest")
	}
	return scheme + ":" + digest, nil
}

func run(args []string, output io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: artifact-ref ARTIFACT")
	}
	ref, err := reference(args[0])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, ref)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
