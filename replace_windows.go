//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// fileRenameInfo mirrors FILE_RENAME_INFO; in the Ex variant (FileRenameInfoEx) the
// ReplaceIfExists BOOLEAN becomes a DWORD Flags at the same offset.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  syscall.Handle
	FileNameLength uint32 // bytes, without the terminating NUL
	FileName       [1]uint16
}

const (
	fileRenameInfoEx            = 22  // FILE_INFO_BY_HANDLE_CLASS FileRenameInfoEx (Windows 10 1809+)
	fileRenameFlagsPosixReplace = 0x3 // FILE_RENAME_FLAG_REPLACE_IF_EXISTS | FILE_RENAME_FLAG_POSIX_SEMANTICS
	accessDelete                = 0x00010000
	accessSynchronize           = 0x00100000
	flagOpenReparsePoint        = 0x00200000

	errInvalidFunction    syscall.Errno = 1
	errNotSupported       syscall.Errno = 50
	errInvalidParameter   syscall.Errno = 87
	errCallNotImplemented syscall.Errno = 120
)

var procSetFileInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

// posixRenameOff is set once the POSIX rename proved unsupported (pre-1809 Windows, FAT, some
// network shares): later renames go straight to MoveFileEx. Tests set it to force the fallback.
var posixRenameOff atomic.Bool

// replaceFile renames tmp over path. MoveFileEx(REPLACE_EXISTING) (os.Rename) cannot replace a
// target that another process holds open, even with FILE_SHARE_DELETE ("Access is denied").
// The POSIX rename (SetFileInformationByHandle, FileRenameInfoEx, REPLACE_IF_EXISTS|POSIX_SEMANTICS)
// can: open readers keep the old contents and the name points at the new file at once.
// When the filesystem or OS does not support it, fall back to os.Rename.
func replaceFile(tmp, path string) error {
	if posixRenameOff.Load() {
		return os.Rename(tmp, path)
	}
	err := posixRename(tmp, path)
	switch {
	case err == nil || errors.Is(err, fs.ErrNotExist):
		return err
	case posixUnsupported(err):
		posixRenameOff.Store(true)
		return os.Rename(tmp, path)
	}
	// any other failure (a reader without FILE_SHARE_DELETE, a scan): MoveFileEx is never worse,
	// and the caller's retry loop waits out what both refuse
	if os.Rename(tmp, path) == nil {
		return nil
	}
	return err
}

func posixUnsupported(err error) bool {
	return errors.Is(err, errInvalidParameter) || errors.Is(err, errNotSupported) ||
		errors.Is(err, errInvalidFunction) || errors.Is(err, errCallNotImplemented)
}

func posixRename(tmp, path string) error {
	lerr := func(err error) error { return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: err} }
	if err := procSetFileInformationByHandle.Find(); err != nil {
		return lerr(errCallNotImplemented)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return lerr(err)
	}
	src, err := syscall.UTF16PtrFromString(tmp)
	if err != nil {
		return lerr(err)
	}
	name, err := syscall.UTF16FromString(abs)
	if err != nil {
		return lerr(err)
	}
	h, err := syscall.CreateFile(src, accessDelete|accessSynchronize,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, flagOpenReparsePoint, 0)
	if err != nil {
		return lerr(err)
	}
	defer syscall.CloseHandle(h)
	off := unsafe.Offsetof(fileRenameInfo{}.FileName)
	size := off + uintptr(len(name))*2 // name includes the NUL
	buf := make([]uint64, (size+7)/8)  // 8-byte aligned for the HANDLE field
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = fileRenameFlagsPosixReplace
	info.FileNameLength = uint32(len(name)-1) * 2
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	r, _, e := procSetFileInformationByHandle.Call(uintptr(h), fileRenameInfoEx, uintptr(unsafe.Pointer(info)), size)
	if r == 0 {
		if e == nil || e == syscall.Errno(0) {
			e = syscall.EINVAL
		}
		return lerr(e)
	}
	return nil
}
