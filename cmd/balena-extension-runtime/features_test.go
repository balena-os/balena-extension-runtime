package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-spec/specs-go/features"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The engine probes this subcommand at start. A dropped field reads as
// "unknown" instead of "unsupported", so every bool must survive marshalling.
func TestFeaturesCmdReportsEverySubsystemAsFalse(t *testing.T) {
	doc := runFeatures(t)

	assert.Equal(t, "1.0.0", doc.OCIVersionMin)
	assert.Equal(t, specs.Version, doc.OCIVersionMax)

	require.NotNil(t, doc.Linux)
	linux := doc.Linux

	require.NotNil(t, linux.Cgroup)
	assertFalse(t, linux.Cgroup.V1, "cgroup.v1")
	assertFalse(t, linux.Cgroup.V2, "cgroup.v2")
	assertFalse(t, linux.Cgroup.Systemd, "cgroup.systemd")
	assertFalse(t, linux.Cgroup.SystemdUser, "cgroup.systemdUser")
	assertFalse(t, linux.Cgroup.Rdma, "cgroup.rdma")

	require.NotNil(t, linux.Seccomp)
	assertFalse(t, linux.Seccomp.Enabled, "seccomp.enabled")
	require.NotNil(t, linux.Apparmor)
	assertFalse(t, linux.Apparmor.Enabled, "apparmor.enabled")
	require.NotNil(t, linux.Selinux)
	assertFalse(t, linux.Selinux.Enabled, "selinux.enabled")
	require.NotNil(t, linux.IntelRdt)
	assertFalse(t, linux.IntelRdt.Enabled, "intelRdt.enabled")

	require.NotNil(t, linux.MountExtensions)
	require.NotNil(t, linux.MountExtensions.IDMap)
	assertFalse(t, linux.MountExtensions.IDMap.Enabled, "mountExtensions.idmap.enabled")

	assert.Nil(t, linux.Namespaces)
	assert.Nil(t, linux.Capabilities)
	assert.Nil(t, doc.Hooks)
	assert.Nil(t, doc.MountOptions)

	assert.NotEmpty(t, doc.Annotations["io.balena.extension-runtime.version"])
	assert.NotEmpty(t, doc.Annotations["io.balena.extension-runtime.commit"])

	// config.md reserves the org.opencontainers namespace for the spec.
	for key := range doc.Annotations {
		assert.False(t, strings.HasPrefix(key, "org.opencontainers."),
			"%s is in the reserved namespace", key)
	}
}

func TestFeaturesCmdRejectsArguments(t *testing.T) {
	assert.Error(t, featuresCmd.Args(featuresCmd, []string{"extra"}))
}

func assertFalse(t *testing.T, got *bool, field string) {
	t.Helper()
	if assert.NotNil(t, got, "%s must be present, nil reads as unknown", field) {
		assert.False(t, *got, field)
	}
}

// runFeatures executes the command through rootCmd and returns the parsed
// stdout. fmt.Println writes to os.Stdout, so the pipe replaces it.
func runFeatures(t *testing.T) features.Features {
	t.Helper()

	// rootCmd is package-level state shared with the other tests.
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	rootCmd.SetArgs([]string{"features"})

	read, write, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = saved }()

	execErr := rootCmd.Execute()
	require.NoError(t, write.Close())
	output, readErr := io.ReadAll(read)
	require.NoError(t, read.Close())

	require.NoError(t, execErr)
	require.NoError(t, readErr)

	var doc features.Features
	require.NoError(t, json.Unmarshal(output, &doc))
	return doc
}
