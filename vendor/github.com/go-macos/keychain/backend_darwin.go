//go:build darwin

package keychain

import (
	"fmt"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// This file binds the CoreFoundation + Security C APIs through purego (no cgo)
// and wires them into the OS-independent seams declared in keychain.go. The
// Keychain surface is plain C functions operating on CFDictionary/CFData — not
// Objective-C message sends — so it is reached with purego's dlsym/RegisterFunc
// directly (the go-macos/objc msgSend bridge does not apply); go-macos/objc is
// reused for the canonical framework paths.

// CoreFoundation string encoding for CFStringCreateWithCString.
const kCFStringEncodingUTF8 = 0x08000100

var (
	cfStringCreate      func(alloc uintptr, s string, enc uint32) uintptr
	cfDataCreate        func(alloc uintptr, bytes *byte, length int) uintptr
	cfDictCreateMutable func(alloc uintptr, capacity int, keyCB, valCB uintptr) uintptr
	cfDictSetValue      func(dict, key, value uintptr)
	cfRelease           func(cf uintptr)
	cfDataGetLength     func(data uintptr) int
	cfDataGetBytePtr    func(data uintptr) *byte

	secItemAdd          func(attrs uintptr, result *uintptr) int32
	secItemUpdate       func(query, attrs uintptr) int32
	secItemCopyMatching func(query uintptr, result *uintptr) int32
	secItemDelete       func(query uintptr) int32
	secACCreate         func(alloc, protection uintptr, flags uint, err *uintptr) uintptr

	// CFStringRef / CFBooleanRef constants (the exported symbol holds the ref).
	kSecClass                                    uintptr
	kSecClassGenericPassword                     uintptr
	kSecAttrService                              uintptr
	kSecAttrAccount                              uintptr
	kSecValueData                                uintptr
	kSecReturnData                               uintptr
	kSecMatchLimit                               uintptr
	kSecMatchLimitOne                            uintptr
	kSecAttrAccessControl                        uintptr
	kSecAttrAccessibleWhenUnlockedThisDeviceOnly uintptr
	kCFBooleanTrue                               uintptr

	// Addresses of the dictionary callback structs (passed by pointer).
	kCFTypeDictionaryKeyCallBacks   uintptr
	kCFTypeDictionaryValueCallBacks uintptr
)

func init() {
	load()
	backendSet = realSet
	backendGet = realGet
	backendDelete = realDelete
}

// load resolves every CoreFoundation + Security symbol the backend needs,
// recording the first failure in backendLoadErr. A missing symbol on a
// supported macOS is not expected — these branches are defensive.
func load() {
	cf, err := purego.Dlopen(objc.CoreFoundation, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		backendLoadErr = fmt.Errorf("keychain: load CoreFoundation: %w", err)
		return
	}
	sec, err := purego.Dlopen(objc.Security, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		backendLoadErr = fmt.Errorf("keychain: load Security: %w", err)
		return
	}

	bind := func(fptr any, h uintptr, name string) {
		if backendLoadErr != nil {
			return
		}
		p, err := purego.Dlsym(h, name)
		if err != nil || p == 0 {
			backendLoadErr = fmt.Errorf("keychain: missing function %s", name)
			return
		}
		purego.RegisterFunc(fptr, p)
	}
	// A value symbol dereferences to the ref it holds (e.g. kSecClass).
	ref := func(h uintptr, name string) uintptr {
		if backendLoadErr != nil {
			return 0
		}
		p, err := purego.Dlsym(h, name)
		if err != nil || p == 0 {
			backendLoadErr = fmt.Errorf("keychain: missing symbol %s", name)
			return 0
		}
		return symbolValue(p)
	}
	// A struct symbol is passed by its own address (the callback tables).
	addr := func(h uintptr, name string) uintptr {
		if backendLoadErr != nil {
			return 0
		}
		p, err := purego.Dlsym(h, name)
		if err != nil || p == 0 {
			backendLoadErr = fmt.Errorf("keychain: missing struct %s", name)
			return 0
		}
		return p
	}

	bind(&cfStringCreate, cf, "CFStringCreateWithCString")
	bind(&cfDataCreate, cf, "CFDataCreate")
	bind(&cfDictCreateMutable, cf, "CFDictionaryCreateMutable")
	bind(&cfDictSetValue, cf, "CFDictionarySetValue")
	bind(&cfRelease, cf, "CFRelease")
	bind(&cfDataGetLength, cf, "CFDataGetLength")
	bind(&cfDataGetBytePtr, cf, "CFDataGetBytePtr")
	bind(&secItemAdd, sec, "SecItemAdd")
	bind(&secItemUpdate, sec, "SecItemUpdate")
	bind(&secItemCopyMatching, sec, "SecItemCopyMatching")
	bind(&secItemDelete, sec, "SecItemDelete")
	bind(&secACCreate, sec, "SecAccessControlCreateWithFlags")

	kCFBooleanTrue = ref(cf, "kCFBooleanTrue")
	kCFTypeDictionaryKeyCallBacks = addr(cf, "kCFTypeDictionaryKeyCallBacks")
	kCFTypeDictionaryValueCallBacks = addr(cf, "kCFTypeDictionaryValueCallBacks")
	kSecClass = ref(sec, "kSecClass")
	kSecClassGenericPassword = ref(sec, "kSecClassGenericPassword")
	kSecAttrService = ref(sec, "kSecAttrService")
	kSecAttrAccount = ref(sec, "kSecAttrAccount")
	kSecValueData = ref(sec, "kSecValueData")
	kSecReturnData = ref(sec, "kSecReturnData")
	kSecMatchLimit = ref(sec, "kSecMatchLimit")
	kSecMatchLimitOne = ref(sec, "kSecMatchLimitOne")
	kSecAttrAccessControl = ref(sec, "kSecAttrAccessControl")
	kSecAttrAccessibleWhenUnlockedThisDeviceOnly = ref(sec, "kSecAttrAccessibleWhenUnlockedThisDeviceOnly")
}

// symbolValue reads the pointer-sized value stored at a data symbol's
// address, as Dlsym returns it: the ref a constant such as kSecClass holds.
//
// The address is in a framework's data segment, never Go memory, so the
// garbage collector neither moves nor frees what it names. Still, turning
// that uintptr into an unsafe.Pointer is none of the valid patterns of the
// unsafe package ("Conversion of a uintptr back to Pointer is not valid in
// general"). Reinterpreting the variable that holds the address, a *uintptr,
// as a **uintptr is pattern (1), *T1 to *T2 of the same size and layout,
// and dereferencing it twice reads the symbol's value with no uintptr ever
// becoming a Pointer.
func symbolValue(addr uintptr) uintptr {
	return **(**uintptr)(unsafe.Pointer(&addr))
}

// cfStr makes a CFString from a Go string (caller releases).
func cfStr(s string) uintptr { return cfStringCreate(0, s, kCFStringEncodingUTF8) }

// newDict makes an empty mutable CFDictionary (caller releases).
func newDict() uintptr {
	return cfDictCreateMutable(0, 0, kCFTypeDictionaryKeyCallBacks, kCFTypeDictionaryValueCallBacks)
}

// baseQuery builds a mutable dictionary keyed to the (service, account)
// generic-password item. svc and acc are the caller-owned CFStrings.
func baseQuery(svc, acc uintptr) uintptr {
	d := newDict()
	cfDictSetValue(d, kSecClass, kSecClassGenericPassword)
	cfDictSetValue(d, kSecAttrService, svc)
	cfDictSetValue(d, kSecAttrAccount, acc)
	return d
}

// realSet is the darwin backendSet. With no access control it updates the
// item in place, adding it on first write; with access control it recreates
// the item behind a fresh SecAccessControl (an existing item cannot be
// updated to carry one, so it is deleted then re-added).
func realSet(service, account string, secret []byte, cfg config) int32 {
	svc, acc := cfStr(service), cfStr(account)
	defer cfRelease(svc)
	defer cfRelease(acc)
	// Go memory passed to C under the cgo rules: CFDataCreate copies the
	// bytes and keeps no reference, and the slice stays alive through the
	// call.
	data := cfDataCreate(0, &secret[0], len(secret))
	runtime.KeepAlive(secret)
	defer cfRelease(data)

	if cfg.useAC {
		del := baseQuery(svc, acc)
		secItemDelete(del)
		cfRelease(del)

		var acErr uintptr
		ac := secACCreate(0, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, uint(cfg.acFlags), &acErr)
		if ac == 0 {
			return errSecAccessControlFailed // defensive: creation only fails on bad flags
		}
		defer cfRelease(ac)

		add := baseQuery(svc, acc)
		defer cfRelease(add)
		cfDictSetValue(add, kSecValueData, data)
		cfDictSetValue(add, kSecAttrAccessControl, ac)
		return secItemAdd(add, nil)
	}

	query := baseQuery(svc, acc)
	defer cfRelease(query)
	upd := newDict()
	defer cfRelease(upd)
	cfDictSetValue(upd, kSecValueData, data)
	st := secItemUpdate(query, upd)
	if st == errSecItemNotFound {
		add := baseQuery(svc, acc)
		defer cfRelease(add)
		cfDictSetValue(add, kSecValueData, data)
		st = secItemAdd(add, nil)
	}
	return st
}

// realGet is the darwin backendGet. It copies the stored bytes into a
// Go-owned slice so nothing points at CoreFoundation-owned memory after the
// CFData is released.
func realGet(service, account string) ([]byte, int32) {
	svc, acc := cfStr(service), cfStr(account)
	defer cfRelease(svc)
	defer cfRelease(acc)

	q := baseQuery(svc, acc)
	defer cfRelease(q)
	cfDictSetValue(q, kSecReturnData, kCFBooleanTrue)
	cfDictSetValue(q, kSecMatchLimit, kSecMatchLimitOne)

	var result uintptr
	if st := secItemCopyMatching(q, &result); st != errSecSuccess {
		return nil, st
	}
	defer cfRelease(result)

	n := cfDataGetLength(result)
	out := make([]byte, n)
	if n > 0 { // n==0 is unreachable: Set rejects an empty secret
		copy(out, unsafe.Slice(cfDataGetBytePtr(result), n))
	}
	return out, errSecSuccess
}

// realDelete is the darwin backendDelete.
func realDelete(service, account string) int32 {
	svc, acc := cfStr(service), cfStr(account)
	defer cfRelease(svc)
	defer cfRelease(acc)
	q := baseQuery(svc, acc)
	defer cfRelease(q)
	return secItemDelete(q)
}
