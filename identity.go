package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	ownershipMarkerName = "ownership.json"
	maxRuntimeID        = uint64(1<<32 - 2)
)

type RuntimeIdentity struct {
	UID  int
	GID  int
	Home string
}

type ownershipMarker struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type ownershipHooks struct {
	afterEntryInspect  func(string, int, string, unix.Stat_t) error
	afterEntryOpen     func(string, int, string, int, unix.Stat_t) error
	syncInode          func(string, string, int) error
	beforeMarkerRename func() error
}

func (h ownershipHooks) inspectEntry(mount string, parent int, name string, stat unix.Stat_t) error {
	if h.afterEntryInspect != nil {
		return h.afterEntryInspect(mount, parent, name, stat)
	}
	return nil
}

func (h ownershipHooks) openedEntry(mount string, parent int, name string, fd int, stat unix.Stat_t) error {
	if h.afterEntryOpen != nil {
		return h.afterEntryOpen(mount, parent, name, fd, stat)
	}
	return nil
}

func (h ownershipHooks) sync(mount, name string, fd int) error {
	if h.syncInode != nil {
		return h.syncInode(mount, name, fd)
	}
	return unix.Fsync(fd)
}

func ResolveIdentity(cfg Config) (RuntimeIdentity, error) {
	serverFD, server, err := openMountRoot(cfg.ServerDir, "server mount")
	if err != nil {
		return RuntimeIdentity{}, err
	}
	defer unix.Close(serverFD)
	persistentFD, _, err := openMountRoot(cfg.DataDir, "persistent-data mount")
	if err != nil {
		return RuntimeIdentity{}, err
	}
	defer unix.Close(persistentFD)
	if !filepath.IsAbs(cfg.StateDir) {
		return RuntimeIdentity{}, fmt.Errorf("state directory must be absolute")
	}

	identity := RuntimeIdentity{Home: filepath.Join(cfg.StateDir, "home")}
	explicit := cfg.PUID != nil || cfg.PGID != nil
	switch {
	case cfg.PUID == nil && cfg.PGID == nil:
		identity.UID = int(server.Uid)
		identity.GID = int(server.Gid)
		if identity.UID == 0 {
			log.Printf("security notice: inferred root-owned server mount; runtime processes will retain UID/GID %d:%d", identity.UID, identity.GID)
		}
	case cfg.PUID == nil || cfg.PGID == nil:
		return RuntimeIdentity{}, fmt.Errorf("PUID and PGID must be supplied together")
	case *cfg.PUID <= 0:
		return RuntimeIdentity{}, fmt.Errorf("PUID must be a positive integer")
	case *cfg.PGID <= 0:
		return RuntimeIdentity{}, fmt.Errorf("PGID must be a positive integer")
	default:
		identity.UID = *cfg.PUID
		identity.GID = *cfg.PGID
	}
	if err := validateRuntimeIdentity(identity, explicit); err != nil {
		return RuntimeIdentity{}, err
	}
	return identity, nil
}

func PrepareOwnership(cfg Config, identity RuntimeIdentity) error {
	return prepareOwnership(cfg, identity, ownershipHooks{})
}

func prepareOwnership(cfg Config, identity RuntimeIdentity, hooks ownershipHooks) error {
	serverFD, server, err := openMountRoot(cfg.ServerDir, "server mount")
	if err != nil {
		return err
	}
	defer unix.Close(serverFD)
	persistentFD, persistent, err := openMountRoot(cfg.DataDir, "persistent-data mount")
	if err != nil {
		return err
	}
	defer unix.Close(persistentFD)
	if identity.Home != filepath.Join(cfg.StateDir, "home") {
		return fmt.Errorf("runtime home must be StateDir/home")
	}

	explicit, err := validateConfiguredIdentity(cfg, identity, server)
	if err != nil {
		return err
	}
	stateDir, err := openDirectoryPath(cfg.StateDir, true)
	if err != nil {
		return fmt.Errorf("open StateDir for ownership preparation: %w", err)
	}
	defer unix.Close(stateDir)
	var stateStat unix.Stat_t
	if err := unix.Fstat(stateDir, &stateStat); err != nil {
		return fmt.Errorf("inspect StateDir for ownership preparation: %w", err)
	}
	if !explicit {
		if err := preparePrivateDirectories(stateDir, identity); err != nil {
			return err
		}
		return verifyPathIdentity(cfg.StateDir, stateStat)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("explicit ownership migration requires root")
	}

	complete, err := ownershipMigrationComplete(stateDir, identity)
	if err != nil {
		return err
	}
	if complete {
		if err := preparePrivateDirectories(stateDir, identity); err != nil {
			return err
		}
		return verifyPathIdentity(cfg.StateDir, stateStat)
	}
	if err := invalidateOwnershipMarker(stateDir); err != nil {
		return fmt.Errorf("invalidate ownership migration marker: %w", err)
	}
	log.Printf("ownership migration: changing server and persistent-data mounts to UID/GID %d:%d", identity.UID, identity.GID)
	if err := lchownTree(serverFD, server, cfg.ServerDir, "server", identity, hooks); err != nil {
		return fmt.Errorf("migrate server mount ownership: %w", err)
	}
	if err := lchownTree(persistentFD, persistent, cfg.DataDir, "persistent-data", identity, hooks); err != nil {
		return fmt.Errorf("migrate persistent-data mount ownership: %w", err)
	}
	if err := preparePrivateDirectories(stateDir, identity); err != nil {
		return err
	}
	if err := writeOwnershipMarker(stateDir, cfg.StateDir, stateStat, identity, hooks); err != nil {
		return fmt.Errorf("record ownership migration: %w", err)
	}
	return nil
}

func invalidateOwnershipMarker(stateDir int) error {
	if err := unix.Unlinkat(stateDir, ownershipMarkerName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(stateDir)
}

func DropPrivileges(identity RuntimeIdentity) error {
	if err := validateRuntimeIdentity(identity, false); err != nil {
		return err
	}
	if err := unix.Setgroups([]int{}); err != nil {
		return fmt.Errorf("clear supplementary groups: %w", err)
	}
	if err := unix.Setgid(identity.GID); err != nil {
		return fmt.Errorf("set runtime GID %d: %w", identity.GID, err)
	}
	if err := unix.Setuid(identity.UID); err != nil {
		return fmt.Errorf("set runtime UID %d: %w", identity.UID, err)
	}
	return nil
}

func (identity RuntimeIdentity) VerifyWritable(cfg Config) error {
	if os.Geteuid() != identity.UID || os.Getegid() != identity.GID {
		return fmt.Errorf("effective identity is %d:%d, want %d:%d", os.Geteuid(), os.Getegid(), identity.UID, identity.GID)
	}
	if err := probeWritableMount(cfg.ServerDir); err != nil {
		return fmt.Errorf("server mount is not writable by runtime identity: %w", err)
	}
	if err := probeWritableMount(cfg.DataDir); err != nil {
		return fmt.Errorf("persistent-data mount is not writable by runtime identity: %w", err)
	}
	return nil
}

func inspectMountRoot(path, label string) (unix.Stat_t, error) {
	fd, stat, err := openMountRoot(path, label)
	if err != nil {
		return unix.Stat_t{}, err
	}
	if err := unix.Close(fd); err != nil {
		return unix.Stat_t{}, fmt.Errorf("close %s: %w", label, err)
	}
	return stat, nil
}

func openMountRoot(path, label string) (int, unix.Stat_t, error) {
	if !filepath.IsAbs(path) {
		return -1, unix.Stat_t{}, fmt.Errorf("%s path must be absolute", label)
	}
	fd, err := openDirectoryPathHandle(path)
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf("open %s: %w", label, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("inspect %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("%s must be a directory and not a symlink", label)
	}
	return fd, stat, nil
}

func validateConfiguredIdentity(cfg Config, identity RuntimeIdentity, server unix.Stat_t) (bool, error) {
	if cfg.PUID == nil && cfg.PGID == nil {
		if err := validateRuntimeIdentity(identity, false); err != nil {
			return false, err
		}
		if uint64(server.Uid) > maxRuntimeID || uint64(server.Gid) > maxRuntimeID {
			return false, fmt.Errorf("server mount owner uses the kernel no-change ID sentinel")
		}
		if identity.UID != int(server.Uid) || identity.GID != int(server.Gid) {
			return false, fmt.Errorf("runtime identity does not match server mount owner")
		}
		return false, nil
	}
	if cfg.PUID == nil || cfg.PGID == nil {
		return false, fmt.Errorf("PUID and PGID must be supplied together")
	}
	if err := validateRuntimeIdentity(identity, true); err != nil {
		return false, err
	}
	if err := validateRuntimeID(*cfg.PUID, "PUID", true); err != nil {
		return false, err
	}
	if err := validateRuntimeID(*cfg.PGID, "PGID", true); err != nil {
		return false, err
	}
	if identity.UID != *cfg.PUID || identity.GID != *cfg.PGID {
		return false, fmt.Errorf("runtime identity does not match PUID and PGID")
	}
	return true, nil
}

func validateRuntimeIdentity(identity RuntimeIdentity, explicit bool) error {
	if err := validateRuntimeID(identity.UID, "runtime UID", explicit); err != nil {
		return err
	}
	return validateRuntimeID(identity.GID, "runtime GID", explicit)
}

func validateRuntimeID(value int, label string, explicit bool) error {
	if value < 0 || uint64(value) > maxRuntimeID {
		return fmt.Errorf("%s must be between 0 and %d", label, maxRuntimeID)
	}
	if explicit && value == 0 {
		return fmt.Errorf("%s must be between 1 and %d", label, maxRuntimeID)
	}
	return nil
}

func lchownTree(rootPathFD int, rootStat unix.Stat_t, rootPath, mount string, identity RuntimeIdentity, hooks ownershipHooks) error {
	root, err := openHeldDirectory(rootPathFD, rootStat)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := unix.Fchown(root, identity.UID, identity.GID); err != nil {
		return err
	}
	if err := hooks.sync(mount, ".", root); err != nil {
		return err
	}
	if err := lchownDirectory(root, mount, ".", identity, hooks); err != nil {
		return err
	}
	return verifyPathIdentity(rootPath, rootStat)
}

func lchownDirectory(root int, mount, directoryName string, identity RuntimeIdentity, hooks ownershipHooks) error {
	copyFD, err := unix.Dup(root)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(copyFD), "ownership migration")
	names, err := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := unix.Openat(root, name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		var before unix.Stat_t
		if err := unix.Fstat(entry, &before); err != nil {
			unix.Close(entry)
			return err
		}
		if err := hooks.inspectEntry(mount, root, name, before); err != nil {
			unix.Close(entry)
			return err
		}
		err = migrateHeldEntry(root, entry, mount, name, before, identity, hooks)
		unix.Close(entry)
		if err != nil {
			return err
		}
	}
	return hooks.sync(mount, directoryName, root)
}

func migrateHeldEntry(parent, entry int, mount, name string, stat unix.Stat_t, identity RuntimeIdentity, hooks ownershipHooks) error {
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return migrateHeldDirectory(parent, entry, mount, name, stat, identity, hooks)
	case unix.S_IFREG:
		return migrateHeldRegular(parent, mount, name, stat, identity, hooks)
	case unix.S_IFLNK:
		if err := unix.Fchownat(entry, "", identity.UID, identity.GID, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if err := verifyNameIdentity(parent, name, stat); err != nil {
			return err
		}
		return hooks.sync(mount, filepath.Join(name, ".."), parent)
	default:
		if err := unix.Fchownat(entry, "", identity.UID, identity.GID, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if err := verifyNameIdentity(parent, name, stat); err != nil {
			return err
		}
		return hooks.sync(mount, filepath.Join(name, ".."), parent)
	}
}

func migrateHeldRegular(parent int, mount, name string, stat unix.Stat_t, identity RuntimeIdentity, hooks ownershipHooks) error {
	file, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(file)
	var opened unix.Stat_t
	if err := unix.Fstat(file, &opened); err != nil {
		return err
	}
	if !sameInode(stat, opened) || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("regular entry changed during ownership migration")
	}
	if err := hooks.openedEntry(mount, parent, name, file, opened); err != nil {
		return err
	}
	if err := unix.Fchown(file, identity.UID, identity.GID); err != nil {
		return err
	}
	if err := hooks.sync(mount, name, file); err != nil {
		return err
	}
	return verifyNameIdentity(parent, name, stat)
}

func migrateHeldDirectory(parent, entry int, mount, name string, stat unix.Stat_t, identity RuntimeIdentity, hooks ownershipHooks) error {
	directory, err := openHeldDirectory(entry, stat)
	if err != nil {
		return err
	}
	defer unix.Close(directory)
	if err := hooks.openedEntry(mount, parent, name, directory, stat); err != nil {
		return err
	}
	if err := unix.Fchown(directory, identity.UID, identity.GID); err != nil {
		return err
	}
	if err := hooks.sync(mount, name, directory); err != nil {
		return err
	}
	if err := verifyNameIdentity(parent, name, stat); err != nil {
		return err
	}
	if err := lchownDirectory(directory, mount, name, identity, hooks); err != nil {
		return err
	}
	return verifyNameIdentity(parent, name, stat)
}

func openHeldDirectory(pathFD int, want unix.Stat_t) (int, error) {
	directory, err := unix.Openat(pathFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(directory, &opened); err != nil {
		unix.Close(directory)
		return -1, err
	}
	if !sameInode(want, opened) || opened.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(directory)
		return -1, fmt.Errorf("directory handle changed during ownership migration")
	}
	return directory, nil
}

func openDirectoryPathHandle(path string) (int, error) {
	if !filepath.IsAbs(path) {
		return -1, fmt.Errorf("directory path must be absolute")
	}
	current, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range filepathComponents(path) {
		next, err := unix.Openat(current, component, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func filepathComponents(path string) []string {
	clean := filepath.Clean(path)
	if clean == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(clean, "/"), "/")
}

func sameInode(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}

func verifyNameIdentity(parent int, name string, want unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Fstatat(parent, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameInode(want, current) {
		return fmt.Errorf("entry changed during ownership migration")
	}
	return nil
}

func verifyPathIdentity(path string, want unix.Stat_t) error {
	current, err := openDirectoryPathHandle(path)
	if err != nil {
		return err
	}
	defer unix.Close(current)
	var stat unix.Stat_t
	if err := unix.Fstat(current, &stat); err != nil {
		return err
	}
	if !sameInode(want, stat) {
		return fmt.Errorf("mount root changed during ownership migration")
	}
	return nil
}

func preparePrivateDirectories(stateDir int, identity RuntimeIdentity) error {
	if err := preparePrivateDirectoryFD(stateDir, identity); err != nil {
		return fmt.Errorf("prepare StateDir: %w", err)
	}
	for _, name := range []string{"home", "wineprefix"} {
		if err := preparePrivateDirectoryAt(stateDir, name, identity, true); err != nil {
			return fmt.Errorf("prepare private directory %s: %w", name, err)
		}
	}
	for _, name := range []string{"generations", "cache", "backups"} {
		if err := preparePrivateDirectoryAt(stateDir, name, identity, false); err != nil {
			return fmt.Errorf("prepare private directory %s: %w", name, err)
		}
	}
	return nil
}

func preparePrivateDirectoryAt(parent int, name string, identity RuntimeIdentity, create bool) error {
	directory, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err := unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		if err := unix.Fsync(parent); err != nil {
			return err
		}
		directory, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		if !create && errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(directory)
	var stat unix.Stat_t
	if err := unix.Fstat(directory, &stat); err != nil {
		return err
	}
	if err := verifyNameIdentity(parent, name, stat); err != nil {
		return err
	}
	if err := preparePrivateDirectoryFD(directory, identity); err != nil {
		return err
	}
	return verifyNameIdentity(parent, name, stat)
}

func preparePrivateDirectoryFD(directory int, identity RuntimeIdentity) error {
	var stat unix.Stat_t
	if err := unix.Fstat(directory, &stat); err != nil {
		return err
	}
	if int(stat.Uid) != identity.UID || int(stat.Gid) != identity.GID {
		if err := unix.Fchown(directory, identity.UID, identity.GID); err != nil {
			return err
		}
	}
	if err := unix.Fchmod(directory, 0o700); err != nil {
		return err
	}
	return unix.Fsync(directory)
}

func ownershipMigrationComplete(root int, identity RuntimeIdentity) (bool, error) {
	var rootStat unix.Stat_t
	if err := unix.Fstat(root, &rootStat); err != nil {
		return false, err
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Mode&0o7777 != 0o700 || int(rootStat.Uid) != identity.UID || int(rootStat.Gid) != identity.GID {
		return false, nil
	}
	fd, err := unix.Openat(root, ownershipMarkerName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, nil
	}
	file := os.NewFile(uintptr(fd), ownershipMarkerName)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || int(stat.Uid) != identity.UID || int(stat.Gid) != identity.GID {
		return false, nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return false, err
	}
	var marker ownershipMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return false, nil
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return false, nil
	}
	return marker.UID == identity.UID && marker.GID == identity.GID, nil
}

func writeOwnershipMarker(root int, statePath string, stateStat unix.Stat_t, identity RuntimeIdentity, hooks ownershipHooks) error {
	data, err := json.Marshal(ownershipMarker{UID: identity.UID, GID: identity.GID})
	if err != nil {
		return err
	}
	temporaryName, fd, err := createTemporaryFileAt(root, ownershipMarkerName, 0o600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(root, temporaryName, 0)
	file := os.NewFile(uintptr(fd), temporaryName)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := unix.Fchown(fd, identity.UID, identity.GID); err != nil {
		file.Close()
		return err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := verifyPathIdentity(statePath, stateStat); err != nil {
		return fmt.Errorf("verify StateDir before ownership marker publication: %w", err)
	}
	if hooks.beforeMarkerRename != nil {
		if err := hooks.beforeMarkerRename(); err != nil {
			return err
		}
	}
	if err := unix.Renameat(root, temporaryName, root, ownershipMarkerName); err != nil {
		return err
	}
	if err := unix.Fsync(root); err != nil {
		return err
	}
	if err := verifyPathIdentity(statePath, stateStat); err != nil {
		return fmt.Errorf("verify StateDir after ownership marker publication: %w", err)
	}
	return nil
}

func probeWritableMount(path string) (returnErr error) {
	if _, err := inspectMountRoot(path, "mount"); err != nil {
		return err
	}
	root, err := openDirectoryPath(path, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	name, fd, err := createTemporaryFileAt(root, "writable-probe", 0o600)
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if removed {
			return
		}
		if err := unix.Unlinkat(root, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove probe after failure: %w", err))
		}
	}()
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write([]byte{0}); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Fsync(root); err != nil {
		return err
	}
	if err := unix.Unlinkat(root, name, 0); err != nil {
		return err
	}
	removed = true
	if err := unix.Fsync(root); err != nil {
		return err
	}
	return nil
}
