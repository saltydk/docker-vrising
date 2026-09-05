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

	"golang.org/x/sys/unix"
)

const ownershipMarkerName = "ownership.json"

type RuntimeIdentity struct {
	UID  int
	GID  int
	Home string
}

type ownershipMarker struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

func ResolveIdentity(cfg Config) (RuntimeIdentity, error) {
	server, err := inspectMountRoot(cfg.ServerDir, "server mount")
	if err != nil {
		return RuntimeIdentity{}, err
	}
	if _, err := inspectMountRoot(cfg.DataDir, "persistent-data mount"); err != nil {
		return RuntimeIdentity{}, err
	}
	if !filepath.IsAbs(cfg.StateDir) {
		return RuntimeIdentity{}, fmt.Errorf("state directory must be absolute")
	}

	identity := RuntimeIdentity{Home: filepath.Join(cfg.StateDir, "home")}
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
	return identity, nil
}

func PrepareOwnership(cfg Config, identity RuntimeIdentity) error {
	server, err := inspectMountRoot(cfg.ServerDir, "server mount")
	if err != nil {
		return err
	}
	if _, err := inspectMountRoot(cfg.DataDir, "persistent-data mount"); err != nil {
		return err
	}
	if identity.Home != filepath.Join(cfg.StateDir, "home") {
		return fmt.Errorf("runtime home must be StateDir/home")
	}

	explicit, err := validateConfiguredIdentity(cfg, identity, server)
	if err != nil {
		return err
	}
	if !explicit {
		return preparePrivateDirectories(cfg, identity)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("explicit ownership migration requires root")
	}

	complete, err := ownershipMigrationComplete(cfg.StateDir, identity)
	if err != nil {
		return err
	}
	if complete {
		return preparePrivateDirectories(cfg, identity)
	}
	log.Printf("ownership migration: changing server and persistent-data mounts to UID/GID %d:%d", identity.UID, identity.GID)
	if err := lchownTree(cfg.ServerDir, identity); err != nil {
		return fmt.Errorf("migrate server mount ownership: %w", err)
	}
	if err := lchownTree(cfg.DataDir, identity); err != nil {
		return fmt.Errorf("migrate persistent-data mount ownership: %w", err)
	}
	if err := preparePrivateDirectories(cfg, identity); err != nil {
		return err
	}
	if err := writeOwnershipMarker(cfg.StateDir, identity); err != nil {
		return fmt.Errorf("record ownership migration: %w", err)
	}
	return nil
}

func DropPrivileges(identity RuntimeIdentity) error {
	if identity.UID < 0 || identity.GID < 0 {
		return fmt.Errorf("runtime identity must use non-negative numeric IDs")
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
	var stat unix.Stat_t
	if !filepath.IsAbs(path) {
		return stat, fmt.Errorf("%s path must be absolute", label)
	}
	if err := unix.Lstat(path, &stat); err != nil {
		return stat, fmt.Errorf("inspect %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return stat, fmt.Errorf("%s must be a directory and not a symlink", label)
	}
	return stat, nil
}

func validateConfiguredIdentity(cfg Config, identity RuntimeIdentity, server unix.Stat_t) (bool, error) {
	if cfg.PUID == nil && cfg.PGID == nil {
		if identity.UID != int(server.Uid) || identity.GID != int(server.Gid) {
			return false, fmt.Errorf("runtime identity does not match server mount owner")
		}
		return false, nil
	}
	if cfg.PUID == nil || cfg.PGID == nil {
		return false, fmt.Errorf("PUID and PGID must be supplied together")
	}
	if *cfg.PUID <= 0 || *cfg.PGID <= 0 {
		return false, fmt.Errorf("explicit runtime IDs must be positive")
	}
	if identity.UID != *cfg.PUID || identity.GID != *cfg.PGID {
		return false, fmt.Errorf("runtime identity does not match PUID and PGID")
	}
	return true, nil
}

func lchownTree(rootPath string, identity RuntimeIdentity) error {
	root, err := openDirectoryPath(rootPath, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := unix.Fchown(root, identity.UID, identity.GID); err != nil {
		return err
	}
	return lchownDirectory(root, identity)
}

func lchownDirectory(root int, identity RuntimeIdentity) error {
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
		var before unix.Stat_t
		if err := unix.Fstatat(root, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if err := unix.Fchownat(root, name, identity.UID, identity.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if before.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		child, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		var opened unix.Stat_t
		if err := unix.Fstat(child, &opened); err != nil {
			unix.Close(child)
			return err
		}
		if before.Dev != opened.Dev || before.Ino != opened.Ino {
			unix.Close(child)
			return fmt.Errorf("directory entry changed during ownership migration")
		}
		if err := lchownDirectory(child, identity); err != nil {
			unix.Close(child)
			return err
		}
		if err := unix.Close(child); err != nil {
			return err
		}
	}
	return unix.Fsync(root)
}

func preparePrivateDirectories(cfg Config, identity RuntimeIdentity) error {
	for _, directory := range []string{cfg.StateDir, identity.Home, filepath.Join(cfg.StateDir, "wineprefix")} {
		if err := preparePrivateDirectory(directory, identity, true); err != nil {
			return fmt.Errorf("prepare private directory %s: %w", directory, err)
		}
	}
	for _, name := range []string{"generations", "cache", "backups"} {
		directory := filepath.Join(cfg.StateDir, name)
		if err := preparePrivateDirectory(directory, identity, false); err != nil {
			return fmt.Errorf("prepare private directory %s: %w", directory, err)
		}
	}
	return nil
}

func preparePrivateDirectory(path string, identity RuntimeIdentity, create bool) error {
	directory, err := openDirectoryPath(path, create)
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

func ownershipMigrationComplete(stateDir string, identity RuntimeIdentity) (bool, error) {
	root, err := openDirectoryPath(stateDir, false)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open ownership marker directory: %w", err)
	}
	defer unix.Close(root)
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

func writeOwnershipMarker(stateDir string, identity RuntimeIdentity) error {
	data, err := json.Marshal(ownershipMarker{UID: identity.UID, GID: identity.GID})
	if err != nil {
		return err
	}
	root, err := openDirectoryPath(stateDir, false)
	if err != nil {
		return err
	}
	defer unix.Close(root)
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
	if err := unix.Renameat(root, temporaryName, root, ownershipMarkerName); err != nil {
		return err
	}
	return unix.Fsync(root)
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
