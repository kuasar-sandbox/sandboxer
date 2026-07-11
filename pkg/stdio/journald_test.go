package stdio

import (
	"strings"
	"testing"
)

func TestFromFlags_JournaldStreams(t *testing.T) {
	m, err := FromFlags(nil, nil, nil, "", "journald=build", "journald=build", boolp(false), "off")
	if err != nil {
		t.Fatal(err)
	}
	if m.Stdout.Kind != StreamJournald || m.Stdout.Tag != "build" {
		t.Errorf("stdout = %+v, want journald tag=build", m.Stdout)
	}
	if m.Stderr.Kind != StreamJournald || m.Stderr.Tag != "build" {
		t.Errorf("stderr = %+v, want journald tag=build", m.Stderr)
	}
	// A plain path still resolves to a file.
	m, err = FromFlags(nil, nil, nil, "", "/tmp/out.img", "", boolp(false), "off")
	if err != nil {
		t.Fatal(err)
	}
	if m.Stdout.Kind != StreamFile || m.Stdout.Path != "/tmp/out.img" {
		t.Errorf("stdout = %+v, want file /tmp/out.img", m.Stdout)
	}
}

func TestParseConsole_Journald(t *testing.T) {
	c, err := parseConsole("journald=console")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != ConsoleJournald || c.Tag != "console" {
		t.Errorf("console = %+v, want journald tag=console", c)
	}
	for _, bad := range []string{"journald=", "journald=bad tag", "journald=a/b"} {
		if _, err := parseConsole(bad); err == nil {
			t.Errorf("parseConsole(%q): want error", bad)
		}
	}
}

// journaldWriter falls back to a prefixed stderr when journald is unavailable;
// drive that path directly to assert line framing (split on \n, strip \r,
// drop empty lines, flush the partial tail on Close).
func TestJournaldWriterLineFraming(t *testing.T) {
	var sink strings.Builder
	w := &journaldWriter{tag: "build", fallback: &sink}

	w.Write([]byte("pull: 1/3 layers\npull: 2/3 lay"))
	w.Write([]byte("ers\r\n\nflatten: build-erofs"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := sink.String()
	want := "[build] pull: 1/3 layers\n[build] pull: 2/3 layers\n[build] flatten: build-erofs\n"
	if got != want {
		t.Errorf("framed output:\n%q\nwant:\n%q", got, want)
	}
}

func TestJournaldWriterKuasarIdentityFields(t *testing.T) {
	t.Setenv("KUASAR_RUN_ID", "sr-test")
	t.Setenv("KUASAR_SANDBOX_ID", "sandbox-test")
	t.Setenv("KUASAR_BUILD_ID", "")

	w := newJournaldWriter("sandbox")
	if got := w.fields["SYSLOG_IDENTIFIER"]; got != "sandbox" {
		t.Fatalf("SYSLOG_IDENTIFIER = %q", got)
	}
	if got := w.fields["KUASAR_RUN_ID"]; got != "sr-test" {
		t.Fatalf("KUASAR_RUN_ID = %q", got)
	}
	if got := w.fields["KUASAR_SANDBOX_ID"]; got != "sandbox-test" {
		t.Fatalf("KUASAR_SANDBOX_ID = %q", got)
	}
	if _, ok := w.fields["KUASAR_BUILD_ID"]; ok {
		t.Fatal("empty KUASAR_BUILD_ID should be omitted")
	}
}

func boolp(b bool) *bool { return &b }
