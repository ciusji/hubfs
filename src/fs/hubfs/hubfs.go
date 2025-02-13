/*
 * hubfs.go
 *
 * Copyright 2021-2022 Bill Zissimopoulos
 */
/*
 * This file is part of Hubfs.
 *
 * You can redistribute it and/or modify it under the terms of the GNU
 * Affero General Public License version 3 as published by the Free
 * Software Foundation.
 */

package hubfs

import (
	"io"
	pathutil "path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	libtrace "github.com/billziss-gh/golib/trace"
	"github.com/winfsp/cgofuse/fuse"
	"github.com/winfsp/hubfs/fs/port"
	"github.com/winfsp/hubfs/prov"
)

type hubfs struct {
	fuse.FileSystemBase
	client  prov.Client
	prefix  string
	lock    sync.RWMutex
	fh      uint64
	openmap map[uint64]*obstack
}

type obstack struct {
	owner      prov.Owner
	repository prov.Repository
	ref        prov.Ref
	entry      prov.TreeEntry
	reader     io.ReaderAt
}

type Config struct {
	Client  prov.Client
	Prefix  string
	Caseins bool
	Overlay bool
}

const refSlashSeparator = "+"

func new(c Config) fuse.FileSystemInterface {
	return &hubfs{
		client:  c.Client,
		prefix:  c.Prefix,
		openmap: make(map[uint64]*obstack),
	}
}

func (fs *hubfs) openex(path string, norm bool) (errc int, res *obstack, lst []string) {
	if strings.HasSuffix(path, "/.") {
		errc = -fuse.ENOENT
		return
	}

	lst = split(pathutil.Join(fs.prefix, path))
	obs := &obstack{}
	var err error
	for i, c := range lst {
		switch i {
		case 0:
			// We disallow some names to speed up operations:
			//
			// - All names containing dots: e.g. ".git", ".DS_Store", "autorun.inf"
			// - The special git name HEAD
			if -1 != strings.IndexFunc(c, func(r rune) bool { return '.' == r }) || "HEAD" == c {
				obs.owner, err = nil, prov.ErrNotFound
			} else {
				obs.owner, err = fs.client.OpenOwner(c)
				if norm && nil == err {
					lst[i] = obs.owner.Name()
				}
			}
		case 1:
			obs.repository, err = fs.client.OpenRepository(obs.owner, c)
			if norm && nil == err {
				lst[i] = obs.repository.Name()
			}
		case 2:
			c = strings.ReplaceAll(c, refSlashSeparator, "/")
			obs.ref, err = obs.repository.GetRef("refs/heads/" + c)
			if prov.ErrNotFound == err {
				obs.ref, err = obs.repository.GetRef("refs/tags/" + c)
				if prov.ErrNotFound == err {
					obs.ref, err = obs.repository.GetTempRef(c)
				}
			}
			if norm && nil == err {
				r := obs.ref.Name()
				n := strings.TrimPrefix(r, "refs/heads/")
				if r == n {
					n = strings.TrimPrefix(r, "refs/tags/")
					if r == n {
						n = r
					}
				}
				n = strings.ReplaceAll(n, "/", refSlashSeparator)
				lst[i] = n
			}
		default:
			obs.entry, err = obs.repository.GetTreeEntry(obs.ref, obs.entry, c)
			if norm && nil == err {
				lst[i] = obs.entry.Name()
			}
		}
		if nil != err {
			fs.release(obs)
			errc = fuseErrc(err)
			return
		}
	}
	res = obs
	return
}

func (fs *hubfs) open(path string) (errc int, res *obstack) {
	errc, res, _ = fs.openex(path, false)
	return
}

func (fs *hubfs) release(obs *obstack) {
	if nil != obs.repository {
		fs.client.CloseRepository(obs.repository)
	}
	if nil != obs.owner {
		fs.client.CloseOwner(obs.owner)
	}
}

func (fs *hubfs) getattr(obs *obstack, entry prov.TreeEntry, path string, stat *fuse.Stat_t) (
	target string) {

	if nil != entry {
		mode := entry.Mode()
		fuseStat(stat, mode, entry.Size(), obs.ref.TreeTime())
		switch mode & fuse.S_IFMT {
		case fuse.S_IFLNK:
			target = entry.Target()
			stat.Size = int64(len(target))
		case 0160000 /* submodule */ :
			path = pathutil.Join(fs.prefix, path)
			target = entry.Target()
			remain := repoPath(path)
			module, err := obs.repository.GetModule(obs.ref, remain, true)
			if "" != module {
				if t, e := filepath.Rel(pathutil.Dir(path), module+"/"+entry.Target()); nil == e {
					if "windows" == runtime.GOOS {
						t = strings.ReplaceAll(t, `\`, `/`)
					}
					target = t
				} else {
					target = strings.TrimPrefix(module, fs.prefix) + "/" + entry.Target()
				}
			} else {
				tracef("repo=%#v Getmodule(ref=%#v, %#v) = %v",
					obs.repository.Name(), obs.ref.Name(), remain, err)
			}
			stat.Size = int64(len(target))
		}
	} else {
		fuseStat(stat, fuse.S_IFDIR, 0, time.Now())
	}

	return
}

func (fs *hubfs) Getpath(path string, fh uint64) (errc int, normpath string) {
	defer trace(path, fh)(&errc, &normpath)

	errc0, obs, pathlst := fs.openex(path, true)
	if 0 == errc0 {
		fs.release(obs)
	}

	normpath = "/" + pathutil.Join(pathlst...)
	normpath = strings.TrimPrefix(normpath, fs.prefix)
	if "" == normpath {
		normpath = "/"
	}

	return
}

func (fs *hubfs) Getattr(path string, stat *fuse.Stat_t, fh uint64) (errc int) {
	defer trace(path, fh)(&errc, stat)

	errc, obs := fs.open(path)
	if 0 != errc {
		return
	}

	fs.getattr(obs, obs.entry, path, stat)

	fs.release(obs)

	return
}

func (fs *hubfs) Readlink(path string) (errc int, target string) {
	defer trace(path)(&errc, &target)

	errc, obs := fs.open(path)
	if 0 != errc {
		return
	}

	stat := fuse.Stat_t{}
	target = fs.getattr(obs, obs.entry, path, &stat)
	if "" == target {
		errc = -fuse.EINVAL
	}

	fs.release(obs)

	return
}

func (fs *hubfs) Opendir(path string) (errc int, fh uint64) {
	defer trace(path)(&errc, &fh)

	errc, obs := fs.open(path)
	if 0 != errc {
		return
	}

	fs.lock.Lock()
	fh = fs.fh
	fs.openmap[fh] = obs
	fs.fh++
	fs.lock.Unlock()

	return
}

func (fs *hubfs) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) (errc int) {
	defer trace(path, ofst, fh)(&errc)

	fs.lock.RLock()
	obs, ok := fs.openmap[fh]
	fs.lock.RUnlock()
	if !ok {
		errc = -fuse.ENOENT
		return
	}

	stat := fuse.Stat_t{}
	if nil != obs.entry {
		fuseStat(&stat, fuse.S_IFDIR, 0, obs.ref.TreeTime())
	} else {
		fuseStat(&stat, fuse.S_IFDIR, 0, time.Now())
	}
	fill(".", &stat, 0)
	fill("..", &stat, 0)

	if nil != obs.ref {
		if lst, err := obs.repository.GetTree(obs.ref, obs.entry); nil == err {
			for _, elm := range lst {
				n := elm.Name()
				fs.getattr(obs, elm, pathutil.Join(path, n), &stat)
				if !fill(n, &stat, 0) {
					break
				}
			}
		}
	} else if nil != obs.repository {
		if lst, err := obs.repository.GetRefs(); nil == err {
			for _, elm := range lst {
				r := elm.Name()
				n := strings.TrimPrefix(r, "refs/heads/")
				if r == n {
					continue
				}
				n = strings.ReplaceAll(n, "/", refSlashSeparator)
				if !fill(n, &stat, 0) {
					break
				}
			}
		}
	} else if nil != obs.owner {
		if lst, err := fs.client.GetRepositories(obs.owner); nil == err {
			for _, elm := range lst {
				if !fill(elm.Name(), &stat, 0) {
					break
				}
			}
		}
	} else {
		if lst, err := fs.client.GetOwners(); nil == err {
			for _, elm := range lst {
				if !fill(elm.Name(), &stat, 0) {
					break
				}
			}
		}
	}

	return
}

func (fs *hubfs) Releasedir(path string, fh uint64) (errc int) {
	defer trace(path, fh)(&errc)

	fs.lock.Lock()
	obs, ok := fs.openmap[fh]
	if ok {
		delete(fs.openmap, fh)
	}
	fs.lock.Unlock()
	if !ok {
		errc = -fuse.ENOENT
		return
	}

	fs.release(obs)

	return
}

func (fs *hubfs) Open(path string, flags int) (errc int, fh uint64) {
	defer trace(path, flags)(&errc, &fh)

	errc, obs := fs.open(path)
	if 0 != errc {
		return
	}

	fs.lock.Lock()
	fh = fs.fh
	fs.openmap[fh] = obs
	fs.fh++
	fs.lock.Unlock()

	return
}

func (fs *hubfs) Read(path string, buff []byte, ofst int64, fh uint64) (n int) {
	defer trace(path, ofst, fh)(&n)

	var reader io.ReaderAt

	fs.lock.RLock()
	obs, ok := fs.openmap[fh]
	if ok {
		reader = obs.reader
	}
	fs.lock.RUnlock()
	if !ok {
		n = -fuse.ENOENT
		return
	}

	if nil == reader {
		reader, _ = obs.repository.GetBlobReader(obs.entry)
		if nil == reader {
			n = -fuse.EIO
			return
		}

		var closer io.Closer
		fs.lock.Lock()
		if nil == obs.reader {
			obs.reader = reader
		} else {
			closer = reader.(io.Closer)
			reader = obs.reader
		}
		fs.lock.Unlock()
		if nil != closer {
			closer.Close()
		}
	}

	n, err := reader.ReadAt(buff, ofst)
	if nil != err && io.EOF != err {
		n = fuseErrc(err)
		return
	}

	return
}

func (fs *hubfs) Release(path string, fh uint64) (errc int) {
	defer trace(path, fh)(&errc)

	fs.lock.Lock()
	obs, ok := fs.openmap[fh]
	if ok {
		delete(fs.openmap, fh)
	}
	fs.lock.Unlock()
	if !ok {
		errc = -fuse.ENOENT
		return
	}

	if closer, ok := obs.reader.(io.Closer); ok {
		closer.Close()
	}

	fs.release(obs)

	return
}

func (self *hubfs) Statfs(path string, stat *fuse.Statfs_t) (errc int) {
	return port.Statfs(self.client.GetDirectory(), stat)
}

func fuseErrc(err error) (errc int) {
	errc = -fuse.EIO
	if prov.ErrNotFound == err {
		errc = -fuse.ENOENT
	}
	return
}

func fuseStat(stat *fuse.Stat_t, mode uint32, size int64, time time.Time) {
	switch mode & fuse.S_IFMT {
	case fuse.S_IFDIR:
		mode = fuse.S_IFDIR | 0755
	case fuse.S_IFLNK, 0160000 /* submodule */ :
		mode = fuse.S_IFLNK | 0777
	default:
		mode = fuse.S_IFREG | 0644 | (mode & 0111)
	}
	ts := fuse.NewTimespec(time)
	*stat = fuse.Stat_t{
		Mode:     mode,
		Nlink:    1,
		Size:     size,
		Atim:     ts,
		Mtim:     ts,
		Ctim:     ts,
		Birthtim: ts,
	}
}

func split(path string) []string {
	comp := strings.Split(path, "/")[1:]
	if 1 == len(comp) && "" == comp[0] {
		return []string{}
	}
	return comp
}

func repoPath(path string) string {
	slashes := 0
	for i := 0; len(path) > i; i++ {
		if '/' == path[i] {
			slashes++
			if 4 == slashes {
				return path[i+1:]
			}
		}
	}
	return ""
}

func trace(vals ...interface{}) func(vals ...interface{}) {
	return libtrace.Trace(1, "", vals...)
}

func tracef(form string, vals ...interface{}) {
	libtrace.Tracef(1, form, vals...)
}
