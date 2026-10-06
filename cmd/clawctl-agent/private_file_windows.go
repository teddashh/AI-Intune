//go:build windows

package main

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

func readPrivateRegularFile(path string) ([]byte, error) {
	file, err := openWindowsCredentialFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, windows.OPEN_EXISTING)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := verifyWindowsCredentialFile(file, false); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func writePrivateFile(path string, body []byte) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, tmpPath, err := createWindowsPrivateTemp(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING); err != nil {
		return err
	}
	file, err := openWindowsCredentialFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, windows.OPEN_EXISTING)
	if err != nil {
		return err
	}
	defer file.Close()
	return verifyWindowsCredentialFile(file, true)
}

func ensurePrivateDir(path string) error {
	if path == "" || path == "." || path == filepath.VolumeName(path) {
		return nil
	}
	cleaned := filepath.Clean(path)
	if cleaned == filepath.Dir(cleaned) {
		return nil
	}
	attrs, err := windowsPathAttributes(cleaned)
	if err == nil {
		if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errPrivateRegularFile
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := ensurePrivateDir(filepath.Dir(cleaned)); err != nil {
		return err
	}
	return createWindowsPrivateDir(cleaned)
}

func createWindowsPrivateDir(path string) error {
	sec, err := newWindowsOwnerOnlySecurity()
	if err != nil {
		return err
	}
	defer sec.close()
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(pathp, &sec.sa)
	if err == nil {
		return applyWindowsOwnerOnlyACLByPath(path, true, sec)
	}
	if err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	attrs, statErr := windowsPathAttributes(path)
	if statErr != nil {
		return statErr
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errPrivateRegularFile
	}
	return nil
}

func createWindowsPrivateTemp(dir string) (*os.File, string, error) {
	sec, err := newWindowsOwnerOnlySecurity()
	if err != nil {
		return nil, "", err
	}
	defer sec.close()
	for i := 0; i < 10000; i++ {
		var n uint64
		if err := binary.Read(rand.Reader, binary.LittleEndian, &n); err != nil {
			return nil, "", err
		}
		tmpPath := filepath.Join(dir, ".clawctl-private-"+strconv.FormatUint(n, 10))
		file, err := createWindowsPrivateFile(tmpPath, sec)
		if err == nil {
			if vErr := verifyWindowsCredentialFile(file, true); vErr != nil {
				_ = file.Close()
				_ = os.Remove(tmpPath)
				return nil, "", vErr
			}
			return file, tmpPath, nil
		}
		if err != windows.ERROR_FILE_EXISTS && err != windows.ERROR_ALREADY_EXISTS {
			return nil, "", err
		}
	}
	return nil, "", errPrivateRegularFile
}

func createWindowsPrivateFile(path string, sec *windowsOwnerOnlySecurity) (*os.File, error) {
	file, err := openWindowsCredentialFileWithSecurity(path, windows.GENERIC_ALL,
		0, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, &sec.sa)
	if err != nil {
		return nil, err
	}
	if err := applyWindowsOwnerOnlyACL(windows.Handle(file.Fd()), sec); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

func applyWindowsOwnerOnlyACLByPath(path string, dir bool, sec *windowsOwnerOnlySecurity) error {
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if dir {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(pathp, windows.WRITE_DAC|windows.WRITE_OWNER|windows.READ_CONTROL,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return applyWindowsOwnerOnlyACL(handle, sec)
}

func applyWindowsOwnerOnlyACL(handle windows.Handle, sec *windowsOwnerOnlySecurity) error {
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		sec.sid, nil, sec.acl, nil)
}

func openWindowsCredentialFile(path string, access, share, disposition uint32) (*os.File, error) {
	return openWindowsCredentialFileWithSecurity(path, access, share, disposition,
		windows.FILE_FLAG_OPEN_REPARSE_POINT, nil)
}

func openWindowsCredentialFileWithSecurity(path string, access, share, disposition, attrs uint32, sa *windows.SecurityAttributes) (*os.File, error) {
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pathp, access, share, sa, disposition, attrs, 0)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errPrivateRegularFile
	}
	return file, nil
}

func verifyWindowsCredentialFile(file *os.File, requireWrite bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	view, userSID, err := windowsCredentialACLView(windows.Handle(file.Fd()))
	if err != nil {
		return err
	}
	return inspectWindowsCredentialHandle(info.FileAttributes, view, userSID, requireWrite)
}

func windowsPathAttributes(path string) (uint32, error) {
	file, err := openWindowsCredentialFile(path, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.OPEN_EXISTING)
	if err != nil {
		// Directories need backup semantics; retry so a junction is still opened, not followed.
		pathp, pErr := windows.UTF16PtrFromString(path)
		if pErr != nil {
			return 0, err
		}
		handle, openErr := windows.CreateFile(pathp, windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if openErr != nil {
			if openErr == windows.ERROR_FILE_NOT_FOUND || openErr == windows.ERROR_PATH_NOT_FOUND {
				return 0, os.ErrNotExist
			}
			return 0, openErr
		}
		defer windows.CloseHandle(handle)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return 0, err
		}
		return info.FileAttributes, nil
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, err
	}
	return info.FileAttributes, nil
}

func windowsCredentialACLView(handle windows.Handle) (windowsPrivateACLView, string, error) {
	userSID, err := currentUserSIDString()
	if err != nil {
		return windowsPrivateACLView{}, "", err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return windowsPrivateACLView{}, "", errPrivateRegularFile
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return windowsPrivateACLView{}, "", errPrivateRegularFile
	}
	control, _, err := sd.Control()
	if err != nil {
		return windowsPrivateACLView{}, "", errPrivateRegularFile
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return windowsPrivateACLView{}, "", errPrivateRegularFile
	}
	view := windowsPrivateACLView{
		OwnerSID:      owner.String(),
		DACLPresent:   true,
		DACLProtected: control&windows.SE_DACL_PROTECTED != 0,
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return windowsPrivateACLView{}, "", errPrivateRegularFile
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		view.ACEs = append(view.ACEs, windowsPrivateACE{
			Type:  ace.Header.AceType,
			Flags: ace.Header.AceFlags,
			Mask:  uint32(ace.Mask),
			SID:   sid.String(),
		})
	}
	return view, userSID, nil
}

func currentUserSIDString() (string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	text := sid.String()
	if text == "" {
		return "", errPrivateRegularFile
	}
	return text, nil
}

func currentUserSID() (*windows.SID, error) {
	tokenUser, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return tokenUser.User.Sid.Copy()
}

type windowsOwnerOnlySecurity struct {
	sa     windows.SecurityAttributes
	sd     *windows.SECURITY_DESCRIPTOR
	acl    *windows.ACL
	sid    *windows.SID
	pinner runtime.Pinner
}

func (s *windowsOwnerOnlySecurity) close() {
	s.pinner.Unpin()
}

func newWindowsOwnerOnlySecurity() (*windowsOwnerOnlySecurity, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sec := &windowsOwnerOnlySecurity{sid: sid}
	sec.pinner.Pin(sid)
	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		sec.close()
		return nil, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		sec.close()
		return nil, err
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		sec.close()
		return nil, err
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		sec.close()
		return nil, err
	}
	if err := sd.SetOwner(sid, false); err != nil {
		sec.close()
		return nil, err
	}
	sec.acl = acl
	sec.sd = sd
	sec.sa = windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	return sec, nil
}
