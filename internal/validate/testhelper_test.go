package validate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/stretchr/testify/require"
)

const (
	blockSize = 1024
	signature = "# GRUB Environment Block\n"
)

const (
	abiX = "aaaa"
	abiY = "bbbb"
	abiZ = "cccc"
)

// world redirects every host fact the validator reads at a temporary tree and
// records what it did.
type world struct {
	t         *testing.T
	root      string
	blockPath string
	logger    *slog.Logger

	// The claim query and a hook to run inside it, which is how a record
	// landing between the recorded-set read and the query is injected.
	claims    []string
	claimErr  error
	onClaim   func()
	claimCall int

	// One entry per healthcheck attempt, reused once exhausted.
	health      []error
	healthCalls int

	waits    []time.Duration
	rebooted int

	locks    int
	lockHeld bool
}

func newWorld(t *testing.T, entries ...string) *world {
	t.Helper()
	w := &world{
		t:      t,
		root:   t.TempDir(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	boot := filepath.Join(w.root, "mnt", "boot")
	require.NoError(t, os.MkdirAll(boot, 0o755))
	w.blockPath = filepath.Join(boot, "bootenv")
	w.writeBlock(entries...)
	t.Cleanup(bootenv.SetBootMount(boot))

	prevState, prevBoot, prevVPN := override.StateMount, override.BootByABIDir, override.VPNActiveMarker
	override.StateMount = filepath.Join(w.root, "mnt", "state")
	override.BootByABIDir = filepath.Join(w.root, "mnt", "data", "boot-by-abi")
	override.VPNActiveMarker = filepath.Join(w.root, "run", "openvpn", "active")
	require.NoError(t, os.MkdirAll(override.StateMount, 0o755))
	require.NoError(t, os.MkdirAll(override.BootByABIDir, 0o755))
	t.Cleanup(func() {
		override.StateMount, override.BootByABIDir, override.VPNActiveMarker = prevState, prevBoot, prevVPN
	})

	prevMounts, prevActive, prevLabels := procMounts, activeRoot, byLabelDir
	prevCmdline, prevDataRoot := procCmdline, dataRoot
	procMounts = filepath.Join(w.root, "proc-mounts")
	byLabelDir = filepath.Join(w.root, "dev", "disk", "by-label")
	procCmdline = filepath.Join(w.root, "proc-cmdline")
	dataRoot = filepath.Join(w.root, "mnt", "data", "docker")
	t.Cleanup(func() {
		procMounts, activeRoot, byLabelDir = prevMounts, prevActive, prevLabels
		procCmdline, dataRoot = prevCmdline, prevDataRoot
	})
	w.slotIs("resin-rootA")
	w.runningIs("")

	prevClaims, prevHealth, prevWait := claimedABIs, runHealthcheck, wait
	prevReboot, prevLock := reboot, withOperationLock
	claimedABIs = func(string) ([]string, error) {
		w.claimCall++
		if w.onClaim != nil {
			w.onClaim()
		}
		return w.claims, w.claimErr
	}
	runHealthcheck = func(context.Context) error {
		require.False(t, w.lockHeld, "the healthcheck must not hold the operation lock")
		err := w.health[min(w.healthCalls, len(w.health)-1)]
		w.healthCalls++
		return err
	}
	wait = func(_ context.Context, d time.Duration) error {
		require.False(t, w.lockHeld, "the waits must not hold the operation lock")
		w.waits = append(w.waits, d)
		return nil
	}
	reboot = func() error { w.rebooted++; return nil }
	withOperationLock = func(_ context.Context, fn func() error) error {
		require.False(t, w.lockHeld, "the operation lock is not reentrant")
		w.locks++
		w.lockHeld = true
		defer func() { w.lockHeld = false }()
		return fn()
	}
	t.Cleanup(func() {
		claimedABIs, runHealthcheck, wait = prevClaims, prevHealth, prevWait
		reboot, withOperationLock = prevReboot, prevLock
	})

	w.health = []error{nil}
	return w
}

// writeBlock lays down a block holding exactly these lines.
func (w *world) writeBlock(entries ...string) {
	w.t.Helper()
	var b strings.Builder
	b.WriteString(signature)
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	require.Less(w.t, b.Len(), blockSize)

	block := make([]byte, blockSize)
	copy(block, b.String())
	for i := b.Len(); i < blockSize; i++ {
		block[i] = '#'
	}
	require.NoError(w.t, os.WriteFile(w.blockPath, block, 0o644))
}

// slotIs points the running root filesystem at a partition carrying label.
func (w *world) slotIs(label string) {
	w.t.Helper()
	device := filepath.Join(w.root, "dev", "sda2")
	require.NoError(w.t, os.MkdirAll(filepath.Dir(device), 0o755))
	require.NoError(w.t, os.WriteFile(device, nil, 0o644))
	w.mountedAs(device, device)
	w.labels(map[string]string{label: device})
}

// mountedAs writes a mount table naming source as the running root, plus the
// unrelated entries a device carries.
func (w *world) mountedAs(sources ...string) {
	w.t.Helper()
	var b strings.Builder
	b.WriteString("proc /proc proc rw 0 0\n")
	for _, source := range sources {
		b.WriteString(source + " " + activeRoot + " ext4 ro 0 0\n")
	}
	require.NoError(w.t, os.WriteFile(procMounts, []byte(b.String()), 0o644))
}

// labels publishes the by-label links udev would have created.
func (w *world) labels(links map[string]string) {
	w.t.Helper()
	require.NoError(w.t, os.RemoveAll(byLabelDir))
	require.NoError(w.t, os.MkdirAll(byLabelDir, 0o755))
	for label, target := range links {
		require.NoError(w.t, os.Symlink(target, filepath.Join(byLabelDir, label)))
	}
}

func (w *world) runningIs(abi string) {
	w.t.Helper()
	cmdline := "console=tty1 root=/dev/sda2"
	if abi != "" {
		cmdline += " balena_kernel_abi=" + abi
	}
	require.NoError(w.t, os.WriteFile(procCmdline, []byte(cmdline+"\n"), 0o644))
}

func (w *world) publish(abis ...string) {
	w.t.Helper()
	for _, abi := range abis {
		require.NoError(w.t, override.PublishKernel(abi, "../docker/volumes/"+abi+"/_data/Image"))
	}
}

// block reads the environment block back as a map.
func (w *world) block() map[string]string {
	w.t.Helper()
	data, err := os.ReadFile(w.blockPath)
	require.NoError(w.t, err)
	require.Len(w.t, data, blockSize, "a write must not resize the block")

	env, err := bootenv.Parse(data)
	require.NoError(w.t, err)
	out := map[string]string{}
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		out[k] = v
	}
	return out
}

func (w *world) published() []string {
	w.t.Helper()
	names, err := override.ListPublished()
	require.NoError(w.t, err)
	return names
}

func (w *world) rejected() string {
	w.t.Helper()
	data, err := os.ReadFile(override.RejectedPath())
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(w.t, err)
	return string(data)
}

func (w *world) audit() string {
	w.t.Helper()
	data, err := os.ReadFile(override.AuditPath())
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(w.t, err)
	_, line, _ := strings.Cut(strings.TrimSuffix(string(data), "\n"), ": ")
	return line
}

// checkFailed is a healthcheck that ran and said no: a real *exec.ExitError,
// the only error the validator reads as a verdict.
func checkFailed(t *testing.T) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "exit 1").Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	return exit
}

func (w *world) writePrestate() {
	w.t.Helper()
	require.NoError(w.t, override.WriteHealthPrestate())
}

func (w *world) prestateExists() bool {
	w.t.Helper()
	_, err := os.Stat(override.HealthPrestatePath())
	return err == nil
}

func (w *world) run() error {
	w.t.Helper()
	return Run(context.Background(), w.logger, Options{
		Settle: time.Minute, Retry: time.Minute, Attempts: 15,
	})
}
