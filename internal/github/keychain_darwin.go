package github

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The Security framework is called directly rather than through the
// security command, so the Keychain checks ebi-x's own executable against
// the item's access list: an item that trusts only ebi-x stays closed to
// the commands Codex runs, security included. purego keeps the build free
// of cgo.
var (
	securityOnce            sync.Once
	securityErr             error
	findGenericPassword     func(keychain uintptr, serviceLength uint32, service *byte, accountLength uint32, account *byte, passwordLength *uint32, passwordData *unsafe.Pointer, item uintptr) int32
	freeKeychainItemContent func(attributes uintptr, data unsafe.Pointer) int32
)

const securityFramework = "/System/Library/Frameworks/Security.framework/Security"

func loadSecurity() error {
	securityOnce.Do(func() {
		lib, err := purego.Dlopen(securityFramework, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			securityErr = fmt.Errorf("load the Security framework: %w", err)
			return
		}
		purego.RegisterLibFunc(&findGenericPassword, lib, "SecKeychainFindGenericPassword")
		purego.RegisterLibFunc(&freeKeychainItemContent, lib, "SecKeychainItemFreeContent")
	})
	return securityErr
}

// readKeychain returns the password of the generic password item for
// service in the default keychain search list. It blocks while macOS asks
// whether ebi-x may read the item.
func readKeychain(service string) ([]byte, error) {
	if err := loadSecurity(); err != nil {
		return nil, err
	}
	name := append([]byte(service), 0)
	var length uint32
	var data unsafe.Pointer
	if status := findGenericPassword(0, uint32(len(service)), &name[0], 0, nil, &length, &data, 0); status != 0 {
		return nil, fmt.Errorf("read Keychain item %q: %s", service, keychainStatus(status))
	}
	defer freeKeychainItemContent(0, data)
	return append([]byte(nil), unsafe.Slice((*byte)(data), length)...), nil
}

func keychainStatus(status int32) string {
	switch status {
	case -25300:
		return "no such item (errSecItemNotFound)"
	case -128:
		return "access was denied at the prompt (userCanceledErr)"
	case -25293:
		return "authorization failed (errSecAuthFailed)"
	case -25308:
		return "no user interaction allowed to approve access (errSecInteractionNotAllowed)"
	}
	return fmt.Sprintf("OSStatus %d", status)
}
