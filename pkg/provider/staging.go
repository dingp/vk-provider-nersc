package provider

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"vk-provider-nersc/pkg/superfacility"
)

func buildStagingState(pod *corev1.Pod, jobScratchBase string, volumeScratchPaths map[string]string) (*podStagingState, error) {
	inputSource := getAnnotation(pod, annotationInputSource)
	outputDest := getAnnotation(pod, annotationOutputDest)
	stageOut, err := getBoolAnnotation(pod, annotationStageOut)
	if err != nil {
		return nil, err
	}

	if inputSource == "" && !stageOut {
		return nil, nil
	}

	state := &podStagingState{}
	if inputSource != "" {
		inputStagePath, err := resolveStagePath(pod, jobScratchBase, volumeScratchPaths, annotationInputVolume)
		if err != nil {
			return nil, err
		}
		input, err := parseGlobusLocation(inputSource)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", annotationInputSource, err)
		}
		state.inputSource = input
		state.inputTargetDir = inputStagePath
	}

	if stageOut {
		outputStagePath, err := resolveStagePath(pod, jobScratchBase, volumeScratchPaths, annotationOutputVolume)
		if err != nil {
			return nil, err
		}
		if outputDest == "" {
			return nil, fmt.Errorf("%s must be set when %s is true", annotationOutputDest, annotationStageOut)
		}
		output, err := parseGlobusLocation(outputDest)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", annotationOutputDest, err)
		}
		state.outputDest = output
		state.outputSourceDir = outputStagePath
		state.outputRequest = &superfacility.GlobusTransferRequest{
			SourceUUID: "perlmutter",
			TargetUUID: output.Endpoint,
			SourceDir:  outputStagePath,
			TargetDir:  output.Path,
			Username:   getAnnotation(pod, annotationGlobusUsername),
		}
	}

	return state, nil
}

func parseGlobusLocation(raw string) (*globusLocation, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid Globus URI: %w", err)
	}
	if parsed.Scheme != "globus" {
		return nil, fmt.Errorf("expected globus:// endpoint URI")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("missing Globus endpoint")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return nil, fmt.Errorf("missing Globus path")
	}
	return &globusLocation{
		Endpoint: parsed.Host,
		Path:     parsed.Path,
	}, nil
}

func resolveStagePath(pod *corev1.Pod, jobScratchBase string, volumeScratchPaths map[string]string, specificAnnotation string) (string, error) {
	annotationUsed := specificAnnotation
	stageVolume := getAnnotation(pod, specificAnnotation)
	if stageVolume == "" {
		stageVolume = getAnnotation(pod, annotationStageVolume)
		if stageVolume != "" {
			annotationUsed = annotationStageVolume
		}
	}
	if stageVolume != "" {
		path, ok := volumeScratchPaths[stageVolume]
		if !ok {
			return "", fmt.Errorf("%s references unknown volume %q", annotationUsed, stageVolume)
		}
		return path, nil
	}

	if len(volumeScratchPaths) == 0 {
		return jobScratchBase, nil
	}
	if len(volumeScratchPaths) == 1 {
		for _, path := range volumeScratchPaths {
			return path, nil
		}
	}

	volumeNames := make([]string, 0, len(volumeScratchPaths))
	for name := range volumeScratchPaths {
		volumeNames = append(volumeNames, name)
	}
	sort.Strings(volumeNames)
	return "", fmt.Errorf("%s or %s is required when staging with multiple volumes: %s", specificAnnotation, annotationStageVolume, strings.Join(volumeNames, ", "))
}

func getAnnotation(pod *corev1.Pod, key string) string {
	if pod == nil || pod.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(pod.Annotations[key])
}

func getBoolAnnotation(pod *corev1.Pod, key string) (bool, error) {
	value := getAnnotation(pod, key)
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func (p *NerscProvider) startAndWaitForTransfer(ctx context.Context, client jobClient, req superfacility.GlobusTransferRequest) (string, error) {
	transfer, err := client.StartGlobusTransfer(ctx, req)
	if err != nil {
		return "", err
	}

	transferID := transfer.TransferID()
	timeout := p.transferTimeout
	if timeout <= 0 {
		timeout = defaultTransferTimeout
	}
	pollInterval := p.transferPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultTransferPollInterval
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		result, err := client.CheckGlobusTransfer(waitCtx, transferID)
		if err != nil {
			return transferID, err
		}
		done, failed := result.IsComplete()
		if done && failed {
			return transferID, fmt.Errorf("globus transfer %s failed: %s", transferID, result.Summary())
		}
		if done {
			return transferID, nil
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return transferID, fmt.Errorf("globus transfer %s timed out after %s", transferID, timeout)
		case <-timer.C:
		}
	}
}

func (p *NerscProvider) reconcileStageOut(ctx context.Context, key, token string) corev1.PodStatus {
	client, err := p.clientForToken(token)
	if err != nil {
		msg := fmt.Sprintf("create Superfacility client: %v", err)
		p.setStageOutStatus(key, transferFailed, "", msg)
		return podStatus(corev1.PodFailed, "StageOutFailed", msg)
	}

	req, transferID, status, outputErr := p.stageOutSnapshot(key)
	switch status {
	case transferSucceeded:
		return podStatus(corev1.PodSucceeded, "StageOutComplete", "Output data staged out")
	case transferFailed:
		return podStatus(corev1.PodFailed, "StageOutFailed", outputErr)
	case transferStarting:
		return podStatus(corev1.PodRunning, "StageOutStarting", "Starting output data transfer")
	}

	if status == transferNotStarted {
		p.setStageOutStatus(key, transferStarting, "", "")
		transfer, err := client.StartGlobusTransfer(ctx, req)
		if err != nil {
			msg := fmt.Sprintf("start output transfer: %v", err)
			p.setStageOutStatus(key, transferFailed, "", msg)
			return podStatus(corev1.PodFailed, "StageOutFailed", msg)
		}
		transferID = transfer.TransferID()
		p.setStageOutStatus(key, transferRunning, transferID, "")
		log.Printf("Pod %s output stage-out started as Globus transfer %s", key, transferID)
	}

	result, err := client.CheckGlobusTransfer(ctx, transferID)
	if err != nil {
		msg := fmt.Sprintf("check output transfer %s: %v", transferID, err)
		p.setStageOutStatus(key, transferFailed, transferID, msg)
		return podStatus(corev1.PodFailed, "StageOutFailed", msg)
	}
	done, failed := result.IsComplete()
	if done && failed {
		msg := fmt.Sprintf("globus transfer %s failed: %s", transferID, result.Summary())
		p.setStageOutStatus(key, transferFailed, transferID, msg)
		return podStatus(corev1.PodFailed, "StageOutFailed", msg)
	}
	if done {
		p.setStageOutStatus(key, transferSucceeded, transferID, "")
		return podStatus(corev1.PodSucceeded, "StageOutComplete", "Output data staged out")
	}

	return podStatus(corev1.PodRunning, "StageOutRunning", fmt.Sprintf("Output transfer %s is still running", transferID))
}

func (p *NerscProvider) stageOutSnapshot(key string) (superfacility.GlobusTransferRequest, string, transferStatus, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	staging := p.stagingMap[key]
	if staging == nil || staging.outputRequest == nil {
		return superfacility.GlobusTransferRequest{}, "", transferSucceeded, ""
	}
	return *staging.outputRequest, staging.outputTransferID, staging.outputStatus, staging.outputError
}

func (p *NerscProvider) setStageOutStatus(key string, status transferStatus, transferID, outputErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	staging := p.stagingMap[key]
	if staging == nil {
		return
	}
	staging.outputStatus = status
	if transferID != "" {
		staging.outputTransferID = transferID
	}
	staging.outputError = outputErr
}

func podStatus(phase corev1.PodPhase, reason, message string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase:   phase,
		Reason:  reason,
		Message: message,
	}
}
