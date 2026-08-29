package manager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOperationLock_SerializesConcurrentCallers proves the lock, not just
// exercises it.
func TestOperationLock_SerializesConcurrentCallers(t *testing.T) {
	stub := newEngineStub()
	stub.Containers = []Container{} // empty list keeps every call past ListContainers quickly

	var inFlight, maxObserved int32
	base := stub.handler()
	delayed := func(method, path string, body []byte) (int, []byte) {
		n := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)
		for {
			old := atomic.LoadInt32(&maxObserved)
			if n <= old || atomic.CompareAndSwapInt32(&maxObserved, old, n) {
				break
			}
		}
		// Long enough that eight goroutines firing at once would need real
		// overlap to finish in anything close to one sleep's worth of time.
		time.Sleep(30 * time.Millisecond)
		return base(method, path, body)
	}

	testEngineEnv(t, testServer(t, delayed))

	const runs = 8
	var wg sync.WaitGroup
	start := time.Now()
	for range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Cleanup(context.Background(), quietLogger(), CleanupOpts{})
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	assert.Equal(t, int32(1), atomic.LoadInt32(&maxObserved),
		"the operation lock must keep two callers' engine calls from overlapping")
	// Belt and braces on the same property from the outside: fully
	// serialized, runs requests can't finish faster than runs sleeps.
	assert.GreaterOrEqual(t, elapsed, time.Duration(runs)*30*time.Millisecond,
		"serialized callers must not complete faster than one sleep each")
}

const deadID = "cafe000000000000"

// deadContainerStub wires an engine serving one dead extension container,
// which cleanup's unconditional dead sweep removes. It gives the lock tests an
// operation with a visible effect, so a call that only queued can be told from
// one that ran.
func deadContainerStub(t *testing.T) *engineStub {
	t.Helper()
	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:      deadID,
		State:   "dead",
		ImageID: "sha256:" + deadID,
		Labels:  overlayLabels(nil),
	}}
	testEngineEnv(t, testServer(t, stub.handler()))
	return stub
}

// holdFileLock takes the flock on lockPath through a descriptor of its own and
// returns a release function. flock ownership belongs to the open file
// description rather than the process, so a second descriptor conflicts with
// the operation lock exactly as a second process would, which is what makes
// this stand in for the CLI-against-daemon case without spawning anything.
func holdFileLock(t *testing.T) func() {
	t.Helper()
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	var once sync.Once
	release := func() {
		once.Do(func() {
			assert.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
			f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// TestOperationLock_WaitsForAnotherHolderOfTheFileLock covers what the
// in-process mutex cannot: the boot cleanup unit and the rollback shell's
// removal run in separate processes, so only the file lock keeps them apart.
func TestOperationLock_WaitsForAnotherHolderOfTheFileLock(t *testing.T) {
	stub := deadContainerStub(t)
	release := holdFileLock(t)

	done := make(chan error, 1)
	go func() {
		done <- Cleanup(context.Background(), quietLogger(), CleanupOpts{})
	}()

	select {
	case err := <-done:
		t.Fatalf("the operation ran while another descriptor held the file lock (err=%v)", err)
	case <-time.After(10 * lockPollInterval):
	}
	assert.Empty(t, stub.removedContainersSnapshot(),
		"a waiting operation must not have removed anything")

	release()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the operation did not proceed once the file lock was released")
	}
	assert.Equal(t, []string{deadID}, stub.removedContainersSnapshot(),
		"the operation must run to completion once it holds the lock")
}

// TestOperationLock_CancelledWhileWaitingDoesNotRun pins the property a
// blocking LOCK_EX would not give: a caller whose context ends while it is
// queued gives up and, crucially, never performs the operation. A caller that
// stopped waiting must not still remove an extension minutes later.
func TestOperationLock_CancelledWhileWaitingDoesNotRun(t *testing.T) {
	stub := deadContainerStub(t)
	holdFileLock(t)

	// The deadline outlives at least one poll, so the operation is cancelled
	// mid-wait rather than before it ever reaches the lock.
	ctx, cancel := context.WithTimeout(context.Background(), 2*lockPollInterval)
	defer cancel()

	start := time.Now()
	err := Cleanup(ctx, quietLogger(), CleanupOpts{})

	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a caller that gives up waiting must report why, not the engine's view of the world")
	assert.Less(t, time.Since(start), 5*time.Second,
		"the wait must end with the context rather than outlive it")
	assert.Empty(t, stub.removedContainersSnapshot(),
		"a cancelled caller must not perform the operation")
}

// TestOperationLock_DoesNotRunWithADoneContext pins the ctx re-check
// directly rather than through an exported operation. Neither acquisition
// rejects a done context on its own: the select has two ready cases when the
// lock is free, and a non-blocking flock on a free lock succeeds without
// consulting ctx. Going through an exported operation would prove nothing,
// because its first engine call fails on a cancelled context anyway. That is exactly the
// incidental behaviour this guards against: the property must not depend on
// every implementation short-circuiting on its own.
func TestOperationLock_DoesNotRunWithADoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Repeated because the select over a ready ctx and a free lock picks
	// pseudo-randomly, so one pass would miss the defect about half the time.
	ran := 0
	for range 50 {
		err := WithOperationLock(ctx, func() error {
			ran++
			return nil
		})
		require.ErrorIs(t, err, context.Canceled,
			"a done context must be reported, not swallowed")
	}
	assert.Zero(t, ran, "a done context must not reach the operation")
}

// TestOperationLock_NoSelfDeadlock is a regression guard, not a concurrency
// test.
func TestOperationLock_NoSelfDeadlock(t *testing.T) {
	const (
		id  = "abiclaimant0000"
		abi = "abi1"
	)

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:      id,
		State:   "exited",
		ImageID: "sha256:" + id,
		Labels: overlayLabels(map[string]string{
			"io.balena.image.kernel-abi-id": abi,
			// Unsatisfiable against any real VERSION_ID, so the stale-OS
			// pass reaches the removal.
			"io.balena.image.os-version": "9.9.*",
		}),
	}}
	stub.Inspects[id] = inspectJSON(id, "exited", "", 0)
	testEngineEnv(t, testServer(t, stub.handler()))

	osr := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(osr, []byte("VERSION_ID=\"2.119.0\"\n"), 0o644))
	prev := osReleasePath
	osReleasePath = osr
	t.Cleanup(func() { osReleasePath = prev })

	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = Cleanup(context.Background(), quietLogger(), CleanupOpts{PruneStaleOS: true})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an exported operation deadlocked on its own lock")
	}

	assert.NoError(t, err, "the operation must have completed, not failed early")
	assert.Equal(t, []string{id}, stub.removedContainersSnapshot(),
		"the operation must have reached the removal")
}
