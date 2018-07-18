// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/singularityware/singularity/src/pkg/buildcfg"
	"github.com/singularityware/singularity/src/pkg/image"
	"github.com/singularityware/singularity/src/pkg/sylog"
	"github.com/singularityware/singularity/src/pkg/util/fs/files"
	"github.com/singularityware/singularity/src/pkg/util/fs/layout"
	"github.com/singularityware/singularity/src/pkg/util/fs/layout/layer/overlay"
	"github.com/singularityware/singularity/src/pkg/util/fs/layout/layer/underlay"
	"github.com/singularityware/singularity/src/pkg/util/fs/mount"
	"github.com/singularityware/singularity/src/pkg/util/fs/proc"
	"github.com/singularityware/singularity/src/pkg/util/loop"
	"github.com/singularityware/singularity/src/runtime/engines/singularity/rpc/client"
	"github.com/sylabs/sif/pkg/sif"
)

type container struct {
	engine  *EngineOperations
	rpcOps  *client.RPC
	session *layout.Session
	userNS  bool
	pidNS   bool
}

func create(engine *EngineOperations, rpcOps *client.RPC) error {
	var err error

	sessionFsType := engine.EngineConfig.File.MemoryFSType
	sessionSize := -1

	if os.Geteuid() != 0 {
		sessionSize = int(engine.EngineConfig.File.SessiondirMaxSize)
	}

	c := &container{engine: engine, rpcOps: rpcOps}

	p := &mount.Points{}
	system := &mount.System{Points: p, Mount: c.localMount}

	if enabled, _ := proc.HasFilesystem("overlay"); enabled {
		switch engine.EngineConfig.File.EnableOverlay {
		case "yes", "try":
			if c.session, err = layout.NewSession(buildcfg.SESSIONDIR, sessionFsType, sessionSize, system, overlay.New()); err != nil {
				return err
			}
			if err := c.addOverlayMount(system); err != nil {
				return err
			}
			if err := system.RunAfterTag(mount.LayerTag, c.switchMount); err != nil {
				return err
			}
		}
	}
	if c.session == nil {
		if engine.EngineConfig.File.EnableUnderlay {
			if c.session, err = layout.NewSession(buildcfg.SESSIONDIR, sessionFsType, sessionSize, system, underlay.New()); err != nil {
				return err
			}
		} else {
			if c.session, err = layout.NewSession(buildcfg.SESSIONDIR, sessionFsType, sessionSize, system, nil); err != nil {
				return err
			}
		}
		if err := system.RunAfterTag(mount.RootfsTag, c.switchMount); err != nil {
			return err
		}
	}

	if err := system.RunAfterTag(mount.RootfsTag, c.addFilesMount); err != nil {
		return err
	}

	if engine.CommonConfig.OciConfig.Linux != nil {
		for _, namespace := range engine.CommonConfig.OciConfig.Linux.Namespaces {
			switch namespace.Type {
			case specs.UserNamespace:
				c.userNS = true
			case specs.PIDNamespace:
				c.pidNS = true
			}
		}
	}

	if err := c.addRootfsMount(system); err != nil {
		return err
	}
	if err := c.addKernelMount(system); err != nil {
		return err
	}
	if err := c.addDevMount(system); err != nil {
		return err
	}
	if err := c.addHostMount(system); err != nil {
		return err
	}
	if err := c.addBindsMount(system); err != nil {
		return err
	}
	if err := c.addHomeMount(system); err != nil {
		return err
	}
	if err := c.addUserbindsMount(system); err != nil {
		return err
	}
	if err := c.addTmpMount(system); err != nil {
		return err
	}
	if err := c.addScratchMount(system); err != nil {
		return err
	}
	if err := c.addCwdMount(system); err != nil {
		return err
	}
	if err := c.addLibsMount(system); err != nil {
		return err
	}

	sylog.Debugf("Mount all")
	if err := system.MountAll(); err != nil {
		return err
	}

	sylog.Debugf("Chroot into %s\n", c.session.FinalPath())
	_, err = c.rpcOps.Chroot(c.session.FinalPath())
	if err != nil {
		return fmt.Errorf("chroot failed: %s", err)
	}

	sylog.Debugf("Chdir into / to avoid errors\n")
	err = syscall.Chdir("/")
	if err != nil {
		return fmt.Errorf("change directory failed: %s", err)
	}

	return nil
}

func (c *container) localMount(point *mount.Point) error {
	uid := os.Getuid()

	if err := syscall.Setresuid(uid, 0, uid); err != nil {
		return fmt.Errorf("failed to elevate privileges")
	}
	defer syscall.Setresuid(uid, uid, 0)

	if _, err := mount.GetOffset(point.InternalOptions); err == nil {
		if err := c.mountImage(point); err != nil {
			return fmt.Errorf("can't mount image %s: %s", point.Source, err)
		}
	} else {
		if err := c.mountGeneric(point, true); err != nil {
			sylog.Verbosef("can't mount %s: %s", point.Source, err)
			return nil
		}
	}
	return nil
}

func (c *container) rpcMount(point *mount.Point) error {
	if err := c.mountGeneric(point, false); err != nil {
		sylog.Verbosef("can't mount %s: %s", point.Source, err)
		return nil
	}
	return nil
}

func (c *container) switchMount(system *mount.System) error {
	system.Mount = c.rpcMount
	return nil
}

// mount any generic mount (not loop dev)
func (c *container) mountGeneric(mnt *mount.Point, local bool) (err error) {
	flags, opts := mount.ConvertOptions(mnt.Options)
	optsString := strings.Join(opts, ",")
	sessionPath, _ := c.session.GetPath("/")

	if flags&syscall.MS_BIND != 0 && flags&syscall.MS_REMOUNT == 0 {
		if _, err := os.Stat(mnt.Source); os.IsNotExist(err) {
			sylog.Debugf("Skipping mount, host source %s doesn't exist", mnt.Source)
			return nil
		}
	}

	dest := ""
	if !strings.HasPrefix(mnt.Destination, sessionPath) {
		dest = c.session.FinalPath() + mnt.Destination
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			sylog.Debugf("Skipping mount, %s doesn't exist in container", dest)
			return nil
		}
	} else {
		dest = mnt.Destination
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return fmt.Errorf("destination %s doesn't exist", dest)
		}
	}

	if mnt.Source != "" {
		sylog.Debugf("Mounting %s to %s\n", mnt.Source, dest)
	}
	if !local {
		_, err = c.rpcOps.Mount(mnt.Source, dest, mnt.Type, flags, optsString)
	} else {
		err = syscall.Mount(mnt.Source, dest, mnt.Type, flags, optsString)
	}
	return err
}

// mount image via loop
func (c *container) mountImage(mnt *mount.Point) error {
	flags, opts := mount.ConvertOptions(mnt.Options)
	optsString := strings.Join(opts, ",")

	offset, err := mount.GetOffset(mnt.InternalOptions)
	if err != nil {
		return err
	}

	sizelimit, err := mount.GetSizeLimit(mnt.InternalOptions)
	if err != nil {
		return err
	}

	attachFlag := os.O_RDWR
	loopFlags := uint32(loop.FlagsAutoClear)

	if flags&syscall.MS_RDONLY == 1 {
		loopFlags |= loop.FlagsReadOnly
		attachFlag = os.O_RDONLY
	}

	info := &loop.Info64{
		Offset:    offset,
		SizeLimit: sizelimit,
		Flags:     loopFlags,
	}

	loopdev := new(loop.Device)

	number := 0

	if err := loopdev.Attach(mnt.Source, attachFlag, &number); err != nil {
		return err
	}
	if err := loopdev.SetStatus(info); err != nil {
		return err
	}

	path := fmt.Sprintf("/dev/loop%d", number)
	sylog.Debugf("Mounting loop device %s to %s\n", path, mnt.Destination)
	err = syscall.Mount(path, mnt.Destination, mnt.Type, flags, optsString)
	if err != nil {
		return fmt.Errorf("failed to mount %s filesystem: %s", mnt.Type, err)
	}

	return nil
}

func (c *container) addRootfsMount(system *mount.System) error {
	var flags uintptr = syscall.MS_NOSUID | syscall.MS_RDONLY | syscall.MS_NODEV
	rootfs := c.engine.EngineConfig.GetImage()

	imageObject, err := image.Init(rootfs, false)
	if err != nil {
		return err
	}

	mountType := ""

	switch imageObject.Type {
	case image.SIF:
		// Load the SIF file
		fimg, err := sif.LoadContainerFp(imageObject.File, !imageObject.Writable)
		if err != nil {
			return err
		}

		// Get the default system partition image
		part, _, err := fimg.GetPartFromGroup(sif.DescrDefaultGroup)
		if err != nil {
			return err
		}

		// Check that this is a system partition
		parttype, err := part.GetPartType()
		if err != nil {
			return err
		}
		if parttype != sif.PartSystem {
			return fmt.Errorf("found partition is not system")
		}

		// record the fs type
		fstype, err := part.GetFsType()
		if err != nil {
			return err
		}
		if fstype == sif.FsSquash {
			mountType = "squashfs"
		} else if fstype == sif.FsExt3 {
			mountType = "ext3"
		} else {
			return fmt.Errorf("unknown file system type: %v", fstype)
		}

		imageObject.Offset = uint64(part.Fileoff)
		imageObject.Size = uint64(part.Filelen)
	case image.SQUASHFS:
		mountType = "squashfs"
	case image.EXT3:
		mountType = "ext3"
	case image.SANDBOX:
		sylog.Debugf("Mounting directory rootfs: %v\n", rootfs)
		return system.Points.AddBind(mount.RootfsTag, rootfs, c.session.RootFsPath(), syscall.MS_BIND|flags)
	}

	src := fmt.Sprintf("/proc/self/fd/%d", imageObject.File.Fd())
	sylog.Debugf("Mounting block [%v] image: %v\n", mountType, rootfs)
	return system.Points.AddImage(mount.RootfsTag, src, c.session.RootFsPath(), mountType, flags, imageObject.Offset, imageObject.Size)
}

func (c *container) addOverlayMount(system *mount.System) error {
	nb := 0
	writable := true

	for _, img := range c.engine.EngineConfig.GetOverlayImage() {
		imageObject, err := image.Init(img, writable)
		if err != nil {
			writable = !writable
		}
		if !writable {
			imageObject, err = image.Init(img, writable)
		}
		sessionDest := fmt.Sprintf("/overlay-images/%d", nb)
		if err := c.session.AddDir(sessionDest); err != nil {
			return fmt.Errorf("failed to create session directory for overlay: %s", err)
		}
		dst, _ := c.session.GetPath(sessionDest)
		nb++

		src := fmt.Sprintf("/proc/self/fd/%d", imageObject.File.Fd())
		switch imageObject.Type {
		case image.EXT3:
			if !writable {
				return fmt.Errorf("you don't have write permission for %s", img)
			}
			flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
			err = system.Points.AddImage(mount.PreLayerTag, src, dst, "ext3", flags, imageObject.Offset, imageObject.Size)
			if err != nil {
				return err
			}
		case image.SQUASHFS:
			flags := uintptr(syscall.MS_NOSUID | syscall.MS_RDONLY | syscall.MS_NODEV)
			err = system.Points.AddImage(mount.PreLayerTag, src, dst, "squashfs", flags, imageObject.Offset, imageObject.Size)
			if err != nil {
				return err
			}
		case image.SANDBOX:
			if os.Geteuid() != 0 {
				return fmt.Errorf("only root user can use sandbox as overlay")
			}
			if !writable {
				return fmt.Errorf("you don't have write permission for %s", img)
			}
			flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
			err = system.Points.AddBind(mount.PreLayerTag, src, dst, flags)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unkown image format")
		}
	}
	return nil
}

func (c *container) addKernelMount(system *mount.System) error {
	var err error
	bindFlags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	sylog.Debugf("Adding proc to mount list\n")
	if c.pidNS {
		err = system.Points.AddFS(mount.KernelTag, "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV, "")
	} else {
		err = system.Points.AddBind(mount.KernelTag, "/proc", "/proc", bindFlags)
		if err == nil {
			system.Points.AddRemount(mount.KernelTag, "/proc", bindFlags)
		}
	}
	if err != nil {
		return fmt.Errorf("unable to add proc to mount list: %s", err)
	}

	sylog.Debugf("Adding sysfs to mount list\n")
	if !c.userNS {
		err = system.Points.AddFS(mount.KernelTag, "/sys", "sysfs", syscall.MS_NOSUID|syscall.MS_NODEV, "")
	} else {
		err = system.Points.AddBind(mount.KernelTag, "/sys", "/sys", bindFlags)
		if err == nil {
			system.Points.AddRemount(mount.KernelTag, "/sys", bindFlags)
		}
	}
	if err != nil {
		return fmt.Errorf("unable to add sys to mount list: %s", err)
	}
	return nil
}

func (c *container) addDevMount(system *mount.System) error {
	sylog.Debugf("Adding dev to mount list\n")
	err := system.Points.AddBind(mount.DevTag, "/dev", "/dev", syscall.MS_BIND|syscall.MS_NOSUID|syscall.MS_REC)
	if err != nil {
		return fmt.Errorf("unable to add dev to mount list: %s", err)
	}
	return nil
}

func (c *container) addHostMount(system *mount.System) error {
	info, err := proc.ParseMountInfo("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)
	for _, child := range info["/"] {
		switch child {
		case "/proc", "/sys", "/dev", "/run":
			continue
		default:
			sylog.Debugf("Adding %s to mount list\n", child)
			if err := system.Points.AddBind(mount.HostfsTag, child, child, flags); err != nil {
				return fmt.Errorf("unable to add %s to mount list: %s", child, err)
			}
			system.Points.AddRemount(mount.HostfsTag, child, flags)
		}
	}
	return nil
}

func (c *container) addBindsMount(system *mount.System) error {
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	for _, bindpath := range c.engine.EngineConfig.File.BindPath {
		splitted := strings.Split(bindpath, ":")
		src := ""
		dst := splitted[0]
		if len(splitted) > 1 {
			src = splitted[1]
		} else {
			src = dst
		}
		err := system.Points.AddBind(mount.BindsTag, src, dst, flags)
		if err != nil {
			return fmt.Errorf("unable to add %s to mount list: %s", src, err)
		}
	}

	return nil
}

func (c *container) addHomeMount(system *mount.System) error {
	sylog.Debugf("Adding home to mount list\n")
	err := system.Points.AddBind(mount.HomeTag, "/home", "/home", syscall.MS_BIND)
	if err != nil {
		return fmt.Errorf("unable to add home to mount list: %s", err)
	}
	return nil
}

func (c *container) addUserbindsMount(system *mount.System) error {
	flags := uintptr(syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_REC)

	for _, b := range c.engine.EngineConfig.GetBindPath() {
		splitted := strings.Split(b, ":")

		src := splitted[0]
		dst := src
		if len(splitted) > 1 {
			dst = splitted[1]
		}
		if len(splitted) > 2 {
			if splitted[2] == "ro" {
				flags |= syscall.MS_RDONLY
			} else if splitted[2] != "rw" {
				sylog.Warningf("Not mounting requested %s bind point, invalid mount option %s", src, splitted[2])
			}
		}

		sylog.Debugf("Adding %s to mount list\n", src)
		if err := system.Points.AddBind(mount.UserbindsTag, src, dst, flags); err != nil {
			return fmt.Errorf("unabled to %s to mount list: %s", src, err)
		}
		system.Points.AddRemount(mount.UserbindsTag, dst, flags)
	}
	return nil
}

func (c *container) addTmpMount(system *mount.System) error {
	return nil
}

func (c *container) addScratchMount(system *mount.System) error {
	return nil
}

func (c *container) addCwdMount(system *mount.System) error {
	return nil
}

func (c *container) addLibsMount(system *mount.System) error {
	return nil
}

func (c *container) addFilesMount(system *mount.System) error {
	rootfs := c.session.RootFsPath()

	if os.Geteuid() == 0 {
		sylog.Verbosef("Not updating passwd/group files, running as root!")
		return nil
	}

	if c.engine.EngineConfig.File.ConfigPasswd {
		passwd := filepath.Join(rootfs, "/etc/passwd")
		content, err := files.Passwd(passwd, "")
		if err != nil {
			sylog.Warningf("%s", err)
		} else {
			if err := c.session.AddFile("/etc/passwd", content); err != nil {
				sylog.Warningf("failed to add passwd session file: %s", err)
			}
			passwd, _ = c.session.GetPath("/etc/passwd")

			sylog.Debugf("Adding /etc/passwd to mount list\n")
			err = system.Points.AddBind(mount.FilesTag, passwd, "/etc/passwd", syscall.MS_BIND)
			if err != nil {
				return fmt.Errorf("unable to add /etc/passwd to mount list: %s", err)
			}
		}
	} else {
		sylog.Verbosef("Skipping bind of the host's /etc/passwd")
	}

	if c.engine.EngineConfig.File.ConfigGroup {
		group := filepath.Join(rootfs, "/etc/group")
		content, err := files.Group(group)
		if err != nil {
			sylog.Warningf("%s", err)
		} else {
			if err := c.session.AddFile("/etc/group", content); err != nil {
				sylog.Warningf("failed to add group session file: %s", err)
			}
			group, _ = c.session.GetPath("/etc/group")

			sylog.Debugf("Adding /etc/group to mount list\n")
			err = system.Points.AddBind(mount.FilesTag, group, "/etc/group", syscall.MS_BIND)
			if err != nil {
				return fmt.Errorf("unable to add /etc/group to mount list: %s", err)
			}
		}
	} else {
		sylog.Verbosef("Skipping bind of the host's /etc/group")
	}

	return nil
}
