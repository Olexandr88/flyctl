package ips

import (
	"context"
	"net"
	"strings"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/internal/format"
	"github.com/superfly/flyctl/internal/render"
	"github.com/superfly/flyctl/iostreams"
)

func renderListTable(ctx context.Context, ipAddresses []api.IPAddress) {
	rows := make([][]string, 0, len(ipAddresses))

	var ipType string
	for _, ipAddr := range ipAddresses {
		if strings.HasPrefix(ipAddr.Address, "fdaa") {
			ipType = "private"
		} else {
			ipType = "public"
		}

		switch {
		case ipAddr.Type == "v4":
			rows = append(rows, []string{"v4", ipAddr.Address, "public (dedicated)", ipAddr.Region, ""})
		case ipAddr.Type == "shared_v4":
			rows = append(rows, []string{"v4", ipAddr.Address, "public (shared)", ipAddr.Region, ""})
		case ipAddr.Type == "v6":
			rows = append(rows, []string{"v6", ipAddr.Address, "public", ipAddr.Region, ""})
		case ipAddr.Type == "private_v6":
			rows = append(rows, []string{"v6", ipAddr.Address, "private", ipAddr.Region, ""})
		default:
			rows = append(rows, []string{ipAddr.Type, ipAddr.Address, ipType, ipAddr.Region, format.RelativeTime(ipAddr.CreatedAt)})
		}
	}

	out := iostreams.FromContext(ctx).Out
	render.Table(out, "", rows, "Version", "IP", "Type", "Region", "Created At")
}

func renderPrivateTable(ctx context.Context, allocations []*api.AllocationStatus, backupRegions []api.Region) {
	rows := make([][]string, 0, len(allocations))

	for _, alloc := range allocations {

		region := alloc.Region
		if len(backupRegions) > 0 {
			for _, r := range backupRegions {
				if alloc.Region == r.Code {
					region = alloc.Region + "(B)"
					break
				}
			}
		}

		rows = append(rows, []string{alloc.IDShort, region, alloc.PrivateIP})
	}

	out := iostreams.FromContext(ctx).Out
	render.Table(out, "", rows, "ID", "Region", "IP")
}

func renderPrivateTableMachines(ctx context.Context, machines []*api.Machine) {
	rows := make([][]string, 0, len(machines))

	for _, machine := range machines {
		rows = append(rows, []string{machine.ID, machine.Region, machine.PrivateIP})
	}

	out := iostreams.FromContext(ctx).Out
	render.Table(out, "", rows, "ID", "Region", "IP")
}

func renderSharedTable(ctx context.Context, ip net.IP) {
	rows := make([][]string, 0, 1)

	rows = append(rows, []string{"v4", ip.String(), "shared", "global"})

	out := iostreams.FromContext(ctx).Out
	render.Table(out, "", rows, "Version", "IP", "Type", "Region")
}
