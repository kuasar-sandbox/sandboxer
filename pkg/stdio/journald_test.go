package stdio

import (
	"os/exec"
	"reflect"
	"testing"
)

func TestFromFlags_JournaldStreams(t *testing.T) {
	m, err := FromFlags(nil, nil, nil, "", "journald=app,WORKLOAD_ID=w-1,STREAM=stdout", "journald=app,STREAM=stderr", boolp(false), "journald=console")
	if err != nil {
		t.Fatal(err)
	}
	if m.Stdout.Kind != StreamJournald || m.Stdout.Journal.Tag != "app" || !reflect.DeepEqual(m.Stdout.Journal.Fields, map[string]string{"WORKLOAD_ID": "w-1", "STREAM": "stdout"}) {
		t.Fatalf("stdout=%+v", m.Stdout)
	}
	if m.Stderr.Kind != StreamJournald || m.Stderr.Journal.Tag != "app" || !reflect.DeepEqual(m.Stderr.Journal.Fields, map[string]string{"STREAM": "stderr"}) {
		t.Fatalf("stderr=%+v", m.Stderr)
	}
	if m.Console.Journal == nil || len(m.Console.Journal.Fields) != 0 {
		t.Fatal("console inherited fields")
	}
	m.Stdout.Journal.Fields["STREAM"] = "changed"
	if m.Stderr.Journal.Fields["STREAM"] != "stderr" {
		t.Fatal("same tag shared fields")
	}
	if !m.ProtoSpec().Stdout || !m.ProtoSpec().Stderr || m.ProtoSpec().TTY {
		t.Fatal("journal changed guest stream protocol")
	}
	for _, path := range []string{"/tmp/out.img", "/tmp/a,b=c%2C", "file=x,y=z%"} {
		m, err = FromFlags(nil, nil, nil, "", path, "", boolp(false), "off")
		if err != nil || m.Stdout.Kind != StreamFile || m.Stdout.Path != path {
			t.Fatalf("path=%q m=%+v err=%v", path, m, err)
		}
	}
}

func TestParseConsole_Journald(t *testing.T) {
	c, err := parseConsole("journald=console,WORKLOAD_ID=a%2Cb,EMPTY=")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != ConsoleJournald || c.Journal.Tag != "console" || c.Journal.Fields["WORKLOAD_ID"] != "a,b" {
		t.Fatalf("console=%+v", c)
	}
	for _, bad := range []string{"journald=", "journald=bad tag", "journald=a/b", "journald=x,A=1,A=2", "journald=x,A=%", "journald=x,_PID=1"} {
		if _, err := parseConsole(bad); err == nil {
			t.Errorf("console accepted %q", bad)
		}
		if _, err := parseStreamTarget(bad); err == nil {
			t.Errorf("stream accepted %q", bad)
		}
	}
	c, err = parseConsole("file=/tmp/a,b=c%2C")
	if err != nil || c.Kind != ConsoleFile || c.Path != "/tmp/a,b=c%2C" {
		t.Fatal(c, err)
	}
}

func TestJournalOutputsWithoutFieldsRemainIndependent(t *testing.T) {
	m, err := FromFlags(nil, nil, nil, "", "journald=app", "journald=app", boolp(false), "journald=app")
	if err != nil {
		t.Fatal(err)
	}
	if m.Stdout.Journal == m.Stderr.Journal || m.Stdout.Journal == m.Console.Journal {
		t.Fatal("shared targets")
	}
	if len(m.Stdout.Journal.Fields) != 0 || len(m.Stderr.Journal.Fields) != 0 || len(m.Console.Journal.Fields) != 0 {
		t.Fatal("unexpected fields")
	}
	cmd := &exec.Cmd{}
	arg, close, err := m.SetupCHStdio(cmd)
	if err != nil || arg != "tty" || cmd.Stdout == nil {
		t.Fatalf("console open: %s %v", arg, err)
	}
	close()
}

func TestMissingJournalTargetIsRejected(t *testing.T) {
	if _, err := openJournal(nil); err == nil {
		t.Fatal("missing target accepted")
	}
}

func boolp(b bool) *bool { return &b }
