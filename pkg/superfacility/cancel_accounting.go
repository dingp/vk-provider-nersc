package superfacility

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Cancellation needs evidence for every selected allocation, not the summary
// status used by ordinary Pod polling. Remember allocations across snapshots so
// a missing task cannot turn a mixed array into an apparently terminal one.
type cancellationAccounting struct {
	jobID       string
	observed    map[string]struct{}
	arrayParent bool
}

func (c *Client) confirmCancellation(ctx context.Context, machine string, accounting *cancellationAccounting) (bool, error) {
	if !accounting.arrayParent {
		out, err := c.getComputeJobOutput(ctx, machine, accounting.jobID)
		if err != nil {
			return false, err
		}
		// The singular SFAPI endpoint can return only one array element even
		// for a parent ID. Once identified, use direct allocation accounting
		// throughout this cancellation attempt, never that incomplete view.
		accounting.arrayParent = accounting.hasArrayElements(out.Output)
		confirmed := accounting.confirmed(out.Output)
		if !accounting.arrayParent {
			return confirmed, nil
		}
	}
	rows, err := c.arrayCancellationRows(ctx, machine, accounting.jobID)
	if err != nil {
		return false, err
	}
	return accounting.confirmed(rows), nil
}

func (a *cancellationAccounting) hasArrayElements(rows []map[string]string) bool {
	if !slurmJobIDPattern.MatchString(a.jobID) || strings.Contains(a.jobID, "_") {
		return false
	}
	for _, row := range rows {
		id := strings.TrimSpace(firstNonEmpty(row["jobid"], row["JobID"], row["JobId"], row["job_id"]))
		// Detect grouped or truncated IDs too: the direct query expands arrays.
		if strings.HasPrefix(id, a.jobID+"_") {
			return true
		}
	}
	return false
}

func (c *Client) arrayCancellationRows(ctx context.Context, machine, jobID string) ([]map[string]string, error) {
	// This value is interpolated into a remote command. Only a numeric parent
	// ID is allowed, independently of caller or accounting-response validation.
	if !slurmJobIDPattern.MatchString(jobID) || strings.Contains(jobID, "_") {
		return nil, fmt.Errorf("array cancellation accounting requires a numeric parent ID")
	}
	command := "LC_ALL=C sacct --noheader --parsable2 --allocations --array --jobs=" + jobID + " --format=JobID%64,JobIDRaw%64,State%64"
	result, err := c.RunCommand(ctx, machine, command)
	if err != nil {
		return nil, fmt.Errorf("array cancellation accounting: %w", err)
	}
	return parseArrayCancellationRows(result)
}

func parseArrayCancellationRows(result string) ([]map[string]string, error) {
	var envelope struct {
		Status   string  `json:"status"`
		Output   *string `json:"output"`
		Error    string  `json:"error"`
		ExitCode *int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		return nil, fmt.Errorf("decode array cancellation accounting result: %w", err)
	}
	if !strings.EqualFold(envelope.Status, "ok") || envelope.ExitCode == nil || *envelope.ExitCode != 0 || envelope.Error != "" || envelope.Output == nil {
		return nil, fmt.Errorf("array cancellation accounting command did not report success")
	}
	var rows []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(*envelope.Output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			return nil, fmt.Errorf("array cancellation accounting row has invalid fields")
		}
		id, raw, state := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
		if !slurmJobIDPattern.MatchString(id) || !slurmJobIDPattern.MatchString(raw) || strings.Contains(raw, "_") || state == "" {
			return nil, fmt.Errorf("array cancellation accounting row has invalid identity or state")
		}
		rows = append(rows, map[string]string{"jobid": id, "jobidraw": raw, "state": state})
	}
	return rows, nil
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
