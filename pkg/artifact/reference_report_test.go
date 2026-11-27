package artifact

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func reportRef(char string) string { return "manifest://" + strings.Repeat(char, 64) }

func TestReferenceReportCompleteAndUnknownWireContracts(t *testing.T) {
	e, s, layer, old := reportRef("a"), reportRef("b"), reportRef("c"), reportRef("d")
	for _, tc := range []struct {
		name   string
		result PublishResult
		fields int
	}{
		{"E", PublishResult{Role: RoleSandbox, Ref: e, Refs: true, SandboxRefs: []string{layer, e, e}, RemovedRefs: []string{}}, 4},
		{"S", PublishResult{Role: RoleSnapshot, Ref: s, SandboxRef: e, Refs: true, SnapshotRefs: []string{s, layer}, SandboxRefs: []string{e, layer}, RemovedRefs: []string{old}}, 6},
		{"unknown-E", PublishResult{Role: RoleSandbox, Ref: e, RemovedRefs: []string{old}}, 2},
		{"unknown-S", PublishResult{Role: RoleSnapshot, Ref: s, SandboxRef: e, RemovedRefs: []string{}}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := tc.result.Report()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var shape map[string]json.RawMessage
			if err := json.Unmarshal(data, &shape); err != nil {
				t.Fatal(err)
			}
			if len(shape) != tc.fields {
				t.Fatalf("shape=%s", data)
			}
			if (shape["removedRefs"] != nil) != tc.result.Refs {
				t.Fatalf("delta presence=%s", data)
			}
			if tc.result.Refs && len(tc.result.RemovedRefs) == 0 && string(shape["removedRefs"]) != "[]" {
				t.Fatalf("known empty delta=%s", data)
			}
			decoded, err := DecodePublishReport(data, tc.result.Role)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, report) {
				t.Fatalf("round trip=%+v want=%+v", decoded, report)
			}
			wrapped, err := json.Marshal(struct {
				Result string `json:"result"`
				PublishReport
			}{"token", report})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(wrapped), `"result":"token"`) {
				t.Fatalf("embedded report hid platform result: %s", wrapped)
			}
		})
	}
}

func TestReferenceReportLegacyNormalizationAndCaptureIsolation(t *testing.T) {
	e, old := reportRef("a"), reportRef("b")
	legacy := fmt.Sprintf(`{"sandboxRef":%q,"removedRefs":[%q]}`, e, old)
	report, err := DecodePublishReport([]byte(legacy), RoleSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if report.Refs || report.RemovedRefs != nil || report.SandboxRefs != nil {
		t.Fatalf("legacy delta escaped: %+v", report)
	}
	wire, _ := json.Marshal(report)
	if string(wire) != fmt.Sprintf(`{"refs":false,"sandboxRef":%q}`, e) {
		t.Fatalf("normalized=%s", wire)
	}
	capture, err := (PublishResult{Role: RoleSandbox, Ref: e}).CaptureReport()
	if err != nil {
		t.Fatal(err)
	}
	wire, _ = json.Marshal(capture)
	if string(wire) != fmt.Sprintf(`{"sandboxRef":%q,"removedRefs":[]}`, e) {
		t.Fatalf("capture output changed: %s", wire)
	}
	decoded, err := DecodeCaptureReport(wire, RoleSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RemovedRefs == nil {
		t.Fatal("capture lost known empty legacy array")
	}
	if _, err := DecodeCaptureReport([]byte(legacy), RoleSandbox); err == nil {
		t.Fatal("capture accepted delta")
	}
	if _, err := DecodeCaptureReport([]byte(fmt.Sprintf(`{"refs":false,"sandboxRef":%q}`, e)), RoleSandbox); err == nil {
		t.Fatal("capture silently adopted new shape")
	}
}

func TestReferenceReportRejectsInvalidPresenceAndStructure(t *testing.T) {
	e, other := reportRef("a"), reportRef("b")
	valid := fmt.Sprintf(`{"refs":true,"sandboxRef":%q,"sandboxRefs":[%q],"removedRefs":[]}`, e, e)
	invalid := []string{
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"removedRefs":[]}`, e),
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"removedRefs":null}`, e),
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"removedRefs":[%q]}`, e, other),
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"sandboxRefs":[]}`, e),
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"snapshotRefs":null}`, e),
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"diskRefs":[]}`, e),
		fmt.Sprintf(`{"refs":false,"sandboxRef":%q,"diskRef":%q}`, e, other),
		strings.Replace(valid, `"refs":true`, `"refs":null`, 1),
		strings.Replace(valid, `"refs":true`, `"refs":"true"`, 1),
		strings.Replace(valid, `"refs":true`, `"refs":true,"refs":true`, 1),
		strings.Replace(valid, `,"removedRefs":[]`, "", 1),
		strings.Replace(valid, `"removedRefs":[]`, `"removedRefs":null`, 1),
		strings.Replace(valid, `"removedRefs":[]`, fmt.Sprintf(`"removedRefs":[%q]`, e), 1),
		strings.Replace(valid, fmt.Sprintf(`"sandboxRefs":[%q]`, e), `"sandboxRefs":null`, 1),
		strings.Replace(valid, fmt.Sprintf(`"sandboxRefs":[%q]`, e), `"sandboxRefs":[]`, 1),
		strings.Replace(valid, fmt.Sprintf(`"sandboxRefs":[%q]`, e), fmt.Sprintf(`"sandboxRefs":[%q]`, other), 1),
		strings.Replace(valid, fmt.Sprintf(`"sandboxRefs":[%q]`, e), fmt.Sprintf(`"sandboxRefs":[%q,%q]`, other, e), 1),
		strings.Replace(valid, fmt.Sprintf(`"sandboxRefs":[%q]`, e), fmt.Sprintf(`"sandboxRefs":[%q,%q]`, e, e), 1),
		strings.Replace(valid, `"refs":true,`, "", 1),
		valid + ` {}`,
		valid + "\nprogress",
	}
	for i, data := range invalid {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := DecodePublishReport([]byte(data), RoleSandbox); err == nil {
				t.Fatalf("accepted %s", data)
			}
		})
	}
	if _, err := DecodePublishReport([]byte(valid), RoleSnapshot); err == nil {
		t.Fatal("accepted wrong role")
	}
}

func TestDiskReportPresenceAndRole(t *testing.T) {
	base, parent := reportRef("a"), reportRef("b")
	r := DiskExportResult{DiskRefs: []string{parent, base, base}, RemovedRefs: []string{}}
	report, err := r.Report()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDiskReport(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, report) {
		t.Fatal("disk report changed on decode")
	}
	if !strings.Contains(string(wire), `"removedRefs":[]`) || strings.Contains(string(wire), `"diskRef":`) {
		t.Fatalf("disk shape=%s", wire)
	}
	unknown, err := DecodeDiskReport([]byte(`{"refs":false}`))
	if err != nil || unknown.Refs {
		t.Fatalf("unknown=%+v %v", unknown, err)
	}
	for _, data := range []string{
		`{"refs":false,"removedRefs":[]}`,
		`{"refs":false,"removedRefs":null}`,
		`{"refs":false,"diskRefs":[]}`,
		fmt.Sprintf(`{"refs":true,"diskRef":%q,"diskRefs":[%q],"removedRefs":[]}`, base, base),
		fmt.Sprintf(`{"refs":true,"sandboxRef":%q,"diskRefs":[%q],"removedRefs":[]}`, base, base),
		`{"refs":true,"diskRefs":[],"removedRefs":[]}`,
		fmt.Sprintf(`{"refs":true,"diskRefs":[%q],"removedRefs":null}`, base),
		fmt.Sprintf(`{"refs":true,"diskRefs":[%q],"removedRefs":[%q]}`, base, base),
		fmt.Sprintf(`{"diskRefs":[%q],"removedRefs":[]}`, base),
	} {
		if _, err := DecodeDiskReport([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
