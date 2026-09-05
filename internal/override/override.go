// Package override owns the host files a kernel override leaves outside the
// boot environment block: the rejection record, the audit line, the health
// prestate and the published kernel links.
//
// Activation writes some of them and validation writes the rest. Splitting a
// file's reader from its writer across two packages is how a format drifts,
// so the paths and the formats live here and nothing else spells them.
//
// This package classifies nothing. Callers decide whether an error is the
// extension's verdict or a machine condition.
package override

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The host paths, as variables so tests in this and in calling packages can
// redirect them at a temporary tree.
var (
	// Carries the validator's records across a boot.
	StateMount = "/mnt/state"

	// One link per published kernel, read by the initramfs.
	BootByABIDir = "/mnt/data/boot-by-abi"

	// The VPN reachability the validator compares against. openvpn's
	// upscript and rollback-tests both spell it /run.
	VPNActiveMarker = "/run/openvpn/vpn_status/active"
)

// RejectedPath is the record activation refuses an ABI on.
func RejectedPath() string { return filepath.Join(StateMount, "override-rejected") }

// HealthPrestatePath is the baseline rollback-tests reads as
// ROLLBACK_HEALTH_VARIABLES.
func HealthPrestatePath() string { return filepath.Join(StateMount, "extension-health-variables") }

// AuditPath is the operator breadcrumb a rejection leaves behind.
func AuditPath() string { return filepath.Join(StateMount, "override-health-triggered") }

// RejectedABI reports whether validation already rejected this kernel. An
// absent record is empty; an unreadable one is a machine condition.
func RejectedABI(abi string) (bool, error) {
	path := RejectedPath()
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if scanner.Text() == abi {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return false, nil
}

// RecordRejection appends abi unless it is already recorded.
//
// Nothing ever removes a line: the ABI is a checksum of the kernel image, so
// an entry stops applying as soon as the extension ships different bytes.
func RecordRejection(abi string) error {
	if abi == "" {
		return errors.New("refusing to record an empty kernel ABI as rejected")
	}
	listed, err := RejectedABI(abi)
	if err != nil {
		return err
	}
	if listed {
		return nil
	}

	path := RejectedPath()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(abi + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return syncDir(StateMount)
}

// Line is the audit record a rejection overwrites override-health-triggered
// with. Boots accounts for a spent boot budget and is empty otherwise.
//
// by=health means the kernel runs and the device is unwell; by=boots means it
// never held long enough to be judged, and boots is the only account of those
// attempts that survives them, since the stage-2 log dies at kexec -e.
type Line struct {
	By    string
	From  string
	To    string
	Slot  string
	Boots string
}

// String renders the line. Go spells a UTC offset "Z" where date -Iseconds
// spelled it "+00:00"; nothing in the tree parses this file.
func (l Line) String() string {
	to := l.To
	if to == "" {
		to = "none"
	}
	s := fmt.Sprintf("%s: by=%s from=%s to=%s slot=%s",
		time.Now().UTC().Format(time.RFC3339), l.By, l.From, to, l.Slot)
	if l.Boots != "" {
		s += " boots=" + l.Boots
	}
	return s
}

// WriteAuditLine overwrites the record with l. One line, not a log: the file
// carries the last rejection and no history.
func WriteAuditLine(l Line) error {
	path := AuditPath()
	if err := writeFileSynced(path, l.String()+"\n"); err != nil {
		return err
	}
	return syncDir(StateMount)
}

// PublishKernel points boot-by-abi/<abi> at the kernel image itself, so that
// the link resolving is the same question as the kernel being there.
// A republish overwrites, because a retry is a recreate.
func PublishKernel(abi, target string) error {
	link, err := KernelLink(abi)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(BootByABIDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", BootByABIDir, err)
	}
	tmp := link + ".new"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear %s: %w", tmp, err)
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("link %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish %s: %w", link, err)
	}
	// Durable before anything else names the ABI.
	return syncDir(BootByABIDir)
}

// RemoveKernel unpublishes abi. An absent link is the wanted state.
func RemoveKernel(abi string) error {
	link, err := KernelLink(abi)
	if err != nil {
		return err
	}
	if err := os.Remove(link); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove %s: %w", link, err)
	}
	return syncDir(BootByABIDir)
}

// KernelPublished reports whether a kernel is published for abi. A dangling
// link counts: the question is what the record says, not what it resolves to.
func KernelPublished(abi string) (bool, error) {
	link, err := KernelLink(abi)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(link); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", link, err)
	}
	return true, nil
}

// ListPublished names every published kernel, deliberately unfiltered by
// whether the link resolves: a dangling link is what the sweep collects.
func ListPublished() ([]string, error) {
	entries, err := os.ReadDir(BootByABIDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", BootByABIDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// KernelLink is where abi's kernel is published. An ABI names a file, so a
// separator in one read back from the block would escape the directory.
func KernelLink(abi string) (string, error) {
	if abi == "" || abi != filepath.Base(abi) || abi == "." || abi == ".." {
		return "", fmt.Errorf("kernel ABI %q is not a bare file name", abi)
	}
	return filepath.Join(BootByABIDir, abi), nil
}

// WriteHealthPrestate records VPN reachability for the next boot's validator.
//
// Written through a temporary name: the validator removes this file when it
// closes a window, so its presence is what says a window is open. A
// truncating write would leave an open window reading an empty prestate.
func WriteHealthPrestate() error {
	value := "BALENAOS_ROLLBACK_VPNONLINE=0\n"
	if _, err := os.Stat(VPNActiveMarker); err == nil {
		value = "BALENAOS_ROLLBACK_VPNONLINE=1\n"
	}
	path := HealthPrestatePath()
	tmp := path + ".new"
	if err := writeFileSynced(tmp, value); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish %s: %w", path, err)
	}
	// A new name needs its directory entry on disk.
	return syncDir(StateMount)
}

// RemoveHealthPrestate closes the window the prestate's presence stands for.
func RemoveHealthPrestate() error {
	path := HealthPrestatePath()
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return syncDir(StateMount)
}

func writeFileSynced(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}
