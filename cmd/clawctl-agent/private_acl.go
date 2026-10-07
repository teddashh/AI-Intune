package main

import "errors"

// Portable Windows NT values so credential policy can be tested off Windows.
// Numbers match winnt.h / golang.org/x/sys/windows.
const (
	windowsAccessAllowedACEType      = 0
	windowsAccessDeniedACEType       = 1
	windowsInheritOnlyACE            = 0x08
	windowsInheritedACE              = 0x10
	windowsGenericRead               = 0x80000000
	windowsGenericWrite              = 0x40000000
	windowsGenericAll                = 0x10000000
	windowsFileReadData              = 0x00000001
	windowsFileWriteData             = 0x00000002
	windowsFileAppendData            = 0x00000004
	windowsFileAttributeDirectory    = 0x00000010
	windowsFileAttributeReparsePoint = 0x00000400
)

var errPrivateRegularFile = errors.New("credential file must be a private regular file")

type windowsPrivateACE struct {
	Type  uint8
	Flags uint8
	Mask  uint32
	SID   string
}

type windowsPrivateACLView struct {
	OwnerSID      string
	DACLPresent   bool
	DACLProtected bool
	ACEs          []windowsPrivateACE
}

func privateFileModeNoticeFor(goos string) string {
	if goos == "windows" {
		return "current user read/write only"
	}
	return "0600"
}

func enrollmentStoredNotice(machineID, path, goos string) string {
	return "Enrolled. machine_id=" + machineID + "\nConfiguration written to " + path + " (" + privateFileModeNoticeFor(goos) + ")\n"
}

func windowsCredentialFileAttributesOK(attrs uint32) bool {
	return attrs&windowsFileAttributeDirectory == 0 && attrs&windowsFileAttributeReparsePoint == 0
}

func inspectWindowsCredentialHandle(attrs uint32, view windowsPrivateACLView, userSID string, requireWrite bool) error {
	if !windowsCredentialFileAttributesOK(attrs) {
		return errPrivateRegularFile
	}
	return windowsPrivateACLAllows(view, userSID, requireWrite)
}

func windowsPrivateACLAllows(view windowsPrivateACLView, userSID string, requireWrite bool) error {
	if userSID == "" || view.OwnerSID == "" || view.OwnerSID != userSID || !view.DACLPresent || !view.DACLProtected || len(view.ACEs) == 0 {
		return errPrivateRegularFile
	}
	var combined uint32
	for _, ace := range view.ACEs {
		if ace.Type != windowsAccessAllowedACEType || ace.Flags != 0 || ace.SID == "" || ace.SID != userSID {
			return errPrivateRegularFile
		}
		combined |= ace.Mask
	}
	if !windowsMaskGrantsRead(combined) {
		return errPrivateRegularFile
	}
	if requireWrite && !windowsMaskGrantsWrite(combined) {
		return errPrivateRegularFile
	}
	return nil
}

func windowsMaskGrantsRead(mask uint32) bool {
	return mask&windowsGenericAll != 0 || mask&windowsGenericRead != 0 || mask&windowsFileReadData != 0
}

func windowsMaskGrantsWrite(mask uint32) bool {
	return mask&windowsGenericAll != 0 || mask&windowsGenericWrite != 0 ||
		mask&windowsFileWriteData != 0 || mask&windowsFileAppendData != 0
}
