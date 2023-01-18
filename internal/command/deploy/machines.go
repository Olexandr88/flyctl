package deploy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/jpillora/backoff"
	"github.com/morikuni/aec"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/flaps"
	"github.com/superfly/flyctl/gql"
	"github.com/superfly/flyctl/internal/app"
	"github.com/superfly/flyctl/internal/build/imgsrc"
	"github.com/superfly/flyctl/internal/prompt"
	"github.com/superfly/flyctl/internal/render"
	"github.com/superfly/flyctl/iostreams"
	"github.com/superfly/flyctl/terminal"
)

const DefaultWaitTimeout = 120 * time.Second
const DefaultLeaseTtl = 30 * time.Minute

// FIXME: move a lot of this stuff to internal/machine pkg... maybe all of it?
type MachineDeployment interface {
	DeployMachinesApp(context.Context) error
}

type MachineDeploymentArgs struct {
	DeploymentImage      *imgsrc.DeploymentImage
	Strategy             string
	EnvFromFlags         []string
	PrimaryRegionFlag    string
	AutoConfirmMigration bool
	BuildOnly            bool
	SkipHealthChecks     bool
	RestartOnly          bool
	WaitTimeout          time.Duration
	LeaseTimeout         time.Duration
}

type machineDeployment struct {
	gqlClient                  graphql.Client
	flapsClient                *flaps.Client
	io                         *iostreams.IOStreams
	colorize                   *iostreams.ColorScheme
	app                        *api.AppCompact
	appConfig                  *app.Config
	img                        *imgsrc.DeploymentImage
	machineSet                 MachineSet
	releaseCommandMachine      MachineSet
	releaseCommand             string
	volumeName                 string
	volumeDestination          string
	appChecksForMachines       map[string]api.MachineCheck
	appServicesForMachines     []api.MachineService
	strategy                   string
	releaseId                  string
	releaseVersion             int
	autoConfirmAppsV2Migration bool
	skipHealthChecks           bool
	restartOnly                bool
	waitTimeout                time.Duration
	leaseTimeout               time.Duration
}

type MachineSet interface {
	AcquireLeases(context.Context, time.Duration) error
	ReleaseLeases(context.Context) error
	IsEmpty() bool
	GetMachines() []LeasableMachine
}

type machineSet struct {
	machines []LeasableMachine
}

type LeasableMachine interface {
	Machine() *api.Machine
	HasLease() bool
	AcquireLease(context.Context, time.Duration) error
	ReleaseLease(context.Context) error
	Update(context.Context, api.LaunchMachineInput) error
	Start(context.Context) error
	WaitForState(context.Context, string, time.Duration) error
	WaitForHealthchecksToPass(context.Context, time.Duration) error
	WaitForEventTypeAfterType(context.Context, string, string, time.Duration) (*api.MachineEvent, error)
}

type leasableMachine struct {
	flapsClient     *flaps.Client
	io              *iostreams.IOStreams
	colorize        *iostreams.ColorScheme
	machine         *api.Machine
	leaseNonce      string
	leaseExpiration time.Time
}

func NewLeasableMachine(flapsClient *flaps.Client, io *iostreams.IOStreams, machine *api.Machine) LeasableMachine {
	return &leasableMachine{
		flapsClient: flapsClient,
		io:          io,
		colorize:    io.ColorScheme(),
		machine:     machine,
	}
}

func (lm *leasableMachine) Update(ctx context.Context, input api.LaunchMachineInput) error {
	if !lm.HasLease() {
		return fmt.Errorf("no current lease for machine %s", lm.machine.ID)
	}
	updateMachine, err := lm.flapsClient.Update(ctx, input, lm.leaseNonce)
	if err != nil {
		return err
	}
	lm.machine = updateMachine
	return nil
}

func (md *machineDeployment) logClearLinesAbove(count int) {
	if md.io.IsInteractive() {
		builder := aec.EmptyBuilder
		str := builder.Up(uint(count)).EraseLine(aec.EraseModes.All).ANSI
		fmt.Fprint(md.io.ErrOut, str.String())
	}
}

func (lm *leasableMachine) logClearLinesAbove(count int) {
	if lm.io.IsInteractive() {
		builder := aec.EmptyBuilder
		str := builder.Up(uint(count)).EraseLine(aec.EraseModes.All).ANSI
		fmt.Fprint(lm.io.ErrOut, str.String())
	}
}

func (lm *leasableMachine) logStatusWaiting(desired string) {
	fmt.Fprintf(lm.io.ErrOut, "  Waiting for %s to have state: %s\n",
		lm.colorize.Bold(lm.Machine().ID),
		lm.colorize.Yellow(desired),
	)
}

func (lm *leasableMachine) logStatusFinished(current string) {
	fmt.Fprintf(lm.io.ErrOut, "  Machine %s has state: %s\n",
		lm.colorize.Bold(lm.Machine().ID),
		lm.colorize.Green(current),
	)
}

func (lm *leasableMachine) logHealthCheckStatus(status *api.HealthCheckStatus) {
	if status == nil {
		return
	}
	resColor := lm.colorize.Green
	if status.Passing != status.Total {
		resColor = lm.colorize.Yellow
	}
	fmt.Fprintf(lm.io.ErrOut, "  Waiting for %s to become healthy: %s\n",
		lm.colorize.Bold(lm.Machine().ID),
		resColor(fmt.Sprintf("%d/%d", status.Passing, status.Total)),
	)
}

func (lm *leasableMachine) Start(ctx context.Context) error {
	if lm.HasLease() {
		return fmt.Errorf("error cannot start machine %s because it has a lease expiring at %s", lm.machine.ID, lm.leaseExpiration.Format(time.RFC3339))
	}
	lm.logStatusWaiting(api.MachineStateStarted)
	_, err := lm.flapsClient.Start(ctx, lm.machine.ID)
	if err != nil {
		return err
	}
	return nil
}

func (lm *leasableMachine) WaitForState(ctx context.Context, desiredState string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b := &backoff.Backoff{
		Min:    500 * time.Millisecond,
		Max:    2 * time.Second,
		Factor: 2,
		Jitter: true,
	}
	lm.logClearLinesAbove(1)
	lm.logStatusWaiting(desiredState)
	for {
		err := lm.flapsClient.Wait(waitCtx, lm.Machine(), desiredState, timeout)
		switch {
		case errors.Is(err, context.Canceled):
			return err
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("timeout reached waiting for machine to %s %w", desiredState, err)
		case err != nil:
			time.Sleep(b.Duration())
			continue
		}
		lm.logClearLinesAbove(1)
		lm.logStatusFinished(desiredState)
		return nil
	}
}

func (lm *leasableMachine) WaitForHealthchecksToPass(ctx context.Context, timeout time.Duration) error {
	if lm.machine.Config.Checks == nil {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shortestInterval := 120 * time.Second
	for _, c := range lm.Machine().Config.Checks {
		ci := c.Interval.Duration
		if ci < shortestInterval {
			shortestInterval = ci
		}
	}
	b := &backoff.Backoff{
		Min:    shortestInterval / 2,
		Max:    2 * shortestInterval,
		Factor: 2,
		Jitter: true,
	}
	printedFirst := false
	for {
		updateMachine, err := lm.flapsClient.Get(waitCtx, lm.Machine().ID)
		switch {
		case errors.Is(err, context.Canceled):
			return err
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("timeout reached waiting for healthchecks to pass for machine %s %w", lm.Machine().ID, err)
		case err != nil:
			return fmt.Errorf("error getting machine %s from api: %w", lm.Machine().ID, err)
		case !updateMachine.HealthCheckStatus().AllPassing():
			if !printedFirst || lm.io.IsInteractive() {
				lm.logClearLinesAbove(1)
				lm.logHealthCheckStatus(updateMachine.HealthCheckStatus())
				printedFirst = true
			}
			time.Sleep(b.Duration())
			continue
		}
		lm.logClearLinesAbove(1)
		lm.logHealthCheckStatus(updateMachine.HealthCheckStatus())
		return nil
	}
}

// waits for an eventType1 type event to show up after we see a eventType2 event, and returns it
func (lm *leasableMachine) WaitForEventTypeAfterType(ctx context.Context, eventType1, eventType2 string, timeout time.Duration) (*api.MachineEvent, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b := &backoff.Backoff{
		Min:    500 * time.Millisecond,
		Max:    2 * time.Second,
		Factor: 2,
		Jitter: true,
	}
	lm.logClearLinesAbove(1)
	fmt.Fprintf(lm.io.ErrOut, "  Waiting for %s to get %s event\n",
		lm.colorize.Bold(lm.Machine().ID),
		lm.colorize.Yellow(eventType1),
	)
	for {
		updateMachine, err := lm.flapsClient.Get(waitCtx, lm.Machine().ID)
		switch {
		case errors.Is(err, context.Canceled):
			return nil, err
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("timeout reached waiting for healthchecks to pass for machine %s %w", lm.Machine().ID, err)
		case err != nil:
			return nil, fmt.Errorf("error getting machine %s from api: %w", lm.Machine().ID, err)
		}
		exitEvent := updateMachine.GetLatestEventOfTypeAfterType(eventType1, eventType2)
		if exitEvent != nil {
			return exitEvent, nil
		} else {
			time.Sleep(b.Duration())
		}
	}
}

func (lm *leasableMachine) Machine() *api.Machine {
	return lm.machine
}

func (lm *leasableMachine) HasLease() bool {
	return lm.leaseNonce != "" && lm.leaseExpiration.After(time.Now())
}

func (lm *leasableMachine) AcquireLease(ctx context.Context, duration time.Duration) error {
	if lm.HasLease() {
		return nil
	}
	seconds := int(duration.Seconds())
	lease, err := lm.flapsClient.AcquireLease(ctx, lm.machine.ID, &seconds)
	if err != nil {
		return err
	}
	if lease.Status != "success" {
		return fmt.Errorf("did not acquire lease for machine %s status: %s code: %s message: %s", lm.machine.ID, lease.Status, lease.Code, lease.Message)
	}
	if lease.Data == nil {
		return fmt.Errorf("missing data from lease response for machine %s, assuming not successful", lm.machine.ID)
	}
	lm.leaseNonce = lease.Data.Nonce
	lm.leaseExpiration = time.Unix(lease.Data.ExpiresAt, 0)
	return nil
}

func (lm *leasableMachine) ReleaseLease(ctx context.Context) error {
	if !lm.HasLease() {
		lm.resetLease()
		return nil
	}
	// don't bother releasing expired leases, and allow for some clock skew between flyctl and flaps
	if time.Since(lm.leaseExpiration) > 5*time.Second {
		lm.resetLease()
		return nil
	}
	err := lm.flapsClient.ReleaseLease(ctx, lm.machine.ID, lm.leaseNonce)
	if err != nil {
		terminal.Warnf("failed to release lease for machine %s (expires at %s): %v\n", lm.machine.ID, lm.leaseExpiration.Format(time.RFC3339), err)
		lm.resetLease()
		return err
	}
	lm.resetLease()
	return nil
}

func (lm *leasableMachine) resetLease() {
	lm.leaseNonce = ""
	lm.leaseExpiration = time.Time{}
}

func NewMachineSet(flapsClient *flaps.Client, io *iostreams.IOStreams, machines []*api.Machine) MachineSet {
	leaseMachines := make([]LeasableMachine, 0)
	for _, m := range machines {
		leaseMachines = append(leaseMachines, NewLeasableMachine(flapsClient, io, m))
	}
	return &machineSet{
		machines: leaseMachines,
	}
}

func (ms *machineSet) IsEmpty() bool {
	return len(ms.machines) == 0
}

func (ms *machineSet) GetMachines() []LeasableMachine {
	return ms.machines
}

func (ms *machineSet) AcquireLeases(ctx context.Context, duration time.Duration) error {
	results := make(chan error, len(ms.machines))
	var wg sync.WaitGroup
	for _, m := range ms.machines {
		wg.Add(1)
		go func(m LeasableMachine) {
			defer wg.Done()
			results <- m.AcquireLease(ctx, duration)
		}(m)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	hadError := false
	for err := range results {
		if err != nil {
			hadError = true
			terminal.Warnf("failed to acquire lease: %v\n", err)
		}
	}
	if hadError {
		if err := ms.ReleaseLeases(ctx); err != nil {
			terminal.Warnf("error releasing machine leases: %v\n", err)
		}
		return fmt.Errorf("error acquiring leases on all machines")
	}
	return nil
}

func (ms *machineSet) ReleaseLeases(ctx context.Context) error {
	// when context is canceled, take 500ms to attempt to release the leases
	contextWasAlreadyCanceled := errors.Is(ctx.Err(), context.Canceled)
	if contextWasAlreadyCanceled {
		var cancel context.CancelFunc
		cancelTimeout := 500 * time.Millisecond
		ctx, cancel = context.WithTimeout(context.TODO(), cancelTimeout)
		terminal.Infof("detected canceled context and allowing %s to release machine leases\n", cancelTimeout)
		defer cancel()
	}

	results := make(chan error, len(ms.machines))
	var wg sync.WaitGroup
	for _, m := range ms.machines {
		wg.Add(1)
		go func(m LeasableMachine) {
			defer wg.Done()
			results <- m.ReleaseLease(ctx)
		}(m)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	hadError := false
	for err := range results {
		contextTimedOutOrCanceled := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
		if err != nil && (!contextWasAlreadyCanceled || !contextTimedOutOrCanceled) {
			hadError = true
			terminal.Warnf("failed to release lease: %v\n", err)
		}
	}
	if hadError {
		return fmt.Errorf("error releasing leases on machines")
	}
	return nil
}

func NewMachineDeployment(ctx context.Context, args MachineDeploymentArgs) (MachineDeployment, error) {
	if !args.RestartOnly && args.DeploymentImage == nil {
		return nil, fmt.Errorf("BUG: machines deployment created without specifying the image")
	}
	if args.RestartOnly && args.DeploymentImage != nil {
		return nil, fmt.Errorf("BUG: restartOnly machines deployment created and specified an image")
	}
	appConfig, err := determineAppConfig(ctx, args.EnvFromFlags, args.PrimaryRegionFlag)
	if err != nil {
		return nil, err
	}
	if appConfig.Env == nil {
		appConfig.Env = map[string]string{}
	}
	err = appConfig.Validate()
	if err != nil {
		return nil, err
	}
	app, err := client.FromContext(ctx).API().GetAppCompact(ctx, appConfig.AppName)
	if err != nil {
		return nil, err
	}
	flapsClient, err := flaps.New(ctx, app)
	if err != nil {
		return nil, err
	}
	releaseCmd := ""
	if appConfig.Deploy != nil {
		releaseCmd = appConfig.Deploy.ReleaseCommand
	}
	waitTimeout := args.WaitTimeout
	if waitTimeout == 0 {
		waitTimeout = DefaultWaitTimeout
	}
	leaseTimeout := args.LeaseTimeout
	if leaseTimeout == 0 {
		leaseTimeout = DefaultLeaseTtl
	}
	if waitTimeout != DefaultWaitTimeout || leaseTimeout != DefaultLeaseTtl || args.WaitTimeout == 0 || args.LeaseTimeout == 0 {
		terminal.Infof("Using wait timeout: %s and lease timeout: %s\n", waitTimeout, leaseTimeout)
	}
	io := iostreams.FromContext(ctx)
	md := &machineDeployment{
		gqlClient:                  client.FromContext(ctx).API().GenqClient,
		flapsClient:                flapsClient,
		io:                         io,
		colorize:                   io.ColorScheme(),
		app:                        app,
		appConfig:                  appConfig,
		img:                        args.DeploymentImage,
		autoConfirmAppsV2Migration: args.AutoConfirmMigration,
		skipHealthChecks:           args.SkipHealthChecks,
		restartOnly:                args.RestartOnly,
		waitTimeout:                waitTimeout,
		leaseTimeout:               leaseTimeout,
		releaseCommand:             releaseCmd,
	}
	md.setStrategy(args.Strategy)
	err = md.translateServicesAndChecksForMachines()
	if err != nil {
		return nil, err
	}
	err = md.setVolumeConfig()
	if err != nil {
		return nil, err
	}
	err = md.setMachinesForDeployment(ctx)
	if err != nil {
		return nil, err
	}
	err = md.validateVolumeConfig()
	if err != nil {
		return nil, err
	}
	err = md.createReleaseInBackend(ctx)
	if err != nil {
		return nil, err
	}
	return md, nil
}

func (md *machineDeployment) runReleaseCommand(ctx context.Context) error {
	if md.releaseCommand == "" || md.releaseCommandMachine.IsEmpty() || md.restartOnly {
		return nil
	}
	io := iostreams.FromContext(ctx)
	fmt.Fprintf(io.ErrOut, "Running %s release_command: %s\n",
		md.colorize.Bold(md.app.Name),
		md.appConfig.Deploy.ReleaseCommand,
	)
	err := md.createOrUpdateReleaseCmdMachine(ctx)
	if err != nil {
		return fmt.Errorf("error running release_command machine: %w", err)
	}
	releaseCmdMachine := md.releaseCommandMachine.GetMachines()[0]
	// FIXME: consolidate this wait stuff with deploy waits? Especially once we improve the outpu
	err = releaseCmdMachine.WaitForState(ctx, api.MachineStateStarted, md.waitTimeout)
	if err != nil {
		return fmt.Errorf("error waiting for release_command machine %s to start: %w", releaseCmdMachine.Machine().ID, err)
	}
	err = releaseCmdMachine.WaitForState(ctx, api.MachineStateStopped, md.waitTimeout)
	if err != nil {
		return fmt.Errorf("error waiting for release_command machine %s to finish running: %w", releaseCmdMachine.Machine().ID, err)
	}
	lastExitEvent, err := releaseCmdMachine.WaitForEventTypeAfterType(ctx, "exit", "start", md.waitTimeout)
	if err != nil {
		return fmt.Errorf("error finding the release_command machine %s exit event: %w", releaseCmdMachine.Machine().ID, err)
	}
	exitCode, err := lastExitEvent.Request.GetExitCode()
	if err != nil {
		return fmt.Errorf("error get release_command machine %s exit code: %w", releaseCmdMachine.Machine().ID, err)
	}
	if exitCode != 0 {
		fmt.Fprintf(md.io.ErrOut, "Error release_command failed running on machine %s with exit code %s. Check the logs at: https://fly.io/apps/%s/monitoring\n",
			md.colorize.Bold(releaseCmdMachine.Machine().ID), md.colorize.Red(strconv.Itoa(exitCode)), md.app.Name)
		return fmt.Errorf("error release_command machine %s exited with non-zero status of %d", releaseCmdMachine.Machine().ID, exitCode)
	}
	md.logClearLinesAbove(1)
	fmt.Fprintf(md.io.ErrOut, "  release_command %s completed successfully\n", md.colorize.Bold(releaseCmdMachine.Machine().ID))
	return nil
}

func (md *machineDeployment) DeployMachinesApp(ctx context.Context) error {
	err := md.runReleaseCommand(ctx)
	if err != nil {
		return fmt.Errorf("release command failed - aborting deployment. %w", err)
	}

	if md.machineSet.IsEmpty() {
		return md.createOneMachine(ctx)
	}

	err = md.machineSet.AcquireLeases(ctx, md.leaseTimeout)
	defer func() {
		err := md.machineSet.ReleaseLeases(ctx)
		if err != nil {
			terminal.Warnf("error releasing leases on machines: %v\n", err)
		}
	}()
	if err != nil {
		return err
	}

	// FIXME: handle deploy strategy: rolling, immediate, canary, bluegreen

	fmt.Fprintf(md.io.Out, "Deploying %s app with %s strategy\n", md.colorize.Bold(md.app.Name), md.strategy)
	for _, m := range md.machineSet.GetMachines() {
		launchInput := md.resolveUpdatedMachineConfig(api.MachineProcessGroupApp, m.Machine())
		fmt.Fprintf(md.io.ErrOut, "  Updating %s\n", md.colorize.Bold(m.Machine().ID))
		err := m.Update(ctx, *launchInput)
		if err != nil {
			if md.strategy != "immediate" {
				return err
			} else {
				fmt.Printf("Continuing after error: %s\n", err)
			}
		}

		if md.strategy != "immediate" {
			err := m.WaitForState(ctx, api.MachineStateStarted, md.waitTimeout)
			if err != nil {
				return err
			}
		}

		if md.strategy != "immediate" && !md.skipHealthChecks {
			err := m.WaitForHealthchecksToPass(ctx, md.waitTimeout)
			// FIXME: combine this wait with the wait for start as one update line (or two per in noninteractive case)
			if err != nil {
				return err
			}
		}
	}

	fmt.Fprintf(md.io.ErrOut, "  Finished deploying\n")
	return nil
}

func (md *machineDeployment) createOneMachine(ctx context.Context) error {
	fmt.Fprintf(md.io.Out, "No machines in %s app, launching one new machine\n", md.colorize.Bold(md.app.Name))
	launchInput := md.resolveUpdatedMachineConfig(api.MachineProcessGroupReleaseCommand, nil)
	newMachineRaw, err := md.flapsClient.Launch(ctx, *launchInput)
	newMachine := NewLeasableMachine(md.flapsClient, md.io, newMachineRaw)
	if err != nil {
		return fmt.Errorf("error creating a new machine machine: %w", err)
	}
	// FIXME: dry this up with release commands and non-empty update
	fmt.Fprintf(md.io.ErrOut, "No machines in %s app, launching one new machine\n", md.colorize.Bold(md.app.Name))
	if md.strategy != "immediate" {
		err := newMachine.WaitForState(ctx, api.MachineStateStarted, md.waitTimeout)
		if err != nil {
			return err
		}
	}
	if md.strategy != "immediate" && !md.skipHealthChecks {
		err := newMachine.WaitForHealthchecksToPass(ctx, md.waitTimeout)
		// FIXME: combine this wait with the wait for start as one update line (or two per in noninteractive case)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(md.io.ErrOut, "  Finished deploying\n")
	return nil
}

func (md *machineDeployment) setMachinesForDeployment(ctx context.Context) error {
	machines, releaseCmdMachine, err := md.flapsClient.ListFlyAppsMachines(ctx)
	if err != nil {
		return err
	}

	// migrate non-platform machines into fly platform
	if len(machines) == 0 {
		terminal.Debug("Found no machines that are part of Fly Apps Platform. Check for other machines...")
		machines, err = md.flapsClient.ListActive(ctx)
		if err != nil {
			return err
		}
		if len(machines) > 0 {
			rows := make([][]string, 0)
			for _, machine := range machines {
				var volName string
				if machine.Config != nil && len(machine.Config.Mounts) > 0 {
					volName = machine.Config.Mounts[0].Volume
				}

				rows = append(rows, []string{
					machine.ID,
					machine.Name,
					machine.State,
					machine.Region,
					machine.ImageRefWithVersion(),
					machine.PrivateIP,
					volName,
					machine.CreatedAt,
					machine.UpdatedAt,
				})
			}
			terminal.Warnf("Found %d machines that are not part of the Fly Apps Platform:\n", len(machines))
			_ = render.Table(iostreams.FromContext(ctx).Out, fmt.Sprintf("%s machines", md.app.Name), rows, "ID", "Name", "State", "Region", "Image", "IP Address", "Volume", "Created", "Last Updated")
			if !md.autoConfirmAppsV2Migration {
				switch confirmed, err := prompt.Confirmf(ctx, "Migrate %d existing machines into Fly Apps Platform?", len(machines)); {
				case err == nil:
					if !confirmed {
						terminal.Info("Skipping machines migration to Fly Apps Platform and the deployment")
						md.machineSet = NewMachineSet(md.flapsClient, md.io, nil)
						return nil
					}
				case prompt.IsNonInteractive(err):
					return prompt.NonInteractiveError("not running interactively, use --auto-confirm flag to confirm")
				default:
					return err
				}
			}
			terminal.Infof("Migrating %d machines to the Fly Apps Platform\n", len(machines))
		}
	}

	md.machineSet = NewMachineSet(md.flapsClient, md.io, machines)
	var releaseCmdSet []*api.Machine
	if releaseCmdMachine != nil {
		releaseCmdSet = []*api.Machine{releaseCmdMachine}
	}
	md.releaseCommandMachine = NewMachineSet(md.flapsClient, md.io, releaseCmdSet)
	return nil
}

func (md *machineDeployment) createOrUpdateReleaseCmdMachine(ctx context.Context) error {
	if md.releaseCommandMachine.IsEmpty() {
		err := md.createReleaseCommandMachine(ctx)
		if err != nil {
			return err
		}
	} else {
		err := md.updateReleaseCommandMachine(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (md *machineDeployment) configureLaunchInputForReleaseCommand(launchInput *api.LaunchMachineInput) *api.LaunchMachineInput {
	launchInput.Config.Init.Cmd = strings.Split(md.releaseCommand, " ")
	launchInput.Config.Services = nil
	launchInput.Config.Checks = nil
	launchInput.Config.Restart = api.MachineRestart{
		Policy: api.MachineRestartPolicyNo,
	}
	if md.appConfig.PrimaryRegion != "" {
		launchInput.Region = md.appConfig.PrimaryRegion
	}
	if _, present := launchInput.Config.Env["RELEASE_COMMAND"]; !present {
		launchInput.Config.Env["RELEASE_COMMAND"] = "1"
	}
	return launchInput
}

func (md *machineDeployment) createReleaseCommandMachine(ctx context.Context) error {
	if md.releaseCommand == "" || !md.releaseCommandMachine.IsEmpty() {
		return nil
	}
	launchInput := md.resolveUpdatedMachineConfig(api.MachineProcessGroupReleaseCommand, nil)

	launchInput = md.configureLaunchInputForReleaseCommand(launchInput)
	releaseCmdMachine, err := md.flapsClient.Launch(ctx, *launchInput)
	if err != nil {
		return fmt.Errorf("error creating a release_command machine: %w", err)
	}
	md.releaseCommandMachine = NewMachineSet(md.flapsClient, md.io, []*api.Machine{releaseCmdMachine})
	return nil
}

func (md *machineDeployment) updateReleaseCommandMachine(ctx context.Context) error {
	if md.releaseCommand == "" {
		return nil
	}
	if md.releaseCommandMachine.IsEmpty() {
		return fmt.Errorf("expected release_command machine to exist already, but it does not :-(")
	}
	releaseCmdMachine := md.releaseCommandMachine.GetMachines()[0]
	fmt.Fprintf(md.io.ErrOut, "  Updating release_command machine %s\n", md.colorize.Bold(releaseCmdMachine.Machine().ID))
	err := releaseCmdMachine.WaitForState(ctx, api.MachineStateStopped, md.waitTimeout)
	if err != nil {
		return err
	}
	updatedConfig := md.resolveUpdatedMachineConfig(api.MachineProcessGroupReleaseCommand, releaseCmdMachine.Machine())
	updatedConfig = md.configureLaunchInputForReleaseCommand(updatedConfig)
	err = md.releaseCommandMachine.AcquireLeases(ctx, md.leaseTimeout)
	defer func() {
		_ = md.releaseCommandMachine.ReleaseLeases(ctx)
	}()
	if err != nil {
		return err
	}
	err = releaseCmdMachine.Update(ctx, *updatedConfig)
	if err != nil {
		return fmt.Errorf("error updating release_command machine: %w", err)
	}
	return nil
}

func (md *machineDeployment) setVolumeConfig() error {
	if md.appConfig.Mounts != nil {
		md.volumeName = md.appConfig.Mounts.Source
		md.volumeDestination = md.appConfig.Mounts.Destination
	}
	return nil
}

func (md *machineDeployment) validateVolumeConfig() error {
	if md.machineSet.IsEmpty() {
		return nil
	}
	for _, m := range md.machineSet.GetMachines() {
		mid := m.Machine().ID
		mountsConfig := m.Machine().Config.Mounts
		if len(mountsConfig) > 1 {
			return fmt.Errorf("error machine %s has %d mounts and expected 1", mid, len(mountsConfig))
		}
		if md.volumeName == "" {
			if len(mountsConfig) != 0 {
				return fmt.Errorf("error machine %s has a volume mounted and app config does not specify a volume; remove the volume from the machine or add a [mounts] configuration to fly.toml", mid)
			}
		} else {
			if len(mountsConfig) == 0 {
				return fmt.Errorf("error machine %s does not have a volume configured and fly.toml expects one with name %s; remove the [mounts] configuration in fly.toml or use the machines API to add a volume to this machine", mid, md.volumeName)
			}
			mVolName := mountsConfig[0].Name
			if md.volumeName != mVolName {
				return fmt.Errorf("error machine %s has volume with name %s and fly.toml has [mounts] source set to %s; update the source to %s or use the machines API to attach a volume with name %s to this machine", mid, mVolName, md.volumeName, mVolName, md.volumeName)
			}
		}
	}
	return nil
}

func (md *machineDeployment) setStrategy(passedInStrategy string) {
	if passedInStrategy != "" {
		md.strategy = passedInStrategy
		return
	}
	stratFromConfig := md.appConfig.GetDeployStrategy()
	if stratFromConfig != "" {
		md.strategy = stratFromConfig
		return
	}
	// FIXME: any other checks we want to do here? e.g., we used to do canary if any max_per_region==0 && app.distinct_regions?==false
	md.strategy = "rolling"
}

func (md *machineDeployment) createReleaseInBackend(ctx context.Context) error {
	_ = `# @genqlient
	mutation MachinesCreateRelease($input:CreateReleaseInput!) {
		createRelease(input:$input) {
			release {
				id
				version
			}
		}
	}
	`
	input := gql.CreateReleaseInput{
		AppId:           md.appConfig.AppName,
		PlatformVersion: "machines",
		Strategy:        gql.DeploymentStrategy(strings.ToUpper(md.strategy)),
		Definition:      md.appConfig.Definition,
	}
	if !md.restartOnly {
		input.Image = md.img.Tag
	} else if !md.machineSet.IsEmpty() {
		input.Image = md.machineSet.GetMachines()[0].Machine().Config.Image
	}
	resp, err := gql.MachinesCreateRelease(ctx, md.gqlClient, input)
	if err != nil {
		return err
	}
	md.releaseId = resp.CreateRelease.Release.Id
	md.releaseVersion = resp.CreateRelease.Release.Version
	return nil
}

func (md *machineDeployment) resolveUpdatedMachineConfig(processGroup string, origMachineRaw *api.Machine) *api.LaunchMachineInput {
	machineConf := &api.MachineConfig{}
	if md.restartOnly {
		machineConf = origMachineRaw.Config
	}
	launchInput := &api.LaunchMachineInput{
		ID:      origMachineRaw.ID,
		AppID:   md.app.Name,
		OrgSlug: md.app.Organization.ID,
		Config:  machineConf,
		Region:  origMachineRaw.Region,
	}
	launchInput.Config.Metadata = md.defaultMachineMetadata(processGroup)
	for k, v := range origMachineRaw.Config.Metadata {
		if !isFlyAppsPlatformMetadata(k) {
			launchInput.Config.Metadata[k] = v
		}
	}
	if md.restartOnly {
		return launchInput
	}

	launchInput.Config.Image = md.img.Tag
	launchInput.Config.Init.Cmd = nil
	launchInput.Config.Checks = md.appChecksForMachines
	launchInput.Config.Services = md.appServicesForMachines
	launchInput.Config.Metrics = md.appConfig.Metrics

	// FIXME: can we set launchInput.Config.Restart from fly.toml?

	launchInput.Config.Env = md.appConfig.Env
	if launchInput.Config.Env == nil {
		launchInput.Config.Env = map[string]string{}
	}
	if launchInput.Config.Env["PRIMARY_REGION"] == "" && origMachineRaw.Config.Env["PRIMARY_REGION"] != "" {
		launchInput.Config.Env["PRIMARY_REGION"] = origMachineRaw.Config.Env["PRIMARY_REGION"]
	}

	if origMachineRaw.Config.Mounts != nil {
		launchInput.Config.Mounts = origMachineRaw.Config.Mounts
	}
	if len(launchInput.Config.Mounts) == 1 && launchInput.Config.Mounts[0].Path != md.volumeDestination {
		currentMount := launchInput.Config.Mounts[0]
		terminal.Warnf("Updating the mount path for volume %s on machine %s from %s to %s due to fly.toml [mounts] destination value\n", currentMount.Volume, origMachineRaw.ID, currentMount.Path, md.volumeDestination)
		launchInput.Config.Mounts[0].Path = md.volumeDestination
	}

	// FIXME: this should be set from the appConfig, right? in particular this ensures all the machines have the same cpu, mem, etc
	if origMachineRaw.Config.Guest != nil {
		launchInput.Config.Guest = origMachineRaw.Config.Guest
	}

	return launchInput
}

func (md *machineDeployment) translateServicesAndChecksForMachines() error {
	md.appServicesForMachines = make([]api.MachineService, 0)
	if md.appConfig.HttpService != nil {
		md.appServicesForMachines = append(md.appServicesForMachines, *md.appConfig.HttpService.ToMachineService())
	}
	checkCount := 0
	md.appChecksForMachines = make(map[string]api.MachineCheck)
	for checkName, check := range md.appConfig.Checks {
		fullCheckName := fmt.Sprintf("chk-%s-%s", checkName, check.String())
		machineCheck, err := check.ToMachineCheck()
		if err != nil {
			return err
		}
		md.appChecksForMachines[fullCheckName] = *machineCheck
	}
	checkCount += len(md.appConfig.Checks)
	for _, service := range md.appConfig.Services {
		md.appServicesForMachines = append(md.appServicesForMachines, *service.ToMachineService())
		for i, httpCheck := range service.HttpChecks {
			checkName := fmt.Sprintf("svcchk%d-%s", checkCount+i, httpCheck.String(service.InternalPort))
			machineCheck, err := httpCheck.ToMachineCheck(service.InternalPort)
			if err != nil {
				return err
			}
			md.appChecksForMachines[checkName] = *machineCheck
		}
		checkCount += len(service.HttpChecks)
		for i, tcpCheck := range service.TcpChecks {
			checkName := fmt.Sprintf("svcchk%d-%s", checkCount+i, tcpCheck.String(service.InternalPort))
			machineCheck, err := tcpCheck.ToMachineCheck(service.InternalPort)
			if err != nil {
				return err
			}
			md.appChecksForMachines[checkName] = *machineCheck
		}
		checkCount += len(service.TcpChecks)
	}
	return nil
}

func (md *machineDeployment) defaultMachineMetadata(processGroup string) map[string]string {
	res := map[string]string{
		api.MachineConfigMetadataKeyFlyPlatformVersion: api.MachineFlyPlatformVersion2,
		api.MachineConfigMetadataKeyFlyReleaseId:       md.releaseId,
		api.MachineConfigMetadataKeyFlyReleaseVersion:  strconv.Itoa(md.releaseVersion),
		api.MachineConfigMetadataKeyProcessGroup:       processGroup,
	}
	if md.app.IsPostgresApp() {
		res[api.MachineConfigMetadataKeyFlyManagedPostgres] = "true"
	}
	return res
}

func isFlyAppsPlatformMetadata(key string) bool {
	return key == api.MachineConfigMetadataKeyFlyPlatformVersion ||
		key == api.MachineConfigMetadataKeyFlyReleaseId ||
		key == api.MachineConfigMetadataKeyFlyReleaseVersion ||
		key == api.MachineConfigMetadataKeyProcessGroup ||
		key == api.MachineConfigMetadataKeyFlyManagedPostgres
}
