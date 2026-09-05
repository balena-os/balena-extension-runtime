package bootenv

// The override keys beyond the two this package writes on an arm. The relay
// is written inside a host OS update window and consumed on the next boot;
// upgrade_available is read only, since the update path owns it.
const (
	KeyRejected         = "kernel_override_abi_rejected"
	KeyUpgradeAvailable = "upgrade_available"
)

// Slot names a root filesystem. A and B are the whole namespace, which is
// what lets a rollback derive its target without being told.
type Slot string

const (
	SlotA Slot = "A"
	SlotB Slot = "B"
)

// Other returns the slot a rollback out of s lands in.
func (s Slot) Other() Slot {
	if s == SlotA {
		return SlotB
	}
	return SlotA
}

// KeyCommitted names the override slot has proven healthy.
func KeyCommitted(s Slot) string { return "kernel_override_abi_committed_" + string(s) }

// Test seam over the verdicts' single write, counted to assert atomicity.
var updateBlock = Update

// Forget erases every record naming one of abis: the arm, and either slot's
// committed value. It reports whether the arm was among them.
//
// The erasure spans both slots because the deployment does: container, image
// and published kernel live on the shared data partition, so a committed
// value left in the other slot re-arms on the next rollback into it.
//
// The trial count is cleared only when the arm was: it belongs to the arm,
// and an unconditional clear would let a sweep of another slot's leftover
// hand a pending arm a fresh boot budget, postponing the spent-count
// rejection that is a never-booting override's only exit.
func Forget(abis []string) (armCleared bool, err error) {
	set := abiSet(abis)
	if len(set) == 0 {
		return false, nil
	}
	err = updateBlock(func(env *Env) error {
		armCleared = forgetInto(env, set)
		if armCleared {
			env.Unset(KeyTrial)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return armCleared, nil
}

// ConsumeRelay closes the window a host OS rollback relayed, in one write:
// the records naming abi, the boot budget and the relay itself.
//
// The count is cleared whatever the arm names: this is a window close rather
// than a sweep, and the count belongs to the window.
func ConsumeRelay(abi string) error {
	set := abiSet([]string{abi})
	if len(set) == 0 {
		return nil
	}
	return updateBlock(func(env *Env) error {
		forgetInto(env, set)
		env.Unset(KeyTrial)
		env.Unset(KeyRejected)
		return nil
	})
}

// Commit records abi as slot's proven override and closes the window. The
// callers commit only an arm that took effect, so the arm is the running
// kernel and there is nothing else to record.
//
// It writes nothing when the arm no longer names abi: the window moved to an
// override armed during the trial, and that arm owns its own boot.
func Commit(abi string, slot Slot) (wrote bool, err error) {
	err = updateBlock(func(env *Env) error {
		if arm, _ := env.Get(KeyABI); arm != abi {
			return errNoChange
		}
		wrote = true
		env.Unset(KeyTrial)
		return env.Set(KeyCommitted(slot), abi)
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// Restore points an empty arm back at slot's proven override.
//
// Forget clears an arm without restoring it, since it runs before the slot
// is known, so a sweep of the arm can leave it empty beside a committed value
// that names a different, still deployed override. Left there, the next boot
// runs stock and the slot's proven kernel is put through a full trial again
// once helios redeploys it.
func Restore(slot Slot) (wrote bool, err error) {
	err = updateBlock(func(env *Env) error {
		committed, _ := env.Get(KeyCommitted(slot))
		if arm, _ := env.Get(KeyABI); arm != "" || committed == "" {
			return errNoChange
		}
		wrote = true
		return env.Set(KeyABI, committed)
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// Reject undoes abi in favour of slot's committed override, in one write.
//
// Clearing the arm and restoring the committed value are one write. Split in
// two, a crash between them leaves an empty arm beside a committed value that
// the next boot then retires.
//
// It writes nothing when the arm no longer names abi, for Commit's reason.
func Reject(abi string, slot Slot) (wrote bool, err error) {
	err = updateBlock(func(env *Env) error {
		if arm, _ := env.Get(KeyABI); arm != abi {
			return errNoChange
		}
		wrote = true
		env.Unset(KeyTrial)
		forgetInto(env, abiSet([]string{abi}))
		return restoreArm(env, slot)
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// HUPReject undoes the active override in favour of slot's committed one and
// relays the rejection to the next boot, in one write. slot is the one the
// rollback lands in, not the running one.
//
// A redundant rollback, one whose arm is already the target slot's proven
// override, relays nothing: the next boot would otherwise forget a proven ABI.
//
// The trial count is left for the relay's consumer.
func HUPReject(slot Slot, running string) (relayed bool, err error) {
	err = updateBlock(func(env *Env) error {
		arm, _ := env.Get(KeyABI)
		committed, _ := env.Get(KeyCommitted(slot))
		if arm != "" && arm != committed && arm == running {
			if err := env.Set(KeyRejected, arm); err != nil {
				return err
			}
			relayed = true
		}
		return restoreArm(env, slot)
	})
	if err != nil {
		return false, err
	}
	return relayed, nil
}

func abiSet(abis []string) map[string]struct{} {
	set := make(map[string]struct{}, len(abis))
	for _, abi := range abis {
		if abi != "" {
			set[abi] = struct{}{}
		}
	}
	return set
}

// forgetInto drops the arm and either committed value naming a member of
// set, reporting whether the arm was one of them.
func forgetInto(env *Env, set map[string]struct{}) bool {
	armCleared := false
	if arm, ok := env.Get(KeyABI); ok {
		if _, named := set[arm]; named {
			env.Unset(KeyABI)
			armCleared = true
		}
	}
	for _, slot := range []Slot{SlotA, SlotB} {
		key := KeyCommitted(slot)
		if committed, ok := env.Get(key); ok {
			if _, named := set[committed]; named {
				env.Unset(key)
			}
		}
	}
	return armCleared
}

// restoreArm points the arm at slot's committed override, clearing it when
// the slot has proven none.
func restoreArm(env *Env, slot Slot) error {
	committed, _ := env.Get(KeyCommitted(slot))
	if committed == "" {
		env.Unset(KeyABI)
		return nil
	}
	return env.Set(KeyABI, committed)
}
