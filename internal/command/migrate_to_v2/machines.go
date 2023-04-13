package migrate_to_v2

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/samber/lo"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/internal/machine"
)

func (m *v2PlatformMigrator) resolveMachineFromAlloc(alloc *api.AllocationStatus) (*api.LaunchMachineInput, error) {
	mConfig, err := m.appConfig.ToMachineConfig(alloc.TaskName, nil)
	if err != nil {
		return nil, err
	}

	mConfig.Mounts = nil
	mConfig.Guest = m.machineGuest
	mConfig.Image = m.img
	mConfig.Metadata[api.MachineConfigMetadataKeyFlyReleaseId] = m.releaseId
	mConfig.Metadata[api.MachineConfigMetadataKeyFlyReleaseVersion] = strconv.Itoa(m.releaseVersion)
	mConfig.Metadata[api.MachineConfigMetadataKeyFlyPreviousAlloc] = alloc.ID

	if m.isPostgres {
		mConfig.Env["FLY_CONSUL_URL"] = m.pgConsulUrl
		mConfig.Metadata[api.MachineConfigMetadataKeyFlyManagedPostgres] = "true"
	}

	launchInput := &api.LaunchMachineInput{
		AppID:   m.appFull.Name,
		OrgSlug: m.appFull.Organization.ID,
		Region:  alloc.Region,
		Config:  mConfig,
	}

	return launchInput, nil
}

func (m *v2PlatformMigrator) prepMachinesToCreate(ctx context.Context) error {
	var err error
	m.newMachinesInput, err = m.resolveMachinesFromAllocs()
	// FIXME: add extra machines that are stopped by default, based on scaling/autoscaling config for the app
	return err
}

func (m *v2PlatformMigrator) resolveMachinesFromAllocs() ([]*api.LaunchMachineInput, error) {
	var res []*api.LaunchMachineInput
	for _, alloc := range m.oldAllocs {
		mach, err := m.resolveMachineFromAlloc(alloc)
		if err != nil {
			return nil, err
		}
		res = append(res, mach)
	}
	return res, nil
}

func (m *v2PlatformMigrator) createMachines(ctx context.Context) error {
	var newlyCreatedMachines []*api.Machine
	defer func() {
		m.recovery.machinesCreated = newlyCreatedMachines
	}()

	for _, machineInput := range m.newMachinesInput {

		// Assign volume
		if nv, ok := lo.Find(m.createdVolumes, func(v *NewVolume) bool {
			return v.previousAllocId == machineInput.Config.Metadata[api.MachineConfigMetadataKeyFlyPreviousAlloc]
		}); ok {
			machineInput.Config.Mounts = []api.MachineMount{{
				Name:   nv.vol.Name, // TODO: Ask @dov if changing this to use volume name instead of pg_data_machines is OK
				Path:   nv.mountPoint,
				Volume: nv.vol.ID,
			}}
		}
		// Launch machine
		newMachine, err := m.flapsClient.Launch(ctx, *machineInput)
		if err != nil {
			return fmt.Errorf("failed creating a machine in region %s: %w", machineInput.Region, err)
		}
		newlyCreatedMachines = append(newlyCreatedMachines, newMachine)
	}
	for _, mach := range newlyCreatedMachines {
		err := machine.WaitForStartOrStop(ctx, mach, "start", time.Minute*5)
		if err != nil {
			return err
		}
	}
	m.newMachines = machine.NewMachineSet(m.flapsClient, m.io, newlyCreatedMachines)
	return nil
}
