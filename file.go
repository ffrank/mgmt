// Mgmt
// Copyright (C) 2013-2016+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"gopkg.in/fsnotify.v1"
	//"github.com/go-fsnotify/fsnotify" // git master of "gopkg.in/fsnotify.v1"
	"encoding/gob"
	"io"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

func init() {
	gob.Register(&FileRes{})
}

type FileRes struct {
	BaseRes   `yaml:",inline"`
	Path      string `yaml:"path"` // path variable (should default to name)
	Dirname   string `yaml:"dirname"`
	Basename  string `yaml:"basename"`
	Content   string `yaml:"content"`
	State     string `yaml:"state"` // state: exists/present?, absent, (undefined?)
	sha256sum string
}

func NewFileRes(name, path, dirname, basename, content, state string) *FileRes {
	// FIXME if path = nil, path = name ...
	obj := &FileRes{
		BaseRes: BaseRes{
			Name: name,
		},
		Path:      path,
		Dirname:   dirname,
		Basename:  basename,
		Content:   content,
		State:     state,
		sha256sum: "",
	}
	obj.Init()
	return obj
}

func (obj *FileRes) Init() {
	obj.BaseRes.kind = "File"
	obj.BaseRes.Init() // call base init, b/c we're overriding
}

func (obj *FileRes) GetPath() string {
	d := Dirname(obj.Path)
	b := Basename(obj.Path)
	if !obj.Validate() || (obj.Dirname == "" && obj.Basename == "") {
		return obj.Path
	} else if obj.Dirname == "" {
		return d + obj.Basename
	} else if obj.Basename == "" {
		return obj.Dirname + b
	} else { // if obj.dirname != "" && obj.basename != "" {
		return obj.Dirname + obj.Basename
	}
}

// validate if the params passed in are valid data
func (obj *FileRes) Validate() bool {
	if obj.Dirname != "" {
		// must end with /
		if obj.Dirname[len(obj.Dirname)-1:] != "/" {
			return false
		}
	}
	if obj.Basename != "" {
		// must not start with /
		if obj.Basename[0:1] == "/" {
			return false
		}
	}
	return true
}

// File watcher for files and directories
// Modify with caution, probably important to write some test cases first!
// obj.GetPath(): file or directory
func (obj *FileRes) Watch(processChan chan Event) {
	if obj.IsWatching() {
		return
	}
	obj.SetWatching(true)
	defer obj.SetWatching(false)
	cuuid := obj.converger.Register()
	defer cuuid.Unregister()

	//var recursive bool = false
	var isDir = strings.HasSuffix(obj.GetPath(), "/")
	//log.Printf("IsDirectory: %v", isdir)
	var safename = path.Clean(obj.GetPath()) // no trailing slash

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	patharray := PathSplit(safename) // tokenize the path
	var index = len(patharray)       // starting index
	var maxIndex = index             // how deep we will descend into the filesystem
	var current string               // current "watcher" location
	var deltaDepth int               // depth delta between watcher and event
	var send = false                 // send event?
	var exit = false
	var dirty = false

	for {
		current = strings.Join(patharray[0:index], "/")
		if current == "" { // the empty string top is the root dir ("/")
			current = "/"
		}
		if DEBUG {
			log.Printf("File[%v]: Watching: %v", obj.GetName(), current) // attempting to watch...
		}
		// initialize in the loop so that we can reset on rm-ed handles
		if isDir && index == maxIndex {
			// add all subdirectories recursively if this is the managed directory
			err = obj.AddSubdirsToWatcher(current, watcher)
		} else {
			err = watcher.Add(current)
		}
		if err != nil {
			if DEBUG {
				log.Printf("File[%v]: watcher.Add(%v): Error: %v", obj.GetName(), current, err)
			}
			if err == syscall.ENOENT {
				index-- // usually not found, move up one dir
			} else if err == syscall.ENOSPC {
				// XXX: occasionally: no space left on device,
				// XXX: probably due to lack of inotify watches
				log.Printf("%v[%v]: Out of inotify watches!", obj.Kind(), obj.GetName())
				log.Fatal(err)
			} else {
				log.Printf("Unknown file[%v] error:", obj.Name)
				log.Fatal(err)
			}
			index = int(math.Max(1, float64(index)))
			continue
		}

		obj.SetState(resStateWatching) // reset
		select {
		case event := <-watcher.Events:
			if DEBUG {
				log.Printf("File[%v]: Watch(%v), Event(%v): %v", obj.GetName(), current, event.Name, event.Op)
			}
			cuuid.SetConverged(false) // XXX: technically i can detect if the event is erroneous or not first
			// the deeper you go, the bigger the deltaDepth is...
			// this is the difference between what we're watching,
			// and the event... doesn't mean we can't watch deeper
			if current == event.Name {
				deltaDepth = 0 // i was watching what i was looking for

			} else if HasPathPrefix(event.Name, current) {
				deltaDepth = len(PathSplit(current)) - len(PathSplit(event.Name)) // -1 or less

			} else if HasPathPrefix(current, event.Name) {
				deltaDepth = len(PathSplit(event.Name)) - len(PathSplit(current)) // +1 or more

			} else {
				// TODO different watchers get each others events!
				// https://github.com/go-fsnotify/fsnotify/issues/95
				// this happened with two values such as:
				// event.Name: /tmp/mgmt/f3 and current: /tmp/mgmt/f2
				continue
			}
			//log.Printf("The delta depth is: %v", deltaDepth)

			// if we have what we wanted, awesome, send an event...
			if event.Name == safename {
				//log.Println("Event!")
				// FIXME: should all these below cases trigger?
				send = true
				dirty = true

				// file removed, move the watch upwards
				if deltaDepth >= 0 && (event.Op&fsnotify.Remove == fsnotify.Remove) {
					//log.Println("Removal!")
					watcher.Remove(current)
					index--
				}

				// we must be a parent watcher, so descend in
				// unless we're at the max, which means that this is a directory with recursive watches
				if deltaDepth < 0 && index < maxIndex {
					watcher.Remove(current)
					index++
				}

				// if safename starts with event.Name, we're above, and no event should be sent
			} else if HasPathPrefix(safename, event.Name) {
				//log.Println("Above!")

				if deltaDepth >= 0 && (event.Op&fsnotify.Remove == fsnotify.Remove) {
					log.Println("Removal!")
					watcher.Remove(current)
					index--
				}

				if deltaDepth < 0 {
					log.Println("Parent!")
					if PathPrefixDelta(safename, event.Name) == 1 { // we're the parent dir
						send = true
						dirty = true
					}
					if index < maxIndex { // this should always be true at this point
						watcher.Remove(current)
						index++
					}
				}

				// if event.Name startswith safename, send event, we're already deeper
			} else if HasPathPrefix(event.Name, safename) {
				//log.Println("Event2!")
				send = true
				dirty = true
			}

		case err := <-watcher.Errors:
			cuuid.SetConverged(false) // XXX ?
			log.Printf("error: %v", err)
			log.Fatal(err)
			//obj.events <- fmt.Sprintf("file: %v", "error") // XXX: how should we handle errors?

		case event := <-obj.events:
			cuuid.SetConverged(false)
			if exit, send = obj.ReadEvent(&event); exit {
				return // exit
			}
			//dirty = false // these events don't invalidate state

		case _ = <-cuuid.ConvergedTimer():
			cuuid.SetConverged(true) // converged!
			continue
		}

		// do all our event sending all together to avoid duplicate msgs
		if send {
			send = false
			// only invalid state on certain types of events
			if dirty {
				dirty = false
				obj.isStateOK = false // something made state dirty
			}
			resp := NewResp()
			processChan <- Event{eventNil, resp, "", true} // trigger process
			resp.ACKWait()                                 // wait for the ACK()
		}
	}
}

func (obj *FileRes) AddSubdirsToWatcher(base string, watcher *fsnotify.Watcher) error {
	return filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("File[%v]: error adding %s to watch: %s", obj.GetName(), path, err)
			return filepath.SkipDir
		}
		if info.IsDir() {
			if info.Name() == "." || info.Name() == ".." {
				return filepath.SkipDir
			}
			if DEBUG {
				log.Printf("File[%v]: adding %s to watch", obj.GetName(), path)
			}
			err = watcher.Add(path)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (obj *FileRes) HashSHA256fromContent() string {
	if obj.sha256sum != "" { // return if already computed
		return obj.sha256sum
	}

	hash := sha256.New()
	hash.Write([]byte(obj.Content))
	obj.sha256sum = hex.EncodeToString(hash.Sum(nil))
	return obj.sha256sum
}

func fileHashSHA256(apath string) (string, error) {
	if PathIsDir(apath) { // assert
		log.Fatal("This should only be called on a File resource.")
	}
	// run a diff, and return true if needs changing
	hash := sha256.New()
	f, err := os.Open(apath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	sha256sum := hex.EncodeToString(hash.Sum(nil))

	return sha256sum, nil
}

func (obj *FileRes) FileHashSHA256Check() (bool, error) {
	if PathIsDir(obj.GetPath()) { // assert
		log.Fatal("This should only be called on a File resource.")
	}
	// run a diff, and return true if it needs changing
	hash := sha256.New()
	f, err := os.Open(obj.GetPath())
	if err != nil {
		if e, ok := err.(*os.PathError); ok && (e.Err.(syscall.Errno) == syscall.ENOENT) {
			return false, nil // no "error", file is just absent
		}
		return false, err
	}
	defer f.Close()
	if _, err := io.Copy(hash, f); err != nil {
		return false, err
	}
	sha256sum := hex.EncodeToString(hash.Sum(nil))
	//log.Printf("sha256sum: %v", sha256sum)
	if obj.HashSHA256fromContent() == sha256sum {
		return true, nil
	}
	return false, nil
}

func (obj *FileRes) FileApply() error {
	if PathIsDir(obj.GetPath()) {
		log.Fatal("This should only be called on a File resource.")
	}

	if obj.State == "absent" {
		log.Printf("About to remove: %v", obj.GetPath())
		err := os.Remove(obj.GetPath())
		return err // either nil or not, for success or failure
	}

	f, err := os.Create(obj.GetPath())
	if err != nil {
		return nil
	}
	defer f.Close()

	_, err = io.WriteString(f, obj.Content)
	if err != nil {
		return err
	}

	return nil // success
}

func (obj *FileRes) DirApply() error {
	if !PathIsDir(obj.GetPath()) {
		log.Fatal("This should only be called on a Dir resource.")
	}

	if obj.State == "absent" {
		log.Printf("About to remove: %v", obj.GetPath())
		err := os.Remove(obj.GetPath())
		return err // either nil or not, for success or failure
	}

	// TODO: should this case also clear out existing directory contents?
	if obj.Content == "" {
		if err := os.Mkdir(obj.GetPath(), 0777); err != nil {
			return err
		}
		return nil
	}

	fileAtDestination := make(map[string]bool)
	source := obj.Content
	destination := obj.GetPath()

	// On the first pass, compare all managed files with the source directory.
	// Remove spurious entries, sync links and files.
	filepath.Walk(destination, func(destPath string, destInfo os.FileInfo, err error) error {
		if err != nil {
			log.Printf("File[%v]: error looking up content path %v: %v", obj.GetName(), destPath, err)
			return filepath.SkipDir
		}

		relativePath, err := filepath.Rel(destination, destPath)
		sourcePath := filepath.Join(source, relativePath)

		sourceInfo, err := os.Lstat(sourcePath)
		if os.IsNotExist(err) {
			if err = os.RemoveAll(destPath); err != nil {
				log.Printf("File[%v]: error removing %v: %v", obj.GetName, destPath, err)
			}
			return nil
		}

		if destInfo.Mode()&os.ModeType != sourceInfo.Mode()&os.ModeType {
			if err = os.RemoveAll(destPath); err != nil {
				log.Printf("File[%v]: error removing %v: %v", obj.GetName, destPath, err)
			}
			// Just keep walking; this entry will be rewritten during the 2nd pass.
			return nil
		}

		// Directories need no immediate processing; contents will be walked next.
		if destInfo.IsDir() {
			return nil
		}

		// Mark this entry as already checked, so that the 2nd pass can ignore it.
		fileAtDestination[relativePath] = true

		// Filter sockets, pipes etc., i.e. everything that is no file or symlink.
		if (destInfo.Mode()&os.ModeType)&^os.ModeSymlink != 0 {
			log.Printf("File[%v]: not processing unsupported filesystem entry %v", obj.GetName, destPath)
			return nil
		}

		if destInfo.Mode()&os.ModeSymlink != 0 {
			err = copySymlink(sourcePath, destPath)
			if err != nil {
				log.Printf("File[%v]: could not sync link %v from %v: %v", obj.GetName, destPath, sourcePath, err)
				return nil
			}
		}

		sourceHash, err := fileHashSHA256(sourcePath)
		if err != nil {
			log.Printf("File[%v]: error reading source file %v: %v", obj.GetName, sourcePath, err)
			return nil
		}
		destHash, err := fileHashSHA256(destPath)
		if err != nil {
			log.Printf("File[%v]: error reading managed file %v: %v", obj.GetName, destPath, err)
			return nil
		}

		if sourceHash == destHash {
			return nil
		}

		err = copyFile(sourcePath, destPath)
		if err != nil {
			log.Printf("File[%v]: error copying %v to %v: %v", obj.GetName, sourcePath, destPath, err)
			return nil
		}

		return nil
	})

	// On the second pass, visit all files in the source directory.
	// Any file not seen in the managed directory tree needs copying.
	filepath.Walk(source, func(sourcePath string, sourceInfo os.FileInfo, err error) error {
		relativePath, err := filepath.Rel(source, sourcePath)

		if fileAtDestination[relativePath] {
			return nil
		}

		destPath := filepath.Join(destination, relativePath)

		if sourceInfo.IsDir() {
			if err = os.Mkdir(destPath, sourceInfo.Mode()); err != nil {
				log.Printf("File[%v]: error creating subdirectory %v: %v", obj.GetName, destPath, err)
				return filepath.SkipDir
			}
			return nil
		}

		if sourceInfo.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(sourcePath)
			if err != nil {
				log.Printf("File[%v]: could not read link %v: %v", obj.GetName, sourcePath, err)
				return nil
			}
			err = os.Symlink(target, destPath)
			if err != nil {
				log.Printf("File[%v]: could not create link %v: %v", obj.GetName, destPath, err)
			}
			return nil
		}

		err = copyFile(sourcePath, destPath)
		if err != nil {
			log.Printf("File[%v]: error copying %v to %v: %v", obj.GetName, sourcePath, destPath, err)
		}

		return nil
	})

	return nil
}

func copySymlink(sourcePath, destPath string) error {
	content, err := os.Readlink(sourcePath)
	if err != nil {
		return err
	}

	current, err := os.Readlink(destPath)
	if err != nil {
		return err
	}

	if content == current {
		return nil
	}

	if err = os.Remove(destPath); err != nil {
		return err
	}

	return os.Symlink(content, destPath)
}

func copyFile(sourcePath, destPath string) error {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)

	return err
}

func collectPaths(srcpath string) ([]string, error) {
	paths := make([]string, 1)
	err := filepath.Walk(srcpath, func(apath string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(srcpath, apath)
		if rel != "." && rel != "" {
			paths = append(paths, rel)
		}
		return nil
	})
	return paths, err
}

func (obj *FileRes) DirCheck() (bool, error) {
	expected_paths, err := collectPaths(obj.Content)
	if err != nil {
		return false, err
	}

	for _, expected_path := range expected_paths {
		if _, err := os.Stat(path.Join(obj.GetPath(), expected_path)); os.IsNotExist(err) {
			return false, nil
		}

		info, err := os.Stat(expected_path)
		if err != nil {
			return false, err
		}

		if mode := info.Mode(); mode.IsRegular() {
			continue
		}

		expected_sha256, err := fileHashSHA256(path.Join(obj.Content, expected_path))
		if err != nil {
			return false, err
		}

		actual_sha256, err := fileHashSHA256(path.Join(obj.GetPath(), expected_path))
		if err != nil {
			return false, err
		}

		if expected_sha256 != actual_sha256 {
			return false, nil
		}
	}

	return true, nil
}

func (obj *FileRes) CheckApply(apply bool) (stateok bool, err error) {
	log.Printf("%v[%v]: CheckApply(%t)", obj.Kind(), obj.GetName(), apply)

	if obj.isStateOK { // cache the state
		return true, nil
	}

	if _, err = os.Stat(obj.GetPath()); os.IsNotExist(err) {
		// no such file or directory
		if obj.State == "absent" {
			// missing file should be missing, phew :)
			obj.isStateOK = true
			return true, nil
		}
	}
	err = nil // reset

	// FIXME: add file mode check here...
	if _, err := os.Stat(obj.GetPath()); err == nil {
		if PathIsDir(obj.GetPath()) && obj.Content == "" {
			obj.isStateOK = true
			return true, nil
		}
		if PathIsDir(obj.GetPath()) {
			ok, err := obj.DirCheck()
			if err != nil {
				return false, err
			}
			if ok {
				obj.isStateOK = true
				return true, nil
			}
		} else {
			ok, err := obj.FileHashSHA256Check()
			if err != nil {
				return false, err
			}
			if ok {
				obj.isStateOK = true
				return true, nil
			}
			// if no err, but !ok, then we continue on...
		}
	}

	// state is not okay, no work done, exit, but without error
	if !apply {
		return false, nil
	}

	// apply portion
	log.Printf("%v[%v]: Apply", obj.Kind(), obj.GetName())
	if PathIsDir(obj.GetPath()) {
		err = obj.DirApply()
	} else {
		err = obj.FileApply()
	}

	if err != nil {
		return false, err
	}

	obj.isStateOK = true
	return false, nil // success
}

type FileUUID struct {
	BaseUUID
	path string
}

// if and only if they are equivalent, return true
// if they are not equivalent, return false
func (obj *FileUUID) IFF(uuid ResUUID) bool {
	res, ok := uuid.(*FileUUID)
	if !ok {
		return false
	}
	return obj.path == res.path
}

type FileResAutoEdges struct {
	data    []ResUUID
	pointer int
	found   bool
}

func (obj *FileResAutoEdges) Next() []ResUUID {
	if obj.found {
		log.Fatal("Shouldn't be called anymore!")
	}
	if len(obj.data) == 0 { // check length for rare scenarios
		return nil
	}
	value := obj.data[obj.pointer]
	obj.pointer++
	return []ResUUID{value} // we return one, even though api supports N
}

// get results of the earlier Next() call, return if we should continue!
func (obj *FileResAutoEdges) Test(input []bool) bool {
	// if there aren't any more remaining
	if len(obj.data) <= obj.pointer {
		return false
	}
	if obj.found { // already found, done!
		return false
	}
	if len(input) != 1 { // in case we get given bad data
		log.Fatal("Expecting a single value!")
	}
	if input[0] { // if a match is found, we're done!
		obj.found = true // no more to find!
		return false
	}
	return true // keep going
}

// generate a simple linear sequence of each parent directory from bottom up!
func (obj *FileRes) AutoEdges() AutoEdge {
	var data []ResUUID                             // store linear result chain here...
	values := PathSplitFullReversed(obj.GetPath()) // build it
	_, values = values[0], values[1:]              // get rid of first value which is me!
	for _, x := range values {
		var reversed = true // cheat by passing a pointer
		data = append(data, &FileUUID{
			BaseUUID: BaseUUID{
				name:     obj.GetName(),
				kind:     obj.Kind(),
				reversed: &reversed,
			},
			path: x, // what matters
		}) // build list
	}
	return &FileResAutoEdges{
		data:    data,
		pointer: 0,
		found:   false,
	}
}

func (obj *FileRes) GetUUIDs() []ResUUID {
	x := &FileUUID{
		BaseUUID: BaseUUID{name: obj.GetName(), kind: obj.Kind()},
		path:     obj.GetPath(),
	}
	return []ResUUID{x}
}

func (obj *FileRes) GroupCmp(r Res) bool {
	_, ok := r.(*FileRes)
	if !ok {
		return false
	}
	// TODO: we might be able to group directory children into a single
	// recursive watcher in the future, thus saving fanotify watches
	return false // not possible atm
}

func (obj *FileRes) CopyFile(srcpath, dstpath string) error {
	srcfile, err := os.Open(srcpath)
	if err != nil {
		return err
	}
	defer srcfile.Close()

	dstfile, err := os.Create(dstpath)
	if err != nil {
		return err
	}
	defer dstfile.Close()

	if _, err := io.Copy(dstfile, srcfile); err != nil {
		dstfile.Close()
		return err
	}

	return dstfile.Close()
}

func (obj *FileRes) Compare(res Res) bool {
	switch res.(type) {
	case *FileRes:
		res := res.(*FileRes)
		if obj.Name != res.Name {
			return false
		}
		if obj.GetPath() != res.Path {
			return false
		}
		if obj.Content != res.Content {
			return false
		}
		if obj.State != res.State {
			return false
		}
	default:
		return false
	}
	return true
}

func (obj *FileRes) CollectPattern(pattern string) {
	// XXX: currently the pattern for files can only override the Dirname variable :P
	obj.Dirname = pattern // XXX: simplistic for now
}
