// Package wireio contains shared primitives for writing complete buffers.
package wireio

import "io"

// WriteAll writes every byte in p. It returns io.ErrShortWrite when the
// writer reports success without making progress.
func WriteAll(w io.Writer, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	_, err := WriteFull(w.Write, p)
	return err
}

// WriteFull retries short writes to consume p, reporting the total bytes written
// even if a later write fails. A write that makes no progress must report an error.
func WriteFull(write func([]byte) (int, error), p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, err := write(p[written:])
		if n <= 0 || n > len(p)-written {
			if err == nil {
				err = io.ErrShortWrite
			}
			return written, err
		}
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
