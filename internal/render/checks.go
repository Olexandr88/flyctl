package render

import (
	"fmt"

	"github.com/superfly/fly-go/api"
)

func MachineHealthChecksSummary(machines ...*api.Machine) string {
	var total, pass, crit, warn int

	for _, machine := range machines {
		if n := len(machine.Checks); n > 0 {
			total += n
			pass += passingChecks(machine.Checks)
			crit += critChecks(machine.Checks)
			warn += warnChecks(machine.Checks)
		}
	}

	if total == 0 {
		return ""
	}

	checkStr := fmt.Sprintf("%d total", total)

	if pass > 0 {
		checkStr += ", " + fmt.Sprintf("%d passing", pass)
	}
	if warn > 0 {
		checkStr += ", " + fmt.Sprintf("%d warning", warn)
	}
	if crit > 0 {
		checkStr += ", " + fmt.Sprintf("%d critical", crit)
	}
	return checkStr
}

func passingChecks(checks []*api.MachineCheckStatus) (n int) {
	for _, check := range checks {
		if check.Status == api.Passing {
			n++
		}
	}

	return n
}

func warnChecks(checks []*api.MachineCheckStatus) (n int) {
	for _, check := range checks {
		if check.Status == api.Warning {
			n++
		}
	}

	return n
}

func critChecks(checks []*api.MachineCheckStatus) (n int) {
	for _, check := range checks {
		if check.Status == api.Critical {
			n++
		}
	}

	return n
}
