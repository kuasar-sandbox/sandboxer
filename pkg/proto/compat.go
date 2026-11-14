// Package proto preserves the sandboxer import path for the shared Guest ABI.
//
// Deprecated: use github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto.
package proto

import (
	"io"

	abi "github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

const (
	VsockHostCID          = abi.VsockHostCID
	VsockGuestCID         = abi.VsockGuestCID
	LaunchPort            = abi.LaunchPort
	MaxMessageBytes       = abi.MaxMessageBytes
	DiscardAuto           = abi.DiscardAuto
	DiscardManual         = abi.DiscardManual
	AppStateRunning       = abi.AppStateRunning
	AppStateExited        = abi.AppStateExited
	InitialLaunchPending  = abi.InitialLaunchPending
	InitialLaunchStarted  = abi.InitialLaunchStarted
	InitialLaunchFailed   = abi.InitialLaunchFailed
	InitialLaunchExited   = abi.InitialLaunchExited
	DropCachesUnknown     = abi.DropCachesUnknown
	DropCachesSkipped     = abi.DropCachesSkipped
	DropCachesSucceeded   = abi.DropCachesSucceeded
	DropCachesFailed      = abi.DropCachesFailed
	TypeDiskBind          = abi.TypeDiskBind
	TypeDiskBindAck       = abi.TypeDiskBindAck
	TypeHello             = abi.TypeHello
	TypeLaunch            = abi.TypeLaunch
	TypeLaunchAck         = abi.TypeLaunchAck
	TypeAppStarted        = abi.TypeAppStarted
	TypeAppExited         = abi.TypeAppExited
	TypeShutdown          = abi.TypeShutdown
	TypePing              = abi.TypePing
	TypePong              = abi.TypePong
	TypeRestore           = abi.TypeRestore
	TypeRestoreAck        = abi.TypeRestoreAck
	TypeAttach            = abi.TypeAttach
	TypeAttachAck         = abi.TypeAttachAck
	TypeQuiesce           = abi.TypeQuiesce
	TypeQuiesced          = abi.TypeQuiesced
	TypeExec              = abi.TypeExec
	TypeExecAck           = abi.TypeExecAck
	TypeConnect           = abi.TypeConnect
	TypeConnectAck        = abi.TypeConnectAck
	TypeMemReport         = abi.TypeMemReport
	TypeMemReportAck      = abi.TypeMemReportAck
	TypeAck               = abi.TypeAck
	TypeError             = abi.TypeError
	DeadlineAppNotify     = abi.DeadlineAppNotify
	DeadlinePing          = abi.DeadlinePing
	DeadlineQuiesce       = abi.DeadlineQuiesce
	DeadlineAttach        = abi.DeadlineAttach
	DeadlineExec          = abi.DeadlineExec
	DeadlineConnect       = abi.DeadlineConnect
	TypeUsageRequest      = abi.TypeUsageRequest
	TypeUsageResponse     = abi.TypeUsageResponse
	MaxUsageFilesystems   = abi.MaxUsageFilesystems
	MaxUsageRunEpochBytes = abi.MaxUsageRunEpochBytes
	UsageOK               = abi.UsageOK
	UsageBusy             = abi.UsageBusy
	UsageTimeout          = abi.UsageTimeout
	UsageUnsupported      = abi.UsageUnsupported
	UsageInvalid          = abi.UsageInvalid
	UsageError            = abi.UsageError
)

type (
	DiskBinding        = abi.DiskBinding
	LaunchSpec         = abi.LaunchSpec
	MountSpec          = abi.MountSpec
	FileSpec           = abi.FileSpec
	InitSpec           = abi.InitSpec
	PluginSpec         = abi.PluginSpec
	NetworkSpec        = abi.NetworkSpec
	StdioSpec          = abi.StdioSpec
	Winsize            = abi.Winsize
	ExecSpec           = abi.ExecSpec
	ConnectSpec        = abi.ConnectSpec
	InitialLaunchState = abi.InitialLaunchState
	Message            = abi.Message
	MemReport          = abi.MemReport
	DropCachesResult   = abi.DropCachesResult
	UsageRequest       = abi.UsageRequest
	UsageReadState     = abi.UsageReadState
	UsageMemory        = abi.UsageMemory
	UsageFilesystem    = abi.UsageFilesystem
	UsageResponse      = abi.UsageResponse
)

var (
	HostConnectLine = abi.HostConnectLine
)

func WriteMessage(w io.Writer, m *Message) error { return abi.WriteMessage(w, m) }

func ReadMessage(r io.Reader) (*Message, error) { return abi.ReadMessage(r) }
