package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var managerBin string

func TestMain(m *testing.M) {
	managerBin = "/src/balena-extension-manager"
	if _, err := os.Stat(managerBin); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "manager binary not found at %s\n", managerBin)
		os.Exit(1)
	}

	waitForDocker()

	os.Exit(m.Run())
}

// waitForDocker polls `docker info` until the daemon is ready or 30s elapse.
func waitForDocker() {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("docker", "info").Run(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "timed out waiting for Docker daemon")
	os.Exit(1)
}

// buildExtensionImage creates a minimal local Docker image with the overlay label.
// Uses `docker import` from an empty tar to avoid any network access.
func buildExtensionImage(t *testing.T, tag string) {
	t.Helper()
	// Create an empty tarball and import it as an image with the required label.
	cmd := exec.Command("sh", "-c",
		fmt.Sprintf(`tar cv --files-from /dev/null | docker import --change 'LABEL io.balena.image.class=overlay' - %s`, tag))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("buildExtensionImage(%s): %v\n%s", tag, err, out)
	}
}

// runManager executes balena-extension-manager with the given args.
func runManager(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(managerBin, args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix:///var/run/docker.sock")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// dockerExec runs a docker CLI command and returns trimmed stdout.
func dockerExec(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// dockerExecMayFail runs a docker CLI command, returns output and error without failing.
func dockerExecMayFail(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// hostKernelVersion reads /proc/sys/kernel/osrelease and strips suffix after '-'.
// Mirrors runningKernelVersion() in cleanup.go.
func hostKernelVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		t.Fatalf("read kernel version: %v", err)
	}
	release := strings.TrimSpace(string(data))
	if idx := strings.IndexByte(release, '-'); idx > 0 {
		release = release[:idx]
	}
	return release
}

// uniqueName generates a unique name with the given prefix for test isolation.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
