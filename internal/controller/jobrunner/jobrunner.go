package jobrunner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JobConfig contains configuration for creating a job
type JobConfig struct {
	// Name prefix for the job
	NamePrefix string
	// Namespace to create the job in
	Namespace string
	// Image to use for the job
	Image string
	// Command to run in the container
	Command []string
	// Service account to run the job as
	ServiceAccount string
	// Labels to apply to the job
	Labels map[string]string
	// Whether to mount the entire host filesystem
	MountHostRoot bool
	// Root mount point in the container
	HostRootMountPath string
	// Optional init script to run before the main command
	InitScript string
	// Environment variables to set in the container
	EnvVars []corev1.EnvVar
	// Additional host paths to mount
	AdditionalMounts []HostMount
	// BackoffLimit for the job
	BackoffLimit int32
	// Callback function to process logs from the job
	OutputCallback func(pod, containerName, log string)
	// Whether to delete the job after it completes (default: true)
	DeleteAfterCompletion bool
}

// HostMount defines a host path to mount in the container
type HostMount struct {
	// Host path to mount
	HostPath string
	// Container mount path
	ContainerPath string
	// Mount type (file or directory)
	Type corev1.HostPathType
}

// Add at package level
var (
	outputCallbacks      = make(map[string]func(string, string, string))
	outputCallbacksMutex sync.Mutex
)

// RunJob creates and runs a kubernetes job with the given configuration
func RunJob(ctx context.Context, k8sClient client.Client, jobConfig JobConfig) error {
	logger := log.FromContext(ctx)

	// Basic validation
	if jobConfig.Image == "" {
		return fmt.Errorf("job image cannot be empty")
	}
	if len(jobConfig.Command) == 0 {
		return fmt.Errorf("job command cannot be empty")
	}
	if jobConfig.Namespace == "" {
		jobConfig.Namespace = "k4all-operator-system" // Default namespace
		logger.Info("Using default namespace", "namespace", jobConfig.Namespace)
	}

	// Set default for deletion if not specified (default to true)
	if !jobConfig.DeleteAfterCompletion {
		// This line intentionally left as explicit check against false
		// to make it obvious that the default is true when not specified
		jobConfig.DeleteAfterCompletion = true
	}

	// Generate a unique job name with timestamp
	jobName := fmt.Sprintf("%s-%d", jobConfig.NamePrefix, time.Now().Unix())

	// Default host root mount path if not specified
	if jobConfig.MountHostRoot && jobConfig.HostRootMountPath == "" {
		jobConfig.HostRootMountPath = "/host"
	}

	// If init script is provided, modify the command to run the init script first
	var command []string
	if jobConfig.InitScript != "" {
		// Create a simpler wrapper script
		wrapperScript := fmt.Sprintf(`#!/bin/bash
set -e

# Save the init script
cat > /tmp/init.sh << 'EOL'
%s
EOL

# Make it executable
chmod +x /tmp/init.sh

# Run the init script
echo "Running init script..."
/tmp/init.sh
echo "Init script finished"

# Set up PATH for host commands
if [ -d "/usr/local/host-bin" ]; then
    export PATH="/usr/local/host-bin:$PATH"
    echo "Added host commands to PATH: $PATH"
fi

# Running main command
echo "Running main command: %s"
exec %s
`, jobConfig.InitScript,
			strings.Join(jobConfig.Command, " "),
			strings.Join(jobConfig.Command, " "))

		// Use bash to run our wrapper script
		command = []string{"/bin/bash", "-c", wrapperScript}
	} else {
		// If direct execution is requested, modify the command wrapper
		if jobConfig.Command[0] == "/bin/bash" && strings.HasPrefix(jobConfig.Command[1], "/host/") {
			// Create a simple wrapper that works with minimal changes
			wrapperScript := fmt.Sprintf(`#!/bin/bash
set -e

# Setup for more reliable host command access
mkdir -p /usr/local/bin /usr/local/host-bin

# DEBUGGING
echo "======== DEBUG INFO ========"
echo "Script path: %s"
echo "Host path contents:"
ls -la /host/usr/local/bin/ | grep helm || echo "No helm found in ls output"
echo "Host helm exists check: $(test -f /host/usr/local/bin/helm && echo YES || echo NO)"
echo "Host helm executable check: $(test -x /host/usr/local/bin/helm && echo YES || echo NO)"
echo "Host helm access:"
cat /host/usr/local/bin/helm 2>/dev/null | head -n 2 || echo "Cannot read helm file"
echo "======== END DEBUG ========"

# DIRECT COPY APPROACH - Simple and direct
if [ -f "/host/usr/local/bin/helm" ]; then
    echo "Found helm at /host/usr/local/bin/helm - Copying directly"
    cp /host/usr/local/bin/helm /bin/helm
    chmod +x /bin/helm
    echo "Copied helm to /bin/helm for direct access"
else
    echo "ERROR: Cannot find helm at /host/usr/local/bin/helm"
fi

# Set up host command environment the way it worked before
cat > /usr/local/bin/host-command << 'EOF'
#!/bin/bash
# Simple host command wrapper
cmd="$1"
shift
# Try direct binary for known commands
if [ "$cmd" = "helm" ] && [ -x "/bin/helm" ]; then
    exec /bin/helm "$@"
    exit $?
fi
# Otherwise try host paths
for dir in /host/usr/local/bin /host/usr/bin /host/bin; do
    if [ -x "$dir/$cmd" ]; then
        exec "$dir/$cmd" "$@"
        exit $?
    fi
done
# Last resort - just try the container command
echo "Command not found on host: $cmd, trying container version" >&2
exec "$cmd" "$@"
EOF
chmod +x /usr/local/bin/host-command

# Create a separate helm wrapper for reliability
cat > /usr/local/bin/helm << 'EOF'
#!/bin/bash
# Direct helm wrapper
if [ -x "/bin/helm" ]; then
    exec /bin/helm "$@"
    exit $?
elif [ -x "/host/usr/local/bin/helm" ]; then
    exec /host/usr/local/bin/helm "$@"
    exit $?
else
    echo "ERROR: Cannot find helm binary"
    exit 1
fi
EOF
chmod +x /usr/local/bin/helm

# Copy k4all-utils if needed
if [ -f "/host/usr/local/bin/k4all-utils" ]; then
    cp /host/usr/local/bin/k4all-utils /usr/local/bin/k4all-utils
    chmod +x /usr/local/bin/k4all-utils
fi

# Show final status
echo "Final path setup:"
echo "PATH=$PATH"
echo "Helm location: $(which helm)"
echo "Helm version: $(helm version 2>/dev/null || echo 'Cannot get helm version')"

# Execute the script
echo "Running script: %s"
exec %s %s
`,
				jobConfig.Command[1],                       // Debug info
				jobConfig.Command[1],                       // First %s - prints the script name
				jobConfig.Command[0], jobConfig.Command[1]) // Second and third %s - command and argument

			command = []string{"/bin/bash", "-c", wrapperScript}
		} else {
			// For direct commands without init script, still ensure host commands are available
			wrapperScript := fmt.Sprintf(`#!/bin/bash
set -e

# Set up host command proxying if not already done
if [ ! -d "/usr/local/host-bin" ]; then
    mkdir -p /usr/local/host-bin
    
    # Create simple host command proxy if it doesn't exist
    if [ ! -f "/usr/local/bin/host-command" ]; then
        cat > /usr/local/bin/host-command << 'EOF'
#!/bin/bash
cmd="$1"
shift
for dir in /host/usr/bin /host/bin /host/usr/sbin /host/sbin; do
    if [ -x "$dir/$cmd" ]; then
        exec "$dir/$cmd" "$@"
        exit $?
    fi
done
echo "Command not found: $cmd" >&2
exit 127
EOF
        chmod +x /usr/local/bin/host-command
    fi
    
    # Create symlinks for common commands that might be missing
    for cmd in hostname ip ifconfig netstat ps ss ping traceroute; do
        if ! command -v $cmd &>/dev/null; then
            echo "Creating symlink for host $cmd command"
            ln -sf /usr/local/bin/host-command /usr/local/host-bin/$cmd
        fi
    done
fi

# Add host-bin to PATH
export PATH="/usr/local/host-bin:$PATH"

# Run the command
exec %s
`,
				strings.Join(jobConfig.Command, " "))

			command = []string{"/bin/bash", "-c", wrapperScript}
		}
	}

	// Set up the job spec
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: jobConfig.Namespace,
			Labels:    jobConfig.Labels,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: jobConfig.Labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: jobConfig.ServiceAccount,
					// Use host network namespace - critical for hostname -I
					HostNetwork: true,
					// Also use host's UTS namespace for hostname commands
					HostIPC: true,
					HostPID: true,
					// Set hostname to match the node
					Hostname: "node-hostname",
					Containers: []corev1.Container{
						{
							Name:    "script-runner",
							Image:   jobConfig.Image,
							Command: command,
							Env:     jobConfig.EnvVars,
						},
					},
					// Add tolerations for common taints
					Tolerations: []corev1.Toleration{
						{
							Key:      "node.kubernetes.io/disk-pressure",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node.kubernetes.io/memory-pressure",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node.kubernetes.io/network-unavailable",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node.kubernetes.io/not-ready",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node.kubernetes.io/pid-pressure",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node.kubernetes.io/unreachable",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
			BackoffLimit: &jobConfig.BackoffLimit,
		},
	}

	// Add host root volume mount if requested
	if jobConfig.MountHostRoot {
		// Add volume mount to container
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      "host-root",
				MountPath: jobConfig.HostRootMountPath,
			},
		)

		// Add volume to pod
		job.Spec.Template.Spec.Volumes = append(
			job.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "host-root",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/",
						Type: func() *corev1.HostPathType {
							t := corev1.HostPathDirectory
							return &t
						}(),
					},
				},
			},
		)

		// Add security context for host access permissions
		privileged := true
		job.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
			Privileged: &privileged,
		}
	}

	// Add additional mounts
	// Default mounts for k4all directories and config
	if len(jobConfig.AdditionalMounts) == 0 {
		// Add default mounts for k4all
		jobConfig.AdditionalMounts = []HostMount{
			{
				HostPath:      "/etc/k4all-config.json",
				ContainerPath: "/etc/k4all-config.json",
				Type:          corev1.HostPathFile,
			},
			{
				HostPath:      "/opt/k4all",
				ContainerPath: "/opt/k4all",
				Type:          corev1.HostPathDirectoryOrCreate,
			},
			{
				HostPath:      "/usr/local/share",
				ContainerPath: "/usr/local/share",
				Type:          corev1.HostPathDirectoryOrCreate,
			},
			{
				HostPath:      "/usr/local/bin",
				ContainerPath: "/usr/local/bin",
				Type:          corev1.HostPathDirectoryOrCreate,
			},
			{
				HostPath:      "/dev",
				ContainerPath: "/dev",
				Type:          corev1.HostPathDirectory,
			},
		}
	}

	// Process all additional mounts
	for i, mount := range jobConfig.AdditionalMounts {
		volumeName := fmt.Sprintf("host-volume-%d", i)

		// Add volume mount to container
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      volumeName,
				MountPath: mount.ContainerPath,
			},
		)

		// Add volume to pod
		job.Spec.Template.Spec.Volumes = append(
			job.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: mount.HostPath,
						Type: &mount.Type,
					},
				},
			},
		)
	}

	// Add at package level
	if jobConfig.OutputCallback != nil {
		// Store the callback for later use
		jobKey := fmt.Sprintf("%s/%s", jobConfig.Namespace, jobName)
		outputCallbacksMutex.Lock()
		outputCallbacks[jobKey] = jobConfig.OutputCallback
		outputCallbacksMutex.Unlock()

		// Add annotation to indicate this job has a callback
		if job.Spec.Template.Annotations == nil {
			job.Spec.Template.Annotations = make(map[string]string)
		}
		job.Spec.Template.Annotations["jobrunner.k4all.io/has-output-callback"] = "true"
	}

	// Create the job
	logger.Info("Creating job with tolerations", "name", jobName, "namespace", jobConfig.Namespace)
	if err := k8sClient.Create(ctx, job); err != nil {
		logger.Error(err, "Failed to create job", "name", jobName)
		return fmt.Errorf("failed to create job: %w", err)
	}

	// Wait for job completion
	namespacedName := types.NamespacedName{
		Name:      jobName,
		Namespace: jobConfig.Namespace,
	}
	err := waitForJobCompletion(ctx, k8sClient, namespacedName)

	// Clean up the job if configured to do so
	if jobConfig.DeleteAfterCompletion {
		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, namespacedName, job)
		if err == nil {
			logger.Info("Deleting job after completion", "job", jobName, "namespace", jobConfig.Namespace)

			// Find and delete pods associated with the job first
			podList := &corev1.PodList{}
			if err := k8sClient.List(ctx, podList,
				client.InNamespace(jobConfig.Namespace),
				client.MatchingLabels{"job-name": jobName}); err == nil {

				for _, pod := range podList.Items {
					logger.Info("Deleting pod", "pod", pod.Name, "namespace", pod.Namespace)
					if err := k8sClient.Delete(ctx, &pod); err != nil {
						logger.Error(err, "Failed to delete pod", "pod", pod.Name)
					}
				}
			}

			// Delete the job
			deleteOptions := &client.DeleteOptions{
				PropagationPolicy: func() *metav1.DeletionPropagation {
					policy := metav1.DeletePropagationBackground
					return &policy
				}(),
			}
			if err := k8sClient.Delete(ctx, job, deleteOptions); err != nil {
				logger.Error(err, "Failed to delete job after completion",
					"job", jobName, "namespace", jobConfig.Namespace)
			}
		}
	}

	return err
}

// waitForJobCompletion waits for a job to complete successfully
func waitForJobCompletion(ctx context.Context, k8sClient client.Client, namespacedName types.NamespacedName) error {
	logger := log.FromContext(ctx)
	var outputCallback func(string, string, string)
	job := &batchv1.Job{}

	// Get the job to find the outputCallback configuration
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(ctx, namespacedName, job); err != nil {
			logger.Error(err, "Failed to get job")
			time.Sleep(5 * time.Second)
			continue
		}

		if job.Status.Active == 0 {
			break
		}

		logger.Info("Waiting for job to complete", "name", namespacedName.Name, "active", job.Status.Active)
		time.Sleep(5 * time.Second)
	}

	// Store OutputCallback for later use
	outputCallback = nil
	if job.Spec.Template.Annotations != nil {
		if _, hasCallback := job.Spec.Template.Annotations["jobrunner.k4all.io/has-output-callback"]; hasCallback {
			// Use the global variable to store callback function for the job
			outputCallbacksMutex.Lock()
			if cb, ok := outputCallbacks[namespacedName.String()]; ok {
				outputCallback = cb
			}
			outputCallbacksMutex.Unlock()
		}
	}

	// Poll for job completion
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, namespacedName, job); err != nil {
			logger.Error(err, "Failed to get job status")
			return false, err
		}

		// Check if job is complete
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				logger.Info("Job completed successfully", "name", namespacedName.Name)
				return true, nil
			}
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				logger.Error(nil, "Job failed", "name", namespacedName.Name, "reason", condition.Reason)
				return false, fmt.Errorf("job failed: %s", condition.Reason)
			}
		}

		logger.Info("Job still running", "name", namespacedName.Name,
			"active", job.Status.Active, "succeeded", job.Status.Succeeded)
		return false, nil
	})

	// Job is complete (success or failure), now collect logs if we have a callback
	if outputCallback != nil {
		// Find the pod associated with this job
		podList := &corev1.PodList{}
		if err := k8sClient.List(ctx, podList, client.InNamespace(namespacedName.Namespace),
			client.MatchingLabels{"job-name": namespacedName.Name}); err != nil {
			logger.Error(err, "Failed to list pods for job")
			return err
		}

		// Process logs for each pod
		for _, pod := range podList.Items {
			for _, container := range pod.Spec.Containers {
				logs, err := getPodLogs(ctx, k8sClient, pod.Name, pod.Namespace, container.Name)
				if err != nil {
					logger.Error(err, "Failed to get logs", "pod", pod.Name, "container", container.Name)
					continue
				}

				// Call the callback with the logs
				outputCallback(pod.Name, container.Name, logs)
			}
		}
	}

	return err
}

// Helper function to get pod logs
func getPodLogs(ctx context.Context, k8sClient client.Client, podName, namespace, containerName string) (string, error) {
	// Get REST config and create clientset
	restConfig, err := config.GetConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get REST config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	// Get logs using clientset
	podLogs, err := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
	}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// RunHelmCommand creates and runs a kubernetes job to execute a helm command
func RunHelmCommand(ctx context.Context, k8sClient client.Client, args []string, namespace string,
	toolsImage string) (string, error) {
	logger := log.FromContext(ctx)

	if len(args) == 0 {
		return "", fmt.Errorf("helm command arguments cannot be empty")
	}

	// Format the command for logging
	helmCmdStr := fmt.Sprintf("helm %s", strings.Join(args, " "))
	logger.Info("Running helm command via container job", "command", helmCmdStr)

	// Create job name with prefix "helm-"
	jobName := fmt.Sprintf("helm-%d", time.Now().Unix())

	// Create a script that captures the helm command output
	helmScript := fmt.Sprintf(`#!/bin/bash
set -e

# Create output directory
mkdir -p /tmp/helm-output

# Try to use host's helm if available, otherwise use container's
if [ -f "/host/usr/local/bin/helm" ]; then
    echo "Using host's helm binary"
    cp /host/usr/local/bin/helm /bin/helm
    chmod +x /bin/helm
fi

echo "Running helm command: helm %s"
# Execute helm command and capture output to file
helm %s > /tmp/helm-output/result.txt 2>/tmp/helm-output/error.txt || true
EXIT_CODE=$?

# Save exit code
echo $EXIT_CODE > /tmp/helm-output/exit-code.txt

# Print the output for the logs
echo "=== Helm Command Output ==="
cat /tmp/helm-output/result.txt
echo "=== Helm Command Error Output ==="
cat /tmp/helm-output/error.txt
echo "=== End of Helm Command Output ==="

# Don't exit, just return the exit code for the main script
echo "Helm command completed with exit code: $EXIT_CODE"
exit $EXIT_CODE
`, strings.Join(args, " "), strings.Join(args, " "))

	// Configure and run the job
	jobConfig := JobConfig{
		NamePrefix: jobName,
		Namespace:  namespace,
		Image:      toolsImage,
		// Use a dummy echo command to avoid the "requires an argument" error
		Command:           []string{"/bin/bash", "-c", "echo 'Job completed'"},
		ServiceAccount:    "k4all-operator-controller-manager",
		MountHostRoot:     true,
		HostRootMountPath: "/host",
		Labels: map[string]string{
			"app.kubernetes.io/name":      "k4all-operator",
			"app.kubernetes.io/component": "helm-runner",
			"app.kubernetes.io/part-of":   "k4all-operator",
		},
		BackoffLimit: 2,
		// Use the helm script as the init script
		InitScript: helmScript,
		// Always delete helm jobs after completion
		DeleteAfterCompletion: true,
	}

	// Create a buffer to collect job output
	var outputBuffer, errorBuffer strings.Builder

	// Add a callback to collect the output
	jobConfig.OutputCallback = func(pod, containerName, logText string) {
		// Check if this log contains our markers
		outputStart := strings.Index(logText, "=== Helm Command Output ===")
		if outputStart >= 0 {
			// Extract the output sections
			errorStart := strings.Index(logText, "=== Helm Command Error Output ===")
			endMarker := strings.Index(logText, "=== End of Helm Command Output ===")

			if errorStart > outputStart && endMarker > errorStart {
				// Get the standard output (between output marker and error marker)
				output := strings.TrimSpace(logText[outputStart+len("=== Helm Command Output ===")+1 : errorStart])
				outputBuffer.WriteString(output)

				// Get the error output (between error marker and end marker)
				errorOutput := strings.TrimSpace(logText[errorStart+len("=== Helm Command Error Output ===")+1 : endMarker])
				errorBuffer.WriteString(errorOutput)

				logger.Info("Captured helm command output",
					"standardOutput", output,
					"errorOutput", errorOutput)
			}
		}

		logger.Info("Processing log output",
			"logLength", len(logText),
			"containsMarkers", strings.Contains(logText, "=== Helm Command Output ==="),
			"outputStart", outputStart)
	}

	logger.Info("Running job", "name", jobName, "namespace", namespace)

	// Run the job
	err := RunJob(ctx, k8sClient, jobConfig)
	if err != nil {
		// If job failed but we captured error output, return that as part of the error
		if errorBuffer.Len() > 0 {
			return outputBuffer.String(), fmt.Errorf("helm command failed: %w\nError output: %s",
				err, errorBuffer.String())
		}
		return outputBuffer.String(), fmt.Errorf("helm command failed: %w", err)
	}

	logger.Info("Job completed", "name", jobName, "namespace", namespace, "output", outputBuffer.String(), "error", errorBuffer.String())

	if errorBuffer.Len() > 0 {
		return outputBuffer.String(), fmt.Errorf("helm command error: %s", errorBuffer.String())
	}
	return outputBuffer.String(), nil
}

// RunHostCommand runs a command on the host and returns its output
func RunHostCommand(ctx context.Context, k8sClient client.Client, cmdArgs []string, namespace string,
	toolsImage string) (string, error) {
	logger := log.FromContext(ctx)

	if len(cmdArgs) == 0 {
		return "", fmt.Errorf("command arguments cannot be empty")
	}

	// Format the command for logging
	cmdStr := strings.Join(cmdArgs, " ")
	logger.Info("Running host command via container job", "command", cmdStr, "args", cmdArgs)

	// Create job name with prefix "host-cmd-"
	jobName := fmt.Sprintf("host-cmd-%d", time.Now().Unix())

	initScript := fmt.Sprintf(`#! /bin/bash
# Link utils
echo "Sourcing utils"
# source utils only if the file exists, else source k4all-utils
if [ -f /usr/local/bin/control-plane-utils ]; then
    source /usr/local/bin/control-plane-utils
else
    source /usr/local/bin/k4all-utils
fi

# Create output directory
mkdir -p /tmp/cmd-output

# Execute the host command using the host-command wrapper
echo "Running host command: %s"
%s > /tmp/cmd-output/result.txt 2>/tmp/cmd-output/error.txt || true
EXIT_CODE=$?

# Print the output for the logs
echo "=== Command Output ==="
cat /tmp/cmd-output/result.txt
echo "=== Command Error Output ==="
cat /tmp/cmd-output/error.txt
echo "=== End of Command Output ==="

echo "Command completed with exit code: $EXIT_CODE"
exit $EXIT_CODE

echo "Utils sourced, cluster ip: "
`, cmdStr, cmdStr)

	// Configure and run the job
	jobConfig := JobConfig{
		NamePrefix: jobName,
		Namespace:  namespace,
		Image:      toolsImage,
		// Use a dummy echo command - the init script does the real work
		Command:           []string{"echo", "Done"},
		ServiceAccount:    "k4all-operator-controller-manager",
		MountHostRoot:     true,
		HostRootMountPath: "/host",
		Labels: map[string]string{
			"app.kubernetes.io/name":      "k4all-operator",
			"app.kubernetes.io/component": "host-command-runner",
			"app.kubernetes.io/part-of":   "k4all-operator",
		},
		BackoffLimit: 2,
		InitScript:   initScript,
		// Always delete host command jobs after completion
		DeleteAfterCompletion: true,
	}

	// Create buffers to collect output
	var outputBuffer, errorBuffer strings.Builder

	// Add a callback to collect the output
	jobConfig.OutputCallback = func(pod, containerName, logText string) {
		outputStart := strings.Index(logText, "=== Command Output ===")
		if outputStart >= 0 {
			errorStart := strings.Index(logText, "=== Command Error Output ===")
			endMarker := strings.Index(logText, "=== End of Command Output ===")

			if errorStart > outputStart && endMarker > errorStart {
				output := strings.TrimSpace(logText[outputStart+len("=== Command Output ===")+1 : errorStart])
				outputBuffer.WriteString(output)

				errorOutput := strings.TrimSpace(logText[errorStart+len("=== Command Error Output ===")+1 : endMarker])
				errorBuffer.WriteString(errorOutput)

				logger.Info("Captured command output",
					"standardOutput", output,
					"errorOutput", errorOutput)
			}
		}
	}

	// Run the job
	err := RunJob(ctx, k8sClient, jobConfig)
	if err != nil {
		if errorBuffer.Len() > 0 {
			return outputBuffer.String(), fmt.Errorf("command failed: %w\nError output: %s",
				err, errorBuffer.String())
		}
		return outputBuffer.String(), fmt.Errorf("command failed: %w", err)
	}

	return strings.TrimSpace(outputBuffer.String()), nil
}
