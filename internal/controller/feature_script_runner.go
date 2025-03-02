package controller

import (
	"context"
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gpillon/k4all-operator/internal/controller/jobrunner"
)

// FeatureScriptConfig contains configuration for running a feature script
type FeatureScriptConfig struct {
	// Feature name (e.g. "argocd", "monitoring", etc.)
	FeatureName string
	// Script name without path (e.g. "setup-feature-argocd.sh")
	ScriptName string
	// Additional host utilities to symlink
	AdditionalUtilities []string
	// Additional environment variables to set
	Env map[string]string
}

// Helper function to add the standard K4all mounts
func addK4allMounts() []jobrunner.HostMount {
	return []jobrunner.HostMount{
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
	}
}

// RunFeatureScript runs a feature script as a Kubernetes job
func RunFeatureScript(ctx context.Context, c client.Client, featureConfig FeatureScriptConfig, toolsImage string) error {
	logger := log.FromContext(ctx)
	logger.Info("Launching job to set up feature", "feature", featureConfig.FeatureName)

	// Default utilities to symlink
	utilities := []string{"k4all-utils"}

	// Add the feature script itself
	utilities = append(utilities, featureConfig.ScriptName)

	// Add any additional utilities
	utilities = append(utilities, featureConfig.AdditionalUtilities...)

	// Generate symlink commands for all utilities
	symlinkCommands := ""
	for _, util := range utilities {
		symlinkCommands += fmt.Sprintf(`
# Set PATH
export PATH=/usr/local/bin:/usr/bin/:$PATH

# Create symlink for %s
if [ -f "/host/usr/local/bin/%s" ]; then
  echo "Creating symlink for %s"
  ln -sf /host/usr/local/bin/%s /usr/local/bin/%s
  chmod +x /usr/local/bin/%s
else
  echo "WARNING: Host file /usr/local/bin/%s not found!"
  ls -la /host/usr/local/bin/ || echo "Cannot list host directory"
fi
`, util, util, util, util, util, util, util)

	}

	// Create init script content with better debugging and host command support
	setupScript := fmt.Sprintf(`#!/bin/bash
set -e  # Exit on error

echo "Starting initialization script..."

# Create directories if they don't exist
mkdir -p /usr/local/bin /usr/local/host-bin
echo "Created /usr/local/bin directory"

# Create symlinks to host binaries
%s

# Create a hostname wrapper that uses chroot
echo "Creating hostname wrapper with chroot to access host commands"
cat > /usr/local/bin/hostname << 'EOF'
#!/bin/bash
# For -I flag (IP addresses)
if [ "$1" = "-I" ]; then
    # Method 1: Try using chroot to run hostname -I
    if command -v chroot >/dev/null 2>&1; then
        # Run hostname -I via chroot to the host filesystem
        RESULT=$(chroot /host /usr/bin/hostname -I 2>/dev/null)
        if [ $? -eq 0 ] && [ -n "$RESULT" ]; then
            echo "$RESULT"
            exit 0
        fi
    fi
    
    # Method 2: Parse network interfaces the BusyBox way (without -P)
    echo "Using network interface parsing"
    # Get all IPv4 addresses, skip loopback
    RESULT=$(grep -v "127.0.0." /proc/net/fib_trie | grep -o '[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}' | sort -u | tr '\n' ' ')
    echo "$RESULT"
    exit 0
fi

# For regular hostname command
# Method 1: Try chroot to the host filesystem
if command -v chroot >/dev/null 2>&1; then
    RESULT=$(chroot /host /usr/bin/hostname "$@" 2>/dev/null)
    if [ $? -eq 0 ]; then
        echo "$RESULT"
        exit 0
    fi
fi

# Method 2: Read from kernel hostname
if [ -f "/proc/sys/kernel/hostname" ]; then
    cat /proc/sys/kernel/hostname
    exit 0
fi

# Method 3: Read from host's hostname file
if [ -f "/host/etc/hostname" ]; then
    cat /host/etc/hostname
    exit 0
fi

# Method 4: Fall back to container hostname
/bin/hostname "$@"
EOF
chmod +x /usr/local/bin/hostname

# Also create a version in host-bin directory
cp /usr/local/bin/hostname /usr/local/host-bin/hostname
chmod +x /usr/local/host-bin/hostname

# Test hostname wrapper
echo "Testing basic hostname: $(hostname)"
echo "Testing IP addresses: $(hostname -I)"

# Ensure k4all-utils is available
if [ -f "/host/usr/local/bin/k4all-utils" ]; then
    echo "Found k4all-utils on host, copying to container"
    cp /host/usr/local/bin/k4all-utils /usr/local/bin/k4all-utils
    chmod +x /usr/local/bin/k4all-utils
else
    echo "WARNING: k4all-utils not found on host at /host/usr/local/bin/k4all-utils"
    # Search in other possible locations
    for dir in /host/usr/bin /host/bin /host/opt/k4all/bin; do
        if [ -f "$dir/k4all-utils" ]; then
            echo "Found k4all-utils at $dir/k4all-utils, copying to container"
            cp "$dir/k4all-utils" /usr/local/bin/k4all-utils
            chmod +x /usr/local/bin/k4all-utils
            break
        fi
    done
fi

# Check if we succeeded
if [ -f "/usr/local/bin/k4all-utils" ]; then
    echo "k4all-utils is available at /usr/local/bin/k4all-utils"
else
    echo "ERROR: k4all-utils not found on host, creating stub"
    # Create a stub that will show an error but won't fail the script
    cat > /usr/local/bin/k4all-utils << 'EOF'
#!/bin/bash
echo "WARNING: This is a stub k4all-utils because the real one wasn't found on the host"
echo "Command was: k4all-utils $@"
# Return success to avoid breaking scripts
exit 0
EOF
    chmod +x /usr/local/bin/k4all-utils
fi

# Set up host command proxying for ALL commands
echo "Creating host command proxy script"
cat > /usr/local/bin/host-command << 'EOF'
#!/bin/bash
cmd="$1"
shift
for dir in /host/usr/bin /host/bin /host/usr/sbin /host/sbin; do
    if [ -x "$dir/$cmd" ]; then
        echo "Using host command: $dir/$cmd"
        exec "$dir/$cmd" "$@"
        exit $?
    fi
done
echo "Command not found on host: $cmd, trying container version" >&2
command -v "$cmd" > /dev/null || { echo "Command not found in container either: $cmd" >&2; exit 127; }
exec "$cmd" "$@"
EOF
chmod +x /usr/local/bin/host-command

# Create directory for host command symlinks
mkdir -p /usr/local/host-bin

# Force use of host versions for critical commands
for cmd in ip ifconfig netstat ps ss ping traceroute kubectl helm jq find grep awk sed hostname; do
    echo "Creating forced symlink for host $cmd command"
    # Remove existing command if needed
    rm -f /usr/local/host-bin/$cmd
    # Create the wrapper that will call the host version
    cat > /usr/local/host-bin/$cmd << EOF
#!/bin/bash
/usr/local/bin/host-command $cmd "\$@"
EOF
    chmod +x /usr/local/host-bin/$cmd
done

# Add host-bin to the BEGINNING of PATH to ensure it's used first
export PATH="/usr/local/host-bin:$PATH"
echo 'export PATH="/usr/local/host-bin:$PATH"' > /etc/profile.d/host-path.sh
echo 'export PATH="/usr/local/host-bin:$PATH"' >> ~/.bashrc

# Verify host bin directory is set up correctly
echo "Host command bin directory contents:"
ls -la /usr/local/host-bin/

# Check the hostname command specifically
which hostname
echo "Hostname from container: $(hostname)"
echo "Hostname from host command: $(/usr/local/bin/host-command hostname)"

# Check if argocd-values.yaml exists
if [ -f "/usr/local/share/argocd-values.yaml" ]; then
  echo "Found argocd-values.yaml in /usr/local/share/"
else
  echo "WARNING: /usr/local/share/argocd-values.yaml not found!"
  echo "Host directory contents:"
  ls -la /host/usr/local/share/ || echo "Cannot list host directory"
  
  # Ensure directory exists
  mkdir -p /usr/local/share
  
  # Check for alternative locations
  if [ -f "/host/usr/local/share/argocd-values.yaml" ]; then
    echo "Found argocd-values.yaml in host path, copying..."
    cp /host/usr/local/share/argocd-values.yaml /usr/local/share/
  fi
fi

echo "Current PATH: $PATH"
echo "Initialization complete"
`, symlinkCommands)

	// Prepare environment variables
	envVars := []corev1.EnvVar{}
	for key, value := range featureConfig.Env {
		envVars = append(envVars, corev1.EnvVar{
			Name:  key,
			Value: value,
		})
	}

	// Add debug environment variable
	envVars = append(envVars, corev1.EnvVar{
		Name:  "K4ALL_DEBUG",
		Value: "true",
	})

	// Configure and run the job
	jobConfig := jobrunner.JobConfig{
		NamePrefix:     fmt.Sprintf("%s-setup", featureConfig.FeatureName),
		Namespace:      "k4all-operator-system",
		Image:          toolsImage,
		Command:        []string{"/bin/bash", filepath.Join("/usr/local/bin", featureConfig.ScriptName)},
		ServiceAccount: "k4all-operator-controller-manager",
		Labels: map[string]string{
			"app.kubernetes.io/name":      "k4all-operator",
			"app.kubernetes.io/component": "feature-installer",
			"app.kubernetes.io/part-of":   "k4all-operator",
			"feature":                     featureConfig.FeatureName,
		},
		MountHostRoot:     true,
		HostRootMountPath: "/host",
		InitScript:        setupScript,
		EnvVars:           envVars,
		AdditionalMounts:  addK4allMounts(),
	}

	if err := jobrunner.RunJob(ctx, c, jobConfig); err != nil {
		return fmt.Errorf("failed to run %s setup job: %w", featureConfig.FeatureName, err)
	}

	logger.Info("Feature setup job completed successfully", "feature", featureConfig.FeatureName)
	return nil
}

// RunFeatureScriptDirect runs a feature script directly from the host path
func RunFeatureScriptDirect(ctx context.Context, c client.Client, featureConfig FeatureScriptConfig, toolsImage string) error {
	logger := log.FromContext(ctx)
	logger.Info("Launching job to set up feature (direct mode)", "feature", featureConfig.FeatureName)

	// Prepare environment variables
	envVars := []corev1.EnvVar{}
	for key, value := range featureConfig.Env {
		envVars = append(envVars, corev1.EnvVar{
			Name:  key,
			Value: value,
		})
	}

	// Add debug environment variable
	envVars = append(envVars, corev1.EnvVar{
		Name:  "K4ALL_DEBUG",
		Value: "true",
	})

	// Configure and run the job
	jobConfig := jobrunner.JobConfig{
		NamePrefix: fmt.Sprintf("%s-setup-direct", featureConfig.FeatureName),
		Namespace:  "k4all-operator-system",
		Image:      toolsImage,
		// Use the script directly from the host path
		Command:        []string{"/bin/bash", "/host/usr/local/bin/" + featureConfig.ScriptName},
		ServiceAccount: "k4all-operator-controller-manager",
		Labels: map[string]string{
			"app.kubernetes.io/name":      "k4all-operator",
			"app.kubernetes.io/component": "feature-installer",
			"app.kubernetes.io/part-of":   "k4all-operator",
			"feature":                     featureConfig.FeatureName,
		},
		MountHostRoot:     true,
		HostRootMountPath: "/host",
		EnvVars:           envVars,
		AdditionalMounts:  addK4allMounts(),
	}

	if err := jobrunner.RunJob(ctx, c, jobConfig); err != nil {
		return fmt.Errorf("failed to run %s setup job (direct mode): %w", featureConfig.FeatureName, err)
	}

	logger.Info("Feature setup job completed successfully (direct mode)", "feature", featureConfig.FeatureName)
	return nil
}
