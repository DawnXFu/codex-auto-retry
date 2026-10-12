package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var errStopRequested = errors.New("stop requested")

type daemon struct {
	config      Config
	dataDir     string
	statePath   string
	statusPath  string
	stopPath    string
	controlPath string
	commandDir  string
	logger      *safeLogger
	runner      resumeRunner
	startedAt   time.Time

	mu                                  sync.Mutex
	wg                                  sync.WaitGroup
	state                               RuntimeState
	active                              map[string]RetryJob
	activeCtx                           map[string]context.CancelFunc
	lastScan                            time.Time
	lastError                           string
	controllerState                     string
	lastControllerProbe                 time.Time
	paused                              bool
	memoryUsageBytes                    uint64
	memoryGuardTriggered                bool
	sharedAppServerMemoryBytes          uint64
	sharedAppServerMemoryGuardTriggered bool
	lastSharedAppServerMemoryCheck      time.Time
	writeState                          func(string, any) error
	writeStatusFile                     func(string, any) error
	stateWriteDeferred                  bool
	statusWriteDeferred                 bool
}

func newDaemon(config Config, dataDir string, logger *safeLogger, runner resumeRunner) (*daemon, error) {
	configPath := filepath.Join(dataDir, "config.json")
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if _, err := updateConfigFile(configPath, func(current *Config) error {
			*current = config
			return nil
		}); err != nil {
			return nil, err
		}
	}
	statePath := filepath.Join(dataDir, "state.json")
	state, err := loadState(statePath)
	if err != nil {
		return nil, err
	}
	controlPath := filepath.Join(dataDir, "control.json")
	control, err := loadOrCreateControlState(controlPath)
	if err != nil {
		return nil, err
	}
	daemon := &daemon{
		config:          config,
		dataDir:         dataDir,
		statePath:       statePath,
		statusPath:      filepath.Join(dataDir, "status.json"),
		stopPath:        filepath.Join(dataDir, "stop.signal"),
		controlPath:     controlPath,
		commandDir:      filepath.Join(dataDir, "commands"),
		logger:          logger,
		runner:          runner,
		startedAt:       time.Now().UTC(),
		state:           state,
		active:          make(map[string]RetryJob),
		activeCtx:       make(map[string]context.CancelFunc),
		paused:          control.Paused,
		controllerState: "starting",
		writeState: func(path string, value any) error {
			state, ok := value.(RuntimeState)
			if !ok {
				return fmt.Errorf("runtime state writer received %T", value)
			}
			return writeRuntimeStateAtomic(path, state)
		},
		writeStatusFile: writeJSONAtomic,
	}
	daemon.reconcileStartupState(time.Now().UTC())
	return daemon, nil
}

func (d *daemon) run(ctx context.Context) error {
	_ = os.Remove(d.stopPath)
	if err := d.tick(ctx, time.Now().UTC()); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Duration(d.config.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := d.tick(ctx, now.UTC()); err != nil {
				return err
			}
		}
	}
}

func (d *daemon) tick(ctx context.Context, now time.Time) error {
	if _, err := os.Stat(d.stopPath); err == nil {
		_ = os.Remove(d.stopPath)
		return errStopRequested
	}

	d.processSharedRecovery(ctx, now)
	controllerReady := d.controllerRestartReady(ctx, now)
	d.mu.Lock()
	d.reloadConfigLocked()
	roots := discoverSessionRoots(d.config)
	baseline := !d.state.Initialized
	events, scanErr := scanSessions(roots, &d.state, now, baseline)
	if baseline {
		d.state.Initialized = true
		d.logger.Printf("initialized session baseline roots=%d", len(roots))
	}
	if scanErr != nil {
		d.lastError = scanErr.Error()
		d.logger.Printf("scan failed category=session_scan")
	} else {
		d.lastError = ""
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Event.Timestamp.Before(events[j].Event.Timestamp)
	})
	for _, item := range events {
		// Ignore old history only when another watched store already owns this
		// thread. Failures written while the watchdog was stopped must survive a
		// restart.
		if item.Mirrored && item.Event.Timestamp.Before(d.startedAt.Add(-3*time.Second)) {
			continue
		}
		d.handleEventLocked(item, now)
	}
	// Apply observed starts, cancellations and goal changes before reopening.
	// Queued user controls below still take precedence over dispatch.
	if controllerReady && scanErr == nil {
		d.reopenRestartRequiredLocked(now)
	}
	d.refreshControlsLocked(now)
	d.expireUnacknowledgedLocked(now)
	sharedBackendEnabled := d.config.SharedAppServerEnabled
	// Acknowledged turns normally finish through rollout events. If a previous
	// process stopped after task_started, release the mutex while the optional
	// app-server lifecycle probe verifies that the turn is still active. The
	// probe never starts a task and its result is revalidated under the lock.
	d.mu.Unlock()
	d.checkSharedAppServerMemory(ctx, now)
	if !sharedBackendEnabled {
		d.reconcileSharedBackendCleanup(ctx)
	}
	d.reconcileAwaitingLifecycle(ctx, now)
	d.mu.Lock()
	jobs := d.dispatchDueLocked(now)
	d.lastScan = now
	d.state.prune(now)
	stateErr := d.persistStateLocked()
	statusErr := d.writeStatusLocked(true, len(roots))
	d.mu.Unlock()

	for _, job := range jobs {
		d.wg.Add(1)
		go d.runJob(ctx, job)
	}
	if scanErr != nil {
		return nil
	}
	if stateErr != nil {
		// The in-memory state remains authoritative. Windows indexers, security
		// scanners, and settings readers can briefly hold state.json without
		// delete sharing, so one failed atomic replacement must not terminate the
		// watchdog and remove its tray icon. The next tick retries persistence.
		d.logger.Printf("state save deferred category=state_write")
	}
	if statusErr != nil {
		d.logger.Printf("status save deferred category=status_write")
	}
	return nil
}

func (d *daemon) persistStateLocked() error {
	writer := d.writeState
	if writer == nil {
		writer = writeJSONAtomic
	}
	if err := writer(d.statePath, d.state); err != nil {
		d.stateWriteDeferred = true
		d.lastError = "state_write_deferred"
		return err
	}
	if d.stateWriteDeferred {
		d.stateWriteDeferred = false
		if d.lastError == "state_write_deferred" {
			d.lastError = ""
		}
		d.logger.Printf("state save recovered category=state_write")
	}
	return nil
}

func (d *daemon) recordMemorySample(sample memorySample) {
	d.mu.Lock()
	d.memoryUsageBytes = sample.PrivateBytes
	d.mu.Unlock()
}

func (d *daemon) handleMemoryLimit(sample memorySample) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.memoryUsageBytes = sample.PrivateBytes
	if d.memoryGuardTriggered {
		return
	}
	d.memoryGuardTriggered = true
	d.controllerState = "memory_limit_exceeded"
	d.lastError = "memory_limit_exceeded"
	if d.logger != nil {
		d.logger.Printf("memory guard triggered pid=%d private_memory_mb=%d limit_mb=%d", os.Getpid(), memoryBytesToMB(sample.PrivateBytes), d.config.MemoryLimitMB)
	}
	// Publish the terminal state before the process exits so the manager can
	// distinguish a guarded shutdown from a stale heartbeat.
	_ = d.writeStatusLocked(false, len(discoverSessionRoots(d.config)))
}

func (d *daemon) refreshControlsLocked(now time.Time) {
	control, err := loadOrCreateControlState(d.controlPath)
	if err != nil {
		d.lastError = err.Error()
		d.logger.Printf("control state failed category=control_state")
		return
	}
	d.paused = control.Paused
	commands, invalid, err := loadControlCommandFiles(d.commandDir)
	if err != nil {
		d.lastError = err.Error()
		d.logger.Printf("control commands failed category=control_commands")
		return
	}
	for _, path := range invalid {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			d.lastError = removeErr.Error()
		}
		d.logger.Printf("control command discarded category=invalid_command")
	}
	for _, item := range commands {
		d.applyControlCommandLocked(item.Command, now)
		if removeErr := os.Remove(item.Path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			d.lastError = removeErr.Error()
			d.logger.Printf("control command cleanup failed category=control_commands")
		}
	}
}

func (d *daemon) expireUnacknowledgedLocked(now time.Time) {
	for threadID, thread := range d.state.Threads {
		if thread.Awaiting == nil || thread.Awaiting.RetryTurnID != "" || thread.Awaiting.StartDeadline.After(now) {
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		d.rescheduleAwaitingLocked(threadID, thread, now, "start_not_acknowledged")
	}
}

func (d *daemon) writeStatus(running bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeStatusLocked(running, len(discoverSessionRoots(d.config)))
}

func (d *daemon) writeStatusLocked(running bool, rootCount int) error {
	pending := 0
	active := 0
	waiting := 0
	for _, thread := range d.state.Threads {
		if thread.Pending != nil {
			pending++
		}
		if thread.Awaiting != nil {
			active++
		}
		if thread.QuotaWait != nil {
			waiting++
		}
	}
	if !running {
		pending = 0
		active = 0
	}
	capability := capabilityForControllerState(d.controllerState, d.config.SharedAppServerEnabled)
	if !running {
		capability = capabilityForControllerState("codex_not_running", false)
	}
	status := StatusSnapshot{
		BuildSourceHash:                     buildSourceHash,
		DesktopLaunchMode:                   "process_scoped",
		DesktopTransport:                    capability.Transport,
		RecoveryMode:                        capability.RecoveryMode,
		AutomaticRecoverySupported:          capability.Automatic,
		RecoveryCapabilityReason:            capability.Reason,
		Version:                             appVersion,
		Running:                             running,
		PID:                                 os.Getpid(),
		StartedAt:                           d.startedAt,
		LastScanAt:                          d.lastScan,
		WatchedRoots:                        rootCount,
		PendingRetries:                      pending,
		WaitingForReset:                     waiting,
		Quota:                               quotaSummary(d.state.Quota, waiting),
		ActiveRetries:                       active,
		Paused:                              d.paused,
		SharedAppServerEnabled:              d.config.SharedAppServerEnabled,
		SharedAppServerRequested:            d.config.SharedAppServerRequested,
		ControllerState:                     d.controllerState,
		LastError:                           d.lastError,
		MemoryUsageMB:                       memoryBytesToMB(d.memoryUsageBytes),
		MemoryLimitMB:                       d.config.MemoryLimitMB,
		MemoryGuardTriggered:                d.memoryGuardTriggered,
		SharedAppServerMemoryUsageMB:        memoryBytesToMB(d.sharedAppServerMemoryBytes),
		SharedAppServerMemoryLimitMB:        d.config.SharedAppServerMemoryLimitMB,
		SharedAppServerMemoryGuardTriggered: d.sharedAppServerMemoryGuardTriggered,
		RetrySafetyWarning:                  d.config.retrySafetyWarning(),
		LogPath:                             d.logger.path,
	}
	writer := d.writeStatusFile
	if writer == nil {
		writer = writeJSONAtomic
	}
	if err := writer(d.statusPath, status); err != nil {
		d.statusWriteDeferred = true
		return err
	}
	if d.statusWriteDeferred {
		d.statusWriteDeferred = false
		d.logger.Printf("status save recovered category=status_write")
	}
	return nil
}
