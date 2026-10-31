//go:build windows

package secret

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// machineScope switches DPAPI to the machine key, so a password saved by an administrator
// during service install can be read by the service running as LocalSystem.
var machineScope bool

// UseMachineScope selects machine-scope DPAPI for subsequent Set calls. Get reads either.
func UseMachineScope(on bool) { machineScope = on }

var entropy = []byte(service)

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func takeBlob(out *windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
}

func set(email, password string) error {
	flags := uint32(windows.CRYPTPROTECT_UI_FORBIDDEN)
	if machineScope {
		flags |= windows.CRYPTPROTECT_LOCAL_MACHINE
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(blob([]byte(password)), nil, blob(entropy), 0, nil, flags, &out); err != nil {
		return fmt.Errorf("encrypt password: %w", err)
	}
	return writeFile(key(email)+".dpapi", takeBlob(&out))
}

func get(email string) (string, error) {
	enc, err := readFile(key(email) + ".dpapi")
	if err != nil {
		return "", err
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(enc), nil, blob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", fmt.Errorf("decrypt saved password (saved by another Windows account?): %w", err)
	}
	return string(takeBlob(&out)), nil
}

func del(email string) error { return removeFile(key(email) + ".dpapi") }

func backend() string {
	if machineScope {
		return "Windows DPAPI (machine scope)"
	}
	return "Windows DPAPI"
}
