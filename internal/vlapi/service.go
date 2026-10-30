package vlapi

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

const ServiceName = "APIService"

// ConfigSignals are the two ways the API tells the cloud client that
// streamers.toml changed. Resubscribe re-runs the whole subscription cycle and
// is only for a change to WHICH streamers are tracked. Reload re-reads the file
// and applies it locally, for a change to HOW a tracked streamer is recorded.
type ConfigSignals struct {
	Resubscribe chan struct{}
	Reload      chan struct{}
}

// resubscribe asks for a new subscription; reload asks for a config re-read.
// Both are coalescing: a signal dropped because one is already queued loses
// nothing, since the queued handler reads the file rather than the signal.
func (s *Service) resubscribe() { signal(s.signals.Resubscribe) }
func (s *Service) reload()      { signal(s.signals.Reload) }

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// AddedRecorder records when a streamer was added.
type AddedRecorder interface {
	MarkAdded(ctx context.Context, key streamer.StreamerKey, at time.Time)
}

type Service struct {
	appConfig       vlconfig.AppConfig
	streamersFile   *vlconfig.StreamersFile
	configCtl       configController
	vodsCtl         VODController
	encodeCtl       EncodeController
	logger          *vllog.FileLogger
	ShutdownCh      chan struct{}
	ForceShutdownCh chan struct{}
	signals         ConfigSignals
	statusReqPriCh  chan streamer.StreamerRef
	kv              jetstream.KeyValue
	natsURL         string
	intervalsFn     intervalsFunc
	streamerFuncs   StreamerFuncs
	cookies         *platformauth.Store // Local, encrypted platform logins (PRIVACY: never leaves this machine)
	added           AddedRecorder       // nil records no added times
	// diskRoots reports free space per Storage root. Installed by the daemon
	// after startup rather than passed to Init, since only the daemon can
	// resolve per-streamer archive folders. nil omits the numbers from info.
	diskRoots func(context.Context) []diskspace.Root
	// reportCtl handles error and bug reports. Installed by the daemon after
	// startup; nil answers reports.* with an internal error.
	reportCtl reportController
}

// SetDiskStatusProvider installs the function that reports free space per
// Storage root, for `vod info`. Called once during daemon startup.
func (s *Service) SetDiskStatusProvider(fn func(context.Context) []diskspace.Root) {
	s.diskRoots = fn
}

func Init(
	nc *nats.Conn,
	cfg vlconfig.AppConfig,
	streamersFile *vlconfig.StreamersFile,
	configCtl configController,
	vodsCtl VODController,
	encodeCtl EncodeController,
	logger *vllog.FileLogger,
	signals ConfigSignals,
	statusReqPriCh chan streamer.StreamerRef,
	kv jetstream.KeyValue,
	intervalsFn intervalsFunc,
	streamerFuncs StreamerFuncs,
	cookies *platformauth.Store,
	added AddedRecorder,
) (*Service, error) {
	vs := &Service{
		appConfig:       cfg,
		streamersFile:   streamersFile,
		configCtl:       configCtl,
		vodsCtl:         vodsCtl,
		encodeCtl:       encodeCtl,
		logger:          logger,
		ShutdownCh:      make(chan struct{}, 1),
		ForceShutdownCh: make(chan struct{}, 1),
		signals:         signals,
		statusReqPriCh:  statusReqPriCh,
		kv:              kv,
		natsURL:         nc.ConnectedUrl(),
		intervalsFn:     intervalsFn,
		streamerFuncs:   streamerFuncs,
		cookies:         cookies,
		added:           added,
	}
	ms, err := micro.AddService(nc, micro.Config{
		Name:    ServiceName,
		Version: "0.0.1",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create API service: %w", err)
	}

	ms.AddEndpoint("shutdown", micro.HandlerFunc(vs.handleShutdown))
	ms.AddEndpoint("shutdown_force", micro.HandlerFunc(vs.handleForceShutdown))
	ms.AddEndpoint("info", micro.HandlerFunc(vs.handleInfo))
	ms.AddEndpoint("update", micro.HandlerFunc(vs.handleUpdate))
	ms.AddEndpoint("platforms", micro.HandlerFunc(vs.handlePlatforms))
	cloudGroup := ms.AddGroup("cloud")
	cloudGroup.AddEndpoint("intervals", micro.HandlerFunc(vs.handleCloudIntervals))
	streamersGroup := ms.AddGroup("streamer")
	streamersGroup.AddEndpoint("list", micro.HandlerFunc(vs.handleListStreamers))
	streamersGroup.AddEndpoint("add", micro.HandlerFunc(vs.handleAddStreamer))
	streamersGroup.AddEndpoint("record_once", micro.HandlerFunc(vs.handleRecordOnce))
	streamersGroup.AddEndpoint("remove", micro.HandlerFunc(vs.handleRemoveStreamer))
	streamersGroup.AddEndpoint("remove_all", micro.HandlerFunc(vs.handleRemoveAll))
	streamersGroup.AddEndpoint("record_off", micro.HandlerFunc(vs.handleRecordOff))
	streamersGroup.AddEndpoint("record_off_all", micro.HandlerFunc(vs.handleRecordOffAll))
	streamersGroup.AddEndpoint("record_on", micro.HandlerFunc(vs.handleRecordOn))
	streamersGroup.AddEndpoint("record_on_all", micro.HandlerFunc(vs.handleRecordOnAll))
	streamersGroup.AddEndpoint("check", micro.HandlerFunc(vs.handleCheckStreamer))
	streamersGroup.AddEndpoint("set_variant", micro.HandlerFunc(vs.handleSetStreamerVariant))
	streamersGroup.AddEndpoint("set_options", micro.HandlerFunc(vs.handleSetStreamerOptions))
	configGroup := ms.AddGroup("config")
	configGroup.AddEndpoint("get", micro.HandlerFunc(vs.handleGetConfig))
	configGroup.AddEndpoint("set", micro.HandlerFunc(vs.handleSetConfig))
	// Local-only: names the container's mounted folders.
	configGroup.AddEndpoint("folders", micro.HandlerFunc(vs.handleConfigFolders))
	// The group name is the subject prefix, so it must track Subjects.vods* and
	// the bridge allowlist.
	vodsGroup := ms.AddGroup("vods")
	vodsGroup.AddEndpoint("list", micro.HandlerFunc(vs.handleListVODs))
	vodsGroup.AddEndpoint("thumbnail", micro.HandlerFunc(vs.handleVODThumbnail))
	vodsGroup.AddEndpoint("previews", micro.HandlerFunc(vs.handleVODPreviews))
	vodsGroup.AddEndpoint("contact_sheet", micro.HandlerFunc(vs.handleVODContactSheet))
	vodsGroup.AddEndpoint("delete", micro.HandlerFunc(vs.handleDeleteVOD))
	vodsGroup.AddEndpoint("delete_many", micro.HandlerFunc(vs.handleDeleteManyVODs))
	vodsGroup.AddEndpoint("cancel_moves", micro.HandlerFunc(vs.handleCancelMoves))
	// Encode queue (VOD merge / re-encode jobs). LOCAL-ONLY: never add encode.*
	// to internal/cloud/bridge.go's bridgeAllowedSubjects, so heavy encode work
	// can't be triggered from the Dashboard.
	encodeGroup := ms.AddGroup("encode")
	encodeGroup.AddEndpoint("enqueue", micro.HandlerFunc(vs.handleEncodeEnqueue))
	encodeGroup.AddEndpoint("list", micro.HandlerFunc(vs.handleEncodeList))
	encodeGroup.AddEndpoint("remove", micro.HandlerFunc(vs.handleEncodeRemove))
	encodeGroup.AddEndpoint("reorder", micro.HandlerFunc(vs.handleEncodeReorder))
	encodeGroup.AddEndpoint("cancel", micro.HandlerFunc(vs.handleEncodeCancel))
	encodeGroup.AddEndpoint("retry", micro.HandlerFunc(vs.handleEncodeRetry))
	encodeGroup.AddEndpoint("pause", micro.HandlerFunc(vs.handleEncodePause))
	encodeGroup.AddEndpoint("resume", micro.HandlerFunc(vs.handleEncodeResume))
	encodeGroup.AddEndpoint("encoders", micro.HandlerFunc(vs.handleEncodeEncoders))
	// Third-party platform logins (Accounts UI). PRIVACY: local-only. NEVER add
	// them to internal/cloud/bridge.go's bridgeAllowedSubjects.
	loginGroup := ms.AddGroup("platformlogin")
	loginGroup.AddEndpoint("set", micro.HandlerFunc(vs.handlePlatformLoginSet))
	loginGroup.AddEndpoint("remove", micro.HandlerFunc(vs.handlePlatformLoginRemove))
	loginGroup.AddEndpoint("list", micro.HandlerFunc(vs.handlePlatformLoginList))
	loginGroup.AddEndpoint("health", micro.HandlerFunc(vs.handlePlatformLoginHealth))
	// Error and bug reports. Local-only, like platformlogin.*.
	reportsGroup := ms.AddGroup("reports")
	reportsGroup.AddEndpoint("get", micro.HandlerFunc(vs.handleGetErrorReports))
	reportsGroup.AddEndpoint("set", micro.HandlerFunc(vs.handleSetErrorReports))
	reportsGroup.AddEndpoint("bug", micro.HandlerFunc(vs.handleSendBugReport))
	return vs, nil
}
