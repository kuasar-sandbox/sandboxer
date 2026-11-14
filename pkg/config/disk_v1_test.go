package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiskV1StrictSourceAndIdentity(t *testing.T) {
	base := strings.Repeat("a", 64)
	parent := strings.Repeat("b", 64)
	parse := func(raw string) DiskV1 {
		t.Helper()
		var v DiskV1
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	tmpl := parse(`{"kind":"disk","version":1,"diff_template":"file://empty.ext4@location:seeds"}`)
	if tmpl.Size != nil {
		t.Fatal("size unexpectedly required")
	}
	for _, raw := range []string{`null`, `"manifest://x"`, `{"kind":"disk","version":1,"size":4096}`, `{"kind":"disk","version":1,"version":1,"diff_template":"file:///t"}`, `{"kind":"disk","version":1,"diff_template":"file:///t","size":null}`, `{"kind":"disk","version":1,"diff_template":"file://relative"}`, `{"kind":"disk","version":1,"diff_template":"file:///t","base_from_refs":["x"]}`, `{"Kind":"disk","version":1,"diff_template":"file:///t"}`} {
		var d DiskV1
		if err := json.Unmarshal([]byte(raw), &d); err == nil {
			t.Errorf("accepted invalid input %s", raw)
		}
	}
	d := DiskV1{Kind: "disk", Version: 1, Base: "manifest://" + base, BaseFromRefs: []string{"manifest://" + parent}}
	copy := d
	copy.Base = "file://d.bundle@manifest:" + base + "@location:n"
	if d.Identity() != copy.Identity() {
		t.Fatal("relocated layer identity changed")
	}
	copy.BaseFromRefs = nil
	if d.Identity() == copy.Identity() {
		t.Fatal("parent not included in identity")
	}
}
