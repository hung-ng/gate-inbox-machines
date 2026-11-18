package machines

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// The extension's files in its data dir, beside the mirrors' state.
const (
	// storeFileName holds the machines on this board and their pauses.
	storeFileName = "machines.json"
	// learnedFileName is what each machine last said about itself, so a
	// mirror's pane and the CLI attach and poll the way the board does.
	learnedFileName = "learned.json"
	// lockFileName serializes writers of the two, which can be the board
	// and a `machines add` run from a shell at the same moment.
	lockFileName = "machines.lock"
)

// storeFile is machines.json.
type storeFile struct {
	Machines map[string]hostSettings `json:"machines,omitempty"`
	// Paused names machines that are not polled.
	Paused []string `json:"paused,omitempty"`
}

// learnedEntry is one machine's line in learned.json. Destination ties it
// to the machine it came from, so a name reused for another destination
// starts over.
type learnedEntry struct {
	Destination string    `json:"destination"`
	About       about     `json:"about"`
	At          time.Time `json:"at"`
}

// registry reads the board's machines and what they last reported about
// themselves. The poll loop and key handlers share it, and it reads files
// again when they change on disk.
type registry struct {
	// io serializes this process's reads and writes of the files, and
	// guards stamps. It is never held while mu is wanted by a caller that
	// only reads: the board draws the machines screen from all(), on its
	// event loop, so a frame must not wait on a disk or on another
	// process's lock.
	io     sync.Mutex
	stamps map[string]fileStamp

	mu      sync.Mutex
	dir     string
	store   storeFile
	learned map[string]learnedEntry
	hosts   []host
	// problem is why the last read of machines.json or one of its
	// machines was refused, for the board's log and the machines screen.
	problem string
}

func newRegistry(dir string) *registry {
	r := &registry{dir: dir, stamps: map[string]fileStamp{}, learned: map[string]learnedEntry{}}
	r.reload()
	r.mu.Lock()
	r.buildLocked()
	r.mu.Unlock()
	return r
}

// all is every machine, paused ones included, sorted by name.
func (r *registry) all() []host {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.hosts)
}

// active is every machine that is not paused.
func (r *registry) active() []host {
	return slices.DeleteFunc(r.all(), func(h host) bool { return h.Paused })
}

func (r *registry) host(name string) (host, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hosts {
		if h.Name == name {
			return h, true
		}
	}
	return host{}, false
}

func (r *registry) issue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.problem
}

// reload reads the files again if they changed since the last read, and
// says whether the machines did.
func (r *registry) reload() bool {
	r.io.Lock()
	defer r.io.Unlock()
	read, ok := r.readFiles()
	if !ok {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.hosts
	if read.store != nil {
		r.store = *read.store
	}
	if read.problem != nil {
		r.problem = *read.problem
	}
	if read.learned != nil {
		r.learned = read.learned
	}
	r.buildLocked()
	return !slices.EqualFunc(before, r.hosts, sameHost)
}

func sameHost(a, b host) bool {
	if (a.About == nil) != (b.About == nil) || (a.About != nil && !a.About.equal(*b.About)) {
		return false
	}
	a.About, b.About = nil, nil
	return a == b
}

// filesRead is what readFiles found: nil for each part that didn't move.
type filesRead struct {
	store   *storeFile
	problem *string
	learned map[string]learnedEntry
}

// readFiles reads each file whose stamp moved, holding io, and says
// whether any did.
func (r *registry) readFiles() (filesRead, bool) {
	var read filesRead
	if r.dir == "" {
		return read, false
	}
	changed := false
	if stamp, moved := r.moved(storeFileName); moved {
		changed = true
		store, err := readStore(filepath.Join(r.dir, storeFileName))
		switch {
		case err == nil:
			read.store = &store
			none := ""
			read.problem = &none
		default:
			// The last good list stands, so a file caught mid-edit by hand
			// does not take every added machine off the board.
			problem := fmt.Sprintf("could not read %s: %v", storeFileName, err)
			read.problem = &problem
		}
		r.stamps[storeFileName] = stamp
	}
	if stamp, moved := r.moved(learnedFileName); moved {
		changed = true
		learned := map[string]learnedEntry{}
		switch err := readJSON(filepath.Join(r.dir, learnedFileName), &learned); {
		case err == nil:
			read.learned = cleanLearned(learned)
		case errors.Is(err, os.ErrNotExist):
			read.learned = map[string]learnedEntry{}
		}
		r.stamps[learnedFileName] = stamp
	}
	return read, changed
}

// buildLocked lays the machines out from what was read, holding mu.
func (r *registry) buildLocked() {
	paused := map[string]bool{}
	for _, name := range r.store.Paused {
		paused[name] = true
	}
	var hosts []host
	for _, name := range sortedKeys(r.store.Machines) {
		h, err := resolveHost(name, r.store.Machines[name])
		if err != nil {
			r.problem = fmt.Sprintf("%s in %s: %v", name, storeFileName, err)
			continue
		}
		hosts = append(hosts, h)
	}
	for i, h := range hosts {
		h.Paused = paused[h.Name]
		if entry, ok := r.learned[h.Name]; ok && entry.Destination == h.Destination {
			h = h.withAbout(entry.About)
		}
		hosts[i] = h
	}
	r.hosts = hosts
}

// learn records what a machine said about itself, and says whether that
// changed anything. It is saved for the other processes of this build.
func (r *registry) learn(name, destination string, a about) (bool, error) {
	a = a.clean()
	r.mu.Lock()
	entry, ok := r.learned[name]
	r.mu.Unlock()
	if ok && entry.Destination == destination && entry.About.equal(a) {
		return false, nil
	}
	entry = learnedEntry{Destination: destination, About: a, At: time.Now().UTC()}
	// io makes this process's learns take turns, so two machines learned
	// at once can't each leave the other's entry out; the file lock does
	// the same for other processes.
	r.io.Lock()
	defer r.io.Unlock()
	var merged map[string]learnedEntry
	var err error
	if r.dir != "" {
		// Another process may have learned about another machine since
		// this one last read the file, so the entry is merged into it as
		// it is on disk.
		err = r.locked(func() error {
			merged = map[string]learnedEntry{}
			if err := readJSON(filepath.Join(r.dir, learnedFileName), &merged); err != nil {
				merged = map[string]learnedEntry{}
			}
			merged = cleanLearned(merged)
			merged[name] = entry
			return writeJSON(filepath.Join(r.dir, learnedFileName), merged)
		})
		if err == nil {
			r.stamps[learnedFileName], _ = r.moved(learnedFileName)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil && merged != nil {
		r.learned = merged
	} else {
		r.learned[name] = entry
	}
	r.buildLocked()
	return true, err
}

func cleanLearned(learned map[string]learnedEntry) map[string]learnedEntry {
	if learned == nil {
		return map[string]learnedEntry{}
	}
	for name, entry := range learned {
		entry.About = entry.About.clean()
		learned[name] = entry
	}
	return learned
}

// errNoStore is a registry with no data dir to save a machine in.
var errNoStore = errors.New("this build has no data directory to keep added machines in")

// change applies edit to machines.json as it is on disk now, under the
// lock, and saves it.
func (r *registry) change(edit func(store *storeFile) error) error {
	if r.dir == "" {
		return errNoStore
	}
	r.io.Lock()
	err := r.locked(func() error {
		store, err := readStore(filepath.Join(r.dir, storeFileName))
		if err != nil {
			return fmt.Errorf("could not read %s: %w; fix or delete it first", storeFileName, err)
		}
		if store.Machines == nil {
			store.Machines = map[string]hostSettings{}
		}
		if err := edit(&store); err != nil {
			return err
		}
		slices.Sort(store.Paused)
		store.Paused = slices.Compact(store.Paused)
		return writeJSON(filepath.Join(r.dir, storeFileName), store)
	})
	r.io.Unlock()
	if err != nil {
		return err
	}
	r.reload()
	return nil
}

// locked runs fn holding the data dir's lock file.
func (r *registry) locked(fn func() error) error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// fileStamp tells a file rewritten by rename from the one read before.
type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
	ino    uint64
}

func (r *registry) moved(name string) (fileStamp, bool) {
	var stamp fileStamp
	if info, err := os.Stat(filepath.Join(r.dir, name)); err == nil {
		stamp = fileStamp{exists: true, mod: info.ModTime(), size: info.Size()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			stamp.ino = uint64(st.Ino)
		}
	} else if _, err := os.Lstat(filepath.Join(r.dir, name)); !errors.Is(err, os.ErrNotExist) {
		// Retry unreadable paths, including dangling symlinks.
		return stamp, true
	}
	before, seen := r.stamps[name]
	return stamp, !seen || before != stamp
}

func readStore(path string) (storeFile, error) {
	var store *storeFile
	err := readJSON(path, &store)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return storeFile{}, nil
		}
	}
	if err != nil {
		return storeFile{}, err
	}
	if store == nil {
		return storeFile{}, errors.New("expected an object in machines.json")
	}
	return *store, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeJSON replaces path by rename, so a reader never sees half a file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
