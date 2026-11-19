package artifact

import (
	"context"
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

type CaptureRecord = memorycheckpoint.CaptureRecord

func ReadCheckpointCapture(ctx context.Context, directory string, producer CurrentRun, checkpointID string) (*CaptureRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := memorycheckpoint.ReadRecord(directory, checkpointID)
	if err != nil {
		return nil, err
	}
	if r.Producer.SandboxID != producer.SandboxID || r.Producer.InstanceID != producer.InstanceID || r.Producer.RunID != producer.RunID {
		return nil, fmt.Errorf("checkpoint capture producer identity mismatch")
	}
	return r, nil
}

func DiscardCheckpointCapture(ctx context.Context, directory string, producer CurrentRun, checkpointID string) (*CaptureRecord, error) {
	return memorycheckpoint.DiscardCapture(ctx, directory, memorycheckpoint.RunIdentity{SandboxID: producer.SandboxID, InstanceID: producer.InstanceID, RunID: producer.RunID}, checkpointID)
}
