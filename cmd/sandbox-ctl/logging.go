package main

import (
	"io"
	"log"
	"os"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/pkg/journalio"
)

// componentLog owns only the run command's diagnostics, not fd 2, guest stderr,
// CH stderr, or exec's error capture. Explicit targets do not auto-inherit fields.
func componentLog(to string) (io.Writer, func(), error) {
	if to == "" || to == "default" {
		return os.Stderr, func() {}, nil
	}
	writer, err := journalio.Open(to, os.Stderr)
	if err != nil {
		return nil, nil, err
	}
	return writer, installComponentLogger(writer), nil
}

// The CLI owns this process-wide installation. It defers cleanup before other
// run resources, so producers and their terminal diagnostics finish first.
func installComponentLogger(writer io.WriteCloser) func() {
	previous := log.Writer()
	log.SetOutput(writer)
	var once sync.Once
	return func() {
		once.Do(func() {
			log.SetOutput(previous)
			_ = writer.Close()
		})
	}
}
