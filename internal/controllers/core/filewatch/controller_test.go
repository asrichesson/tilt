package filewatch

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/tilt-dev/tilt/internal/controllers/core/filewatch/fsevent"
	"github.com/tilt-dev/tilt/internal/controllers/fake"
	"github.com/tilt-dev/tilt/internal/controllers/indexer"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/testutils/tempdir"
	"github.com/tilt-dev/tilt/internal/watch"
	"github.com/tilt-dev/tilt/pkg/apis"
	filewatches "github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"
)

// Test constants
const timeout = time.Second
const interval = 5 * time.Millisecond

type testStore struct {
	*store.TestingStore
	out io.Writer
}

func NewTestingStore(out io.Writer) *testStore {
	return &testStore{
		TestingStore: store.NewTestingStore(),
		out:          out,
	}
}

func (s *testStore) Dispatch(action store.Action) {
	s.TestingStore.Dispatch(action)
	switch action := action.(type) {
	case store.LogAction:
		_, _ = s.out.Write(action.Message())
	}
}

type fixture struct {
	*fake.ControllerFixture
	t                testing.TB
	tmpdir           *tempdir.TempDirFixture
	controller       *Controller
	store            *testStore
	fakeMultiWatcher *fsevent.FakeMultiWatcher
	fakeTimerMaker   fsevent.FakeTimerMaker
	clock            clockwork.FakeClock
}

func newFixture(t *testing.T) *fixture {
	tmpdir := tempdir.NewTempDirFixture(t)
	t.Cleanup(tmpdir.TearDown)
	tmpdir.Chdir()

	timerMaker := fsevent.MakeFakeTimerMaker(t)
	fakeMultiWatcher := fsevent.NewFakeMultiWatcher()

	cfb := fake.NewControllerFixtureBuilder(t)
	testingStore := NewTestingStore(cfb.OutWriter())
	clock := clockwork.NewFakeClock()
	controller := NewController(cfb.Client, testingStore, fakeMultiWatcher.NewSub, timerMaker.Maker(), filewatches.NewScheme(), clock)

	indexer.StartRequeuerForTesting(cfb.Context(), controller.requeuer, controller)

	return &fixture{
		ControllerFixture: cfb.Build(controller),
		t:                 t,
		tmpdir:            tmpdir,
		controller:        controller,
		store:             testingStore,
		fakeMultiWatcher:  fakeMultiWatcher,
		fakeTimerMaker:    timerMaker,
		clock:             clock,
	}
}

func (f *fixture) ChangeAndWaitForSeenFile(key types.NamespacedName, pathElems ...string) {
	f.t.Helper()
	f.ChangeFile(pathElems...)
	f.WaitForSeenFile(key, pathElems...)
}

func (f *fixture) ChangeFile(elem ...string) {
	f.t.Helper()
	path, err := filepath.Abs(f.tmpdir.JoinPath(elem...))
	require.NoErrorf(f.t, err, "Could not get abs path for %q", path)
	select {
	case f.fakeMultiWatcher.Events <- watch.NewFileEvent(path):
	default:
		f.t.Fatal("emitting a FileEvent would block. Perhaps there are too many events or the buffer size is too small.")
	}
}

func (f *fixture) WaitForSeenFile(key types.NamespacedName, pathElems ...string) {
	f.t.Helper()
	relPath := filepath.Join(pathElems...)
	var seenPaths []string
	require.Eventuallyf(f.t, func() bool {
		seenPaths = nil
		var fw filewatches.FileWatch
		if !f.Get(key, &fw) {
			return false
		}
		found := false
		for _, e := range fw.Status.FileEvents {
			for _, p := range e.SeenFiles {
				// relativize all the paths before comparison/storage
				// (this makes the test output way more comprehensible on failure by hiding all the tmpdir cruft)
				p, _ = filepath.Rel(f.tmpdir.Path(), p)
				if p == relPath {
					found = true
				}
				seenPaths = append(seenPaths, p)
			}
		}
		return found
	}, 2*time.Second, 20*time.Millisecond, "Did not find path %q, seen: %v", relPath, &seenPaths)
}

func (f *fixture) CreateSimpleFileWatch() (types.NamespacedName, *filewatches.FileWatch) {
	f.t.Helper()
	fw := &filewatches.FileWatch{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: apis.SanitizeName(f.t.Name()),
			Name:      "test-file-watch",
		},
		Spec: filewatches.FileWatchSpec{
			WatchedPaths: []string{f.tmpdir.JoinPath("a"), f.tmpdir.JoinPath("b", "c")},
			DisableSource: &filewatches.DisableSource{
				ConfigMap: &filewatches.ConfigMapDisableSource{
					Name: "disable-test-file-watch",
					Key:  "isDisabled",
				},
			},
		},
	}
	f.Create(fw)
	return f.KeyForObject(fw), fw
}

func (f *fixture) reconcileFw(key types.NamespacedName) {
	_, err := f.controller.Reconcile(f.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(f.T(), err)
}

func (f *fixture) setDisabled(key types.NamespacedName, isDisabled bool) {
	fw := &filewatches.FileWatch{}
	err := f.Client.Get(f.Context(), key, fw)
	require.NoError(f.T(), err)

	// Make sure that there's a `DisableSource` set on fw
	require.NotNil(f.T(), fw.Spec.DisableSource)
	require.NotNil(f.T(), fw.Spec.DisableSource.ConfigMap)

	// Make sure that the configmap exists
	configmap := &filewatches.ConfigMap{}
	err = f.Client.Get(f.Context(), types.NamespacedName{Name: fw.Spec.DisableSource.ConfigMap.Name}, configmap)
	// If the configmap doesn't exist, create it
	if apierrors.IsNotFound(err) {
		configmap.ObjectMeta.Name = fw.Spec.DisableSource.ConfigMap.Name
		configmap.Data = map[string]string{fw.Spec.DisableSource.ConfigMap.Key: strconv.FormatBool(isDisabled)}
		err = f.Client.Create(f.Context(), configmap)
		require.NoError(f.T(), err)
	} else {
		// Otherwise, update the existing configmap
		require.Nil(f.T(), err)
		configmap.Data[fw.Spec.DisableSource.ConfigMap.Key] = strconv.FormatBool(isDisabled)
		err = f.Client.Update(f.Context(), configmap)
		require.NoError(f.T(), err)
	}

	f.reconcileFw(key)

	require.Eventually(f.T(), func() bool {
		err := f.Client.Get(f.Context(), key, fw)
		require.NoError(f.T(), err)

		return fw.Status.DisableStatus != nil && fw.Status.DisableStatus.Disabled == isDisabled
	}, timeout, interval)
}

func TestController_LimitFileEventsHistory(t *testing.T) {
	f := newFixture(t)

	key, fw := f.CreateSimpleFileWatch()

	const eventOverflowCount = 5
	for i := 0; i < MaxFileEventHistory+eventOverflowCount; i++ {
		// need to wait for each file 1-by-1 to prevent batching
		f.ChangeAndWaitForSeenFile(key, "a", strconv.Itoa(i))
	}

	f.MustGet(key, fw)
	require.Equal(t, MaxFileEventHistory, len(fw.Status.FileEvents), "Wrong number of file events")
	for i := 0; i < len(fw.Status.FileEvents); i++ {
		p := f.tmpdir.JoinPath("a", strconv.Itoa(i+eventOverflowCount))
		assert.Contains(t, fw.Status.FileEvents[i].SeenFiles, p)
	}
}

func TestController_ShortRead(t *testing.T) {
	f := newFixture(t)
	key, _ := f.CreateSimpleFileWatch()

	f.fakeMultiWatcher.Errors <- fmt.Errorf("short read on readEvents()")

	require.Eventuallyf(t, func() bool {
		return strings.Contains(f.Stdout(), "short read")
	}, time.Second, 10*time.Millisecond, "short read error was not propagated")

	if runtime.GOOS == "windows" {
		assert.Contains(t, f.Stdout(), "https://github.com/tilt-dev/tilt/issues/3556")
	}

	var fw filewatches.FileWatch
	f.MustGet(key, &fw)
	assert.Contains(t, fw.Status.Error, "short read on readEvents()")
}

func TestController_IgnoreEphemeralFiles(t *testing.T) {
	f := newFixture(t)
	key, orig := f.CreateSimpleFileWatch()
	// spec should have no ignores - these are purely implicit ignores
	require.Empty(t, orig.Spec.Ignores)

	// sandwich in some ignored files with seen files on the outside as synchronization
	f.ChangeAndWaitForSeenFile(key, "a", "start")
	// see internal/ignore/ephemeral.go for where these come from - they're NOT part of a FileWatch spec
	// but are always included at the filesystem watcher level by Tilt
	f.ChangeFile("a", ".idea", "workspace.xml")
	f.ChangeFile("b", "c", ".vim.swp")
	f.ChangeAndWaitForSeenFile(key, "b", "c", "stop")

	var fw filewatches.FileWatch
	f.MustGet(key, &fw)
	require.Equal(t, 2, len(fw.Status.FileEvents), "Wrong file event count")
	assert.Equal(t, []string{f.tmpdir.JoinPath("a", "start")}, fw.Status.FileEvents[0].SeenFiles)
	assert.Equal(t, []string{f.tmpdir.JoinPath("b", "c", "stop")}, fw.Status.FileEvents[1].SeenFiles)
}

// TestController_Watcher_Cancel peeks into internal/unexported portions of the controller to inspect the actual
// filesystem monitor so it can ensure reconciler is not leaking resources; other tests should prefer observing
// desired state!
func TestController_Watcher_Cancel(t *testing.T) {
	f := newFixture(t)
	key, _ := f.CreateSimpleFileWatch()

	assert.Equalf(t, 1, len(f.controller.targetWatches), "There should be exactly one file watcher")
	watcher := f.controller.targetWatches[key]
	require.NotNilf(t, watcher, "Watcher does not exist for %q", key.String())

	// cancel the root context, which should propagate to the watcher
	f.Cancel()

	require.Eventuallyf(t, func() bool {
		watcher.mu.Lock()
		defer watcher.mu.Unlock()
		return watcher.done
	}, time.Second, 10*time.Millisecond, "Watcher was never cleaned up")
}

func TestController_Reconcile_Create(t *testing.T) {
	f := newFixture(t)
	key, fw := f.CreateSimpleFileWatch()

	f.MustGet(key, fw)
	assert.NotZero(t, fw.Status.MonitorStartTime, "Filesystem monitor was not started")
}

// TestController_Reconcile_Delete peeks into internal/unexported portions of the controller to inspect the actual
// filesystem monitor so it can ensure reconciler is not leaking resources; other tests should prefer observing
// desired state!
func TestController_Reconcile_Delete(t *testing.T) {
	f := newFixture(t)
	key, fw := f.CreateSimpleFileWatch()

	assert.Equalf(t, 1, len(f.controller.targetWatches), "There should be exactly one file watcher")
	watcher := f.controller.targetWatches[key]
	require.NotNilf(t, watcher, "Watcher does not exist for %q", key.String())

	deleted, _ := f.Delete(fw)
	require.True(t, deleted, "FileWatch was not deleted")

	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	require.True(t, watcher.done, "Watcher was not stopped")
	require.Empty(t, f.controller.targetWatches, "There should not be any remaining file watchers")
}

func TestController_Reconcile_Watches(t *testing.T) {
	f := newFixture(t)
	key, fw := f.CreateSimpleFileWatch()

	f.ChangeAndWaitForSeenFile(key, "a", "1")

	f.MustGet(key, fw)
	originalStart := fw.Status.MonitorStartTime.Time
	assert.NotZero(t, originalStart, "Filesystem monitor was not started")

	fw.Spec.Ignores = []filewatches.IgnoreDef{
		{
			BasePath: f.tmpdir.Path(),
			Patterns: []string{"**/ignore_me"},
		},
		{
			// no patterns means ignore the path recursively
			BasePath: f.tmpdir.JoinPath("d", "ignore_dir"),
		},
	}
	fw.Spec.WatchedPaths = []string{f.tmpdir.JoinPath("d")}
	f.Update(fw)

	// sandwich in some ignored files with seen files on the outside as synchronization
	f.ChangeAndWaitForSeenFile(key, "d", "1")
	f.ChangeFile("a", "2")
	f.ChangeFile("d", "ignore_me")
	f.ChangeFile("d", "ignore_dir", "file")
	f.ChangeAndWaitForSeenFile(key, "d", "2")

	var updated filewatches.FileWatch
	f.MustGet(key, &updated)
	updatedStart := updated.Status.MonitorStartTime.Time
	assert.Truef(t, updatedStart.After(originalStart),
		"Monitor start time should be more recent after update, (original: %s, updated: %s)",
		originalStart, updatedStart)
	if assert.Equal(t, 2, len(updated.Status.FileEvents)) {
		// ensure ONLY the expected files were seen
		assert.NotZero(t, updated.Status.FileEvents[0].Time.Time)
		mostRecentEventTime := updated.Status.FileEvents[1].Time.Time
		assert.NotZero(t, mostRecentEventTime)
		assert.Equal(t, []string{f.tmpdir.JoinPath("d", "1")}, updated.Status.FileEvents[0].SeenFiles)
		assert.Equal(t, []string{f.tmpdir.JoinPath("d", "2")}, updated.Status.FileEvents[1].SeenFiles)
		assert.Equal(t, mostRecentEventTime, updated.Status.LastEventTime.Time)
	}
}

func TestController_Disable_By_Configmap(t *testing.T) {
	f := newFixture(t)
	key, _ := f.CreateSimpleFileWatch()

	// when enabling the configmap, the filewatch object is enabled
	f.setDisabled(key, true)

	// when disabling the configmap, the filewatch object is disabled
	f.setDisabled(key, false)

	// when enabling the configmap, the filewatch object is enabled
	f.setDisabled(key, true)
}

func TestController_Disable_Ignores_File_Changes(t *testing.T) {
	f := newFixture(t)
	key, _ := f.CreateSimpleFileWatch()

	// Disable the filewatch
	f.setDisabled(key, true)
	// Change a watched file
	f.ChangeFile("a", "1")

	// Expect that no file events were triggered
	var fwAfterDisable filewatches.FileWatch
	f.MustGet(key, &fwAfterDisable)
	require.Equal(t, 0, len(fwAfterDisable.Status.FileEvents))
}
