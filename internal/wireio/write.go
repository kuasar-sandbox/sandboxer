// Package wireio contains I/O primitives shared by the repository's framed protocols.
package wireio

import "io"

// WriteAll writes every byte in p. It returns io.ErrShortWrite when the
// writer reports success without making progress.
func WriteAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n < 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
