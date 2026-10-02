// Package bootenv reads and writes the grub environment block, which is where
// the kernel override state crosses a boot.
//
// A block opens with a signature line, then key=value lines, padded with '#'.
// The format fixes no size: a block keeps the one it was created at. Padding
// is what grub-editenv writes into, so a rendered block always keeps some.
//
// Comment lines other than the signature carry no state and are not carried
// over. Nothing that creates a block on a device writes one.
//
// Writes go in place, under an exclusive flock on the block itself. That is
// the lock os-helpers-bootenv takes, so it only serialises writers that go
// through it.
//
// Grub escapes backslashes and newlines in values; this package does not.
// Every value the OS keeps here is a hex ABI, a small count, or empty.
package bootenv

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/mounts"
)

const (
	// grub-editenv's DEFAULT_ENVBLK_SIZE. Only what it creates at.
	defaultBlockSize = 1024

	// Bounds what a read pulls into memory.
	maxBlockSize = 64 * 1024

	signature = "# GRUB Environment Block\n"

	pad = '#'
)

// Keys this package owns. Every other key is carried through untouched.
const (
	KeyABI   = "kernel_override_abi"
	KeyTrial = "kernel_override_trial"
)

const blockName = "bootenv"

// The boot partitions, by label, linked in from the build. The block is on
// the non-encrypted one when the device has it. Must stay vars. -X cannot
// reach a const and fails silently.
var (
	nonencBootLabel = "balena-boot"
	encBootLabel    = "resin-boot"
)

// byStateDir is a test seam. udev links every balena partition on the root
// drive there by label.
var byStateDir = "/dev/disk/by-state"

// Test seams over the two host probes the resolver makes.
var (
	partition    = resolvePartition
	mountpointOf = mounts.MountpointOf
)

// errNoPartition is a label no partition on the device carries.
var errNoPartition = errors.New("no partition carries the label")

// resolvePartition follows the by-state link to the device. A link that
// exists and does not resolve is an error, not an absent partition: skipping
// it would read the other partition's block.
func resolvePartition(label string) (string, error) {
	link := filepath.Join(byStateDir, label)
	if _, err := os.Lstat(link); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", errNoPartition, link)
		}
		return "", fmt.Errorf("stat %s: %w", link, err)
	}
	dev, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", link, err)
	}
	return dev, nil
}

// ErrNotMounted is a boot defect, so callers classify it as retryable.
var ErrNotMounted = errors.New("the boot partition is not mounted")

// ErrNoBlock separates a device with no override axis from an unreadable
// block. A u-boot device keeps its bootloader environment elsewhere.
var ErrNoBlock = errors.New("no bootenv block on this device")

// layout is the device as the resolver probes it: which labels name a
// partition, and where each partition is mounted.
type layout struct {
	partitions map[string]string
	mounts     map[string]string
}

// setLayout fakes both probes and returns a restore. Do not fake one probe
// alone.
func setLayout(l layout) func() {
	prevPartition, prevMountpoint := partition, mountpointOf
	partition = func(label string) (string, error) {
		dev, ok := l.partitions[label]
		if !ok {
			return "", fmt.Errorf("%w: %s", errNoPartition, label)
		}
		return dev, nil
	}
	mountpointOf = func(dev string) (string, error) { return l.mounts[dev], nil }
	return func() { partition, mountpointOf = prevPartition, prevMountpoint }
}

// SetBootMount points the package at a device with no non-encrypted boot
// partition, whose boot partition is mounted at path, and returns a restore.
// The build sets the device's labels through -X, so this is how a test in
// another package reaches a block it can write.
func SetBootMount(path string) func() {
	return setLayout(layout{
		partitions: map[string]string{encBootLabel: "boot"},
		mounts:     map[string]string{"boot": path},
	})
}

// Env is a block's entries, in block order, as grub keeps them.
type Env struct {
	// Parsed length. A write must not resize the file.
	size   int
	keys   []string
	values map[string]string
}

// Keys returns the entry names in block order.
func (e *Env) Keys() []string { return append([]string(nil), e.keys...) }

// Get reports the value and whether the key is present at all.
// rollback-health writes an empty value to mean "good on stock".
func (e *Env) Get(key string) (string, bool) {
	v, ok := e.values[key]
	return v, ok
}

// Set adds or overwrites a key, keeping an existing key in its place.
func (e *Env) Set(key, value string) error {
	switch {
	case key == "":
		return errors.New("a bootenv key cannot be empty")
	case strings.ContainsAny(key, "=\n"):
		return fmt.Errorf("bootenv key %q carries a separator the block cannot escape", key)
	case strings.HasPrefix(key, string(pad)):
		return fmt.Errorf("bootenv key %q would render as a comment", key)
	case strings.Contains(value, "\n"):
		return fmt.Errorf("bootenv value for %q carries a newline the block cannot escape", key)
	}
	if _, ok := e.values[key]; !ok {
		e.keys = append(e.keys, key)
	}
	e.values[key] = value
	return nil
}

// Unset removes a key. Callers cannot predict whether it is there.
func (e *Env) Unset(key string) {
	if _, ok := e.values[key]; !ok {
		return
	}
	delete(e.values, key)
	for i, k := range e.keys {
		if k == key {
			e.keys = append(e.keys[:i], e.keys[i+1:]...)
			return
		}
	}
}

// Parse reads a block. Comment lines carry no entries and are regenerated.
func Parse(block []byte) (*Env, error) {
	if len(block) < len(signature) || len(block) > maxBlockSize {
		return nil, fmt.Errorf("bootenv block is %d bytes", len(block))
	}
	if !strings.HasPrefix(string(block), signature) {
		return nil, errors.New("bootenv block does not open with the grub signature")
	}

	env := &Env{size: len(block), values: map[string]string{}}
	for _, line := range strings.Split(string(block[len(signature):]), "\n") {
		if line == "" || line[0] == pad {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("bootenv line %q carries no value", line)
		}
		if _, dup := env.values[key]; !dup {
			env.keys = append(env.keys, key)
		}
		env.values[key] = value
	}
	return env, nil
}

// Marshal renders the block. Overflow errors rather than truncating.
//
// One pad byte is reserved: grub-editenv finds its free space by scanning
// back over trailing '#', and a block with none rejects every later write.
func (e *Env) Marshal() ([]byte, error) {
	var b strings.Builder
	b.WriteString(signature)
	for _, k := range e.keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(e.values[k])
		b.WriteByte('\n')
	}
	if b.Len() >= e.size {
		return nil, fmt.Errorf("bootenv entries need %d bytes plus padding, the block holds %d", b.Len(), e.size)
	}
	out := make([]byte, e.size)
	copy(out, b.String())
	for i := b.Len(); i < e.size; i++ {
		out[i] = pad
	}
	return out, nil
}

// blockMount returns the mount of the partition that holds the block. The
// flasher creates the non-encrypted boot partition only when it splits the
// boot partition under secure boot, and the block lives there. The initramfs
// cryptsetup scripts and the non-EFI mount unit use the same by-state test.
// Every other device keeps the block on the boot partition.
//
// An unmounted block partition is ErrNotMounted. The resolver never reads the
// other partition.
func blockMount() (string, error) {
	for _, label := range []string{nonencBootLabel, encBootLabel} {
		dev, err := partition(label)
		if errors.Is(err, errNoPartition) {
			continue
		}
		if err != nil {
			return "", err
		}
		mount, err := mountpointOf(dev)
		if err != nil {
			return "", err
		}
		if mount == "" {
			return "", fmt.Errorf("%w: %s (%s)", ErrNotMounted, label, dev)
		}
		return mount, nil
	}
	// udev has not linked the drive yet, or the OS and this binary disagree.
	return "", fmt.Errorf("%w: no partition under %s is labelled %s or %s",
		ErrNotMounted, byStateDir, nonencBootLabel, encBootLabel)
}

// resolveBlockPath returns the block on this device. A mounted partition with
// no block is a device with no override axis, which is not a defect.
func resolveBlockPath() (string, error) {
	mount, err := blockMount()
	if err != nil {
		return "", err
	}
	path := filepath.Join(mount, blockName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNoBlock, path)
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	return path, nil
}

// openBlock opens the block under the given flock mode.
func openBlock(flag, how int) (*os.File, error) {
	path, err := resolveBlockPath()
	if err != nil {
		return nil, err
	}

	// The block can vanish after the stat.
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoBlock, path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// readBlock parses the block behind an already locked descriptor.
func readBlock(f *os.File) (*Env, error) {
	path := f.Name()
	// Under the lock, so the size cannot change.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() < int64(len(signature)) || info.Size() > maxBlockSize {
		return nil, fmt.Errorf("bootenv block %s is %d bytes", path, info.Size())
	}
	block := make([]byte, info.Size())
	if _, err := io.ReadFull(f, block); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	env, err := Parse(block)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return env, nil
}

// Read parses the block under a shared lock, writing nothing. An ordinary
// boot reaches the block through this and never opens it for writing.
func Read() (*Env, error) {
	f, err := openBlock(os.O_RDONLY, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return readBlock(f)
}

// errNoChange, returned by an Update closure, leaves the block untouched.
// A verdict whose window moved must not rewrite even identical bytes.
var errNoChange = errors.New("bootenv unchanged")

// Update rewrites the block in place under an exclusive lock on the block
// itself, which is the lock os-helpers-bootenv's bootenv_set takes.
func Update(fn func(*Env) error) error {
	f, err := openBlock(os.O_RDWR, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	path := f.Name()
	env, err := readBlock(f)
	if err != nil {
		return err
	}
	if err := fn(env); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	out, err := env.Marshal()
	if err != nil {
		return fmt.Errorf("render %s: %w", path, err)
	}

	// In place, so no directory entry changes to sync.
	if _, err := f.WriteAt(out, 0); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// validABI reports whether abi is a kernel ABI id: the lowercase hex sha256
// of a kernel image. Stage 2 boots stock for anything else, because the arm
// becomes a path and a kernel command line argument there.
func validABI(abi string) bool {
	if len(abi) != sha256.Size*2 {
		return false
	}
	for _, c := range abi {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Arm opens the validation window for abi.
//
// One write, so no state shows the arm beside a stale count.
func Arm(abi string) error {
	if !validABI(abi) {
		return fmt.Errorf("kernel ABI %q is not a lowercase hex sha256", abi)
	}
	return Update(func(env *Env) error {
		// Only a different kernel earns a fresh trial budget.
		if current, _ := env.Get(KeyABI); current != abi {
			env.Unset(KeyTrial)
		}
		return env.Set(KeyABI, abi)
	})
}
