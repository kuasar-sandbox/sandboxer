// Package runtime is the first-class sandbox runtime SDK. Lifecycle calls use
// operation contexts; successfully returned runtimes live until exit or Close.
package runtime

import (
	"context"

	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

type Runtime = sandbox.Runtime
type RuntimeSpec = sandbox.RuntimeSpec
type RuntimeDiskSpec = sandbox.RuntimeDiskSpec
type NetworkDeviceSpec = sandbox.NetworkDeviceSpec
type GuestNetworkSpec = sandbox.GuestNetworkSpec
type LaunchSpec = sandbox.LaunchSpec
type SandboxSpec = sandbox.SandboxSpec
type ArtifactCredentials = sandbox.ArtifactCredentials
type ExitResult = sandbox.ExitResult

// RestoreSpec describes snapshot reconstruction, including explicit artifact
// credentials and host bindings. Fetcher is borrowed until Runtime.Wait returns.
type RestoreSpec restore.Options

func StartRuntime(ctx context.Context, spec RuntimeSpec) (*Runtime, error) {
	return sandbox.StartRuntime(ctx, spec)
}
func Start(ctx context.Context, spec SandboxSpec) (*Runtime, error) { return sandbox.Start(ctx, spec) }

// Restore reconstructs the snapshot directly. It never cold-boots a base VM,
// and uses the existing restore ACK/MUX protocol for older snapshots as well.
func Restore(ctx context.Context, spec RestoreSpec) (*Runtime, error) {
	return sandbox.RunLifecycle(ctx, func(life context.Context) (int, error) { return restore.Run(life, restore.Options(spec)) })
}
