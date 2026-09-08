package journalio

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func terminalFallback(out io.Writer) io.Writer {
	file, ok := out.(*os.File)
	if !ok {
		return out
	}
	if file == nil {
		return nil
	}
	return &terminalFallbackWriter{out: out, raw: func() bool {
		state, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
		return err == nil && state.Oflag&unix.OPOST == 0
	}}
}
