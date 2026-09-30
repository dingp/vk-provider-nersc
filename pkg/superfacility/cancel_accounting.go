package superfacility

import (
	"context"
	"strings"
)

// Cancellation needs evidence for every selected allocation, not the summary
// status used by ordinary Pod polling. Remember allocations across snapshots so
// a missing task cannot turn a mixed array into an apparently terminal one.
type cancellationAccounting struct {
	jobID    string
	observed map[string]struct{}
}

func (c *Client) confirmCancellation(ctx context.Context, machine string, accounting *cancellationAccounting) (bool, error) {
	out, err := c.getComputeJobOutput(ctx, machine, accounting.jobID)
	if err != nil {
		return false, err
	}
	return accounting.confirmed(out.Output), nil
}

func (a *cancellationAccounting) confirmed(rows []map[string]string) bool {
	complete := len(rows) > 0
	numericRequest := slurmJobIDPattern.MatchString(a.jobID) && !strings.Contains(a.jobID, "_")
	allocations := make(map[string]struct{})
	for _, row := range rows {
		id := strings.TrimSpace(firstNonEmpty(row["jobid"], row["JobID"], row["JobId"], row["job_id"]))
		if id == "" {
			// Raw numeric IDs alone cannot distinguish an array parent from an
			// element. Unidentified rows must not establish complete accounting.
			complete = false
			continue
		}
		allocation, _, step := strings.Cut(id, ".")
		raw := strings.TrimSpace(firstNonEmpty(row["jobidraw"], row["JobIDRaw"]))
		rawAllocation, _, _ := strings.Cut(raw, ".")
		selected := allocation == a.jobID || (numericRequest && rawAllocation == a.jobID)
		if numericRequest {
			// Include malformed/compressed array IDs and heterogeneous components
			// in the selection, then fail closed on their unsupported identity.
			selected = selected || strings.HasPrefix(allocation, a.jobID+"_") || strings.HasPrefix(allocation, a.jobID+"+")
		}
		if !slurmJobIDPattern.MatchString(allocation) {
			complete = false
			continue
		}
		if !selected {
			continue
		}
		a.observed[allocation] = struct{}{}
		if step {
			// A completed batch/extern/numbered step cannot prove the allocation
			// ended. Its corresponding allocation row must be present below.
			continue
		}
		allocations[allocation] = struct{}{}
		if !terminalComputeStatus(statusFromJobOutput([]map[string]string{row})) {
			complete = false
		}
	}
	if len(a.observed) == 0 {
		return false
	}
	for id := range a.observed {
		if _, present := allocations[id]; !present {
			return false
		}
	}
	return complete
}
