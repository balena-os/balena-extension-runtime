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

// Linked in from the build: the partition label is per-layer when signed.
// Must stay a var; -X cannot reach a const and fails silently.
var bootMount = "/mnt/boot"

// isMounted is a test seam over the mount table read.
var isMounted = mounts.IsMounted

// ErrNotMounted is a boot defect, so callers classify it as retryable.
var ErrNotMounted = errors.New("the boot partition is not mounted")

// Path is the environment block's location on this build.
func Path() string { return filepath.Join(bootMount, "bootenv") }

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

// Update rewrites the block in place under an exclusive lock on the block
// itself, which is the lock os-helpers-bootenv's bootenv_set takes.
func Update(fn func(*Env) error) error {
	mounted, err := isMounted(bootMount)
	if err != nil {
		return fmt.Errorf("checking whether %s is mounted: %w", bootMount, err)
	}
	if !mounted {
		return fmt.Errorf("%w: %s", ErrNotMounted, bootMount)
	}

	path := Path()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	// Under the lock, so the size cannot change.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() < int64(len(signature)) || info.Size() > maxBlockSize {
		return fmt.Errorf("bootenv block %s is %d bytes", path, info.Size())
	}
	block := make([]byte, info.Size())
	if _, err := io.ReadFull(f, block); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	env, err := Parse(block)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := fn(env); err != nil {
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

// Arm opens the validation window for abi.
//
// One write, so no state shows the arm beside a stale count.
func Arm(abi string) error {
	return Update(func(env *Env) error {
		env.Unset(KeyTrial)
		return env.Set(KeyABI, abi)
	})
}
