package restore

import (
	"slices"
	"testing"
)

func TestRestoreCHArgv(t *testing.T) {
	const chSock = "/run/sb/ch.sock"
	const restoreArg = "source_url=file:///run/sb/snap-state,net_fds=[_net0@[4]]"
	fixed := []string{"--api-socket", chSock, "--restore", restoreArg}

	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"no extras", nil},
		{"extras appended verbatim", []string{"-vv", "--log-file", "/tmp/ch.log"}},
		{"= joined flag stays one token", []string{"--log-file=/tmp/ch.log"}},
		{"value containing whitespace stays one token", []string{"/tmp/a b.log"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreCHArgv(chSock, restoreArg, tc.extra)
			want := append([]string{}, fixed...)
			want = append(want, tc.extra...)
			if !slices.Equal(got, want) {
				t.Fatalf("restoreCHArgv() = %v\n  want %v", got, want)
			}
			// --restore takes exactly one value: the token right after it
			// must be the restore arg itself, never an extra.
			if i := slices.Index(got, "--restore"); got[i+1] != restoreArg {
				t.Fatalf("token after --restore = %q, want the restore arg", got[i+1])
			}
		})
	}
}
