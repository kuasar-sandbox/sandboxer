package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
)

// DiskReport describes the complete immutable layer set of one exported disk.
// DiskV1 remains the ordered recovery description; there is no separate disk root.
type DiskReport struct {
	Refs        bool     `json:"refs"`
	DiskRefs    []string `json:"diskRefs,omitempty"`
	RemovedRefs []string `json:"removedRefs,omitzero"`
}

// DiskExportResult keeps the disk and operation metadata from the same upload.
// References retain their full source scope until Report projects public names.
type DiskExportResult struct {
	Disk        DiskV1
	DiskRefs    []string
	RemovedRefs []string
}

func (r DiskExportResult) Report() (DiskReport, error) {
	out := DiskReport{Refs: true}
	var err error
	out.DiskRefs, err = publicReferences(r.DiskRefs)
	if err != nil {
		return DiskReport{}, err
	}
	out.RemovedRefs, err = publicReferences(r.RemovedRefs)
	if err != nil {
		return DiskReport{}, err
	}
	if err := out.Validate(); err != nil {
		return DiskReport{}, err
	}
	return out, nil
}

func (r DiskReport) Validate() error {
	if !r.Refs {
		if r.DiskRefs != nil || r.RemovedRefs != nil {
			return errors.New("disk report: refs:false must omit diskRefs and removedRefs")
		}
		return nil
	}
	if err := validateReferenceSet("diskRefs", r.DiskRefs, true); err != nil {
		return err
	}
	if err := validateReferenceSet("removedRefs", r.RemovedRefs, false); err != nil {
		return err
	}
	for _, raw := range r.RemovedRefs {
		if containsReference(r.DiskRefs, raw) {
			return errors.New("disk report: removed reference is still in the final disk")
		}
	}
	return nil
}

// DecodeDiskReport accepts exactly one new-format disk report. It never
// accepts diskRef, S/E fields, partial sets, duplicate keys or trailing output.
func DecodeDiskReport(data []byte) (DiskReport, error) {
	var out DiskReport
	fields, err := decodeReportFields(data)
	if err != nil {
		return out, err
	}
	if fields["refs"] == nil || bytes.Equal(bytes.TrimSpace(fields["refs"]), []byte("null")) {
		return out, errors.New("disk report: refs must be a boolean")
	}
	if err := json.Unmarshal(fields["refs"], &out.Refs); err != nil {
		return out, err
	}
	allowed := []string{"refs"}
	if out.Refs {
		allowed = append(allowed, "diskRefs", "removedRefs")
	}
	if err := exactReportFields(fields, allowed); err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if err := out.Validate(); err != nil {
		return DiskReport{}, err
	}
	return out, nil
}
