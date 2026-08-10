//go:build ignore

package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
)

type entryContract struct {
	typeflag byte
	mode     int64
}

var archiveContract = map[string]entryContract{
	"./":                     {typeflag: tar.TypeDir, mode: 0o755},
	"./bin/":                 {typeflag: tar.TypeDir, mode: 0o755},
	"./bin/cloud-hypervisor": {typeflag: tar.TypeReg, mode: 0o755},
	"./bin/sandbox-ctl":      {typeflag: tar.TypeReg, mode: 0o755},
	"./bin/sandbox-init":     {typeflag: tar.TypeReg, mode: 0o755},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: release-archive-validator <archive>")
		os.Exit(2)
	}
	if err := validateArchive(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "release archive: %v\n", err)
		os.Exit(1)
	}
}

func validateArchive(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	seen := make(map[string]struct{}, len(archiveContract))
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		contract, ok := archiveContract[header.Name]
		if !ok {
			return fmt.Errorf("unexpected member %q", header.Name)
		}
		if _, ok := seen[header.Name]; ok {
			return fmt.Errorf("duplicate member %q", header.Name)
		}
		seen[header.Name] = struct{}{}

		if !matchesType(header.Typeflag, contract.typeflag) {
			return fmt.Errorf("member %q has type %q, want %q", header.Name, header.Typeflag, contract.typeflag)
		}
		if header.Mode != contract.mode {
			return fmt.Errorf("member %q has mode %#o, want %#o", header.Name, header.Mode, contract.mode)
		}
		if header.Uid != 0 || header.Gid != 0 {
			return fmt.Errorf("member %q has owner %d/%d, want 0/0", header.Name, header.Uid, header.Gid)
		}
		if header.Uname != "" || header.Gname != "" {
			return fmt.Errorf("member %q stores owner names %q/%q", header.Name, header.Uname, header.Gname)
		}
		if header.Linkname != "" {
			return fmt.Errorf("member %q stores link target %q", header.Name, header.Linkname)
		}
	}

	for name := range archiveContract {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("missing member %q", name)
		}
	}
	return nil
}

func matchesType(actual, expected byte) bool {
	if expected == tar.TypeReg {
		return actual == tar.TypeReg || actual == tar.TypeRegA
	}
	return actual == expected
}
