//go:build darwin && cgo

package auth

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>
static CFMutableDictionaryRef query(const char *service,const char *account){
 CFMutableDictionaryRef q=CFDictionaryCreateMutable(NULL,0,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
 CFStringRef s=CFStringCreateWithCString(NULL,service,kCFStringEncodingUTF8),a=CFStringCreateWithCString(NULL,account,kCFStringEncodingUTF8);
 CFDictionarySetValue(q,kSecClass,kSecClassGenericPassword);CFDictionarySetValue(q,kSecAttrService,s);CFDictionarySetValue(q,kSecAttrAccount,a);CFRelease(s);CFRelease(a);return q;
}
static OSStatus loadSecret(const char*s,const char*a,void **out,CFIndex *len){
 CFMutableDictionaryRef q=query(s,a);CFDictionarySetValue(q,kSecReturnData,kCFBooleanTrue);CFDictionarySetValue(q,kSecMatchLimit,kSecMatchLimitOne);
 CFTypeRef result=NULL;OSStatus status=SecItemCopyMatching(q,&result);CFRelease(q);
 if(status==errSecSuccess){*len=CFDataGetLength((CFDataRef)result);*out=malloc(*len);memcpy(*out,CFDataGetBytePtr((CFDataRef)result),*len);CFRelease(result);}return status;
}
static OSStatus saveSecret(const char*s,const char*a,const void*b,CFIndex len){
 CFMutableDictionaryRef q=query(s,a);CFDataRef data=CFDataCreate(NULL,b,len);CFMutableDictionaryRef attrs=CFDictionaryCreateMutable(NULL,0,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);CFDictionarySetValue(attrs,kSecValueData,data);
 OSStatus status=SecItemUpdate(q,attrs);if(status==errSecItemNotFound){CFDictionarySetValue(q,kSecValueData,data);CFDictionarySetValue(q,kSecAttrAccessible,kSecAttrAccessibleWhenUnlockedThisDeviceOnly);status=SecItemAdd(q,NULL);}CFRelease(attrs);CFRelease(data);CFRelease(q);return status;
}
static OSStatus deleteSecret(const char*s,const char*a){CFMutableDictionaryRef q=query(s,a);OSStatus status=SecItemDelete(q);CFRelease(q);return status;}
*/
import "C"
import (
	"bytes"
	"fmt"
	"unsafe"
)

type keychainStore struct{ service, account string }

func NewKeychainStore(service, account string) Store {
	return &keychainStore{service: service, account: account}
}
func (s *keychainStore) strings() (*C.char, *C.char, func()) {
	a, b := C.CString(s.service), C.CString(s.account)
	return a, b, func() { C.free(unsafe.Pointer(a)); C.free(unsafe.Pointer(b)) }
}
func keychainError(status C.OSStatus) error {
	if status == C.errSecItemNotFound {
		return ErrNotFound
	}
	return fmt.Errorf("Keychain operation failed (status %d)", int(status))
}
func (s *keychainStore) Load() (Credentials, error) {
	a, b, done := s.strings()
	defer done()
	var out unsafe.Pointer
	var n C.CFIndex
	status := C.loadSecret(a, b, &out, &n)
	if status != C.errSecSuccess {
		return Credentials{}, keychainError(status)
	}
	defer C.free(out)
	return Parse(bytes.NewReader(C.GoBytes(out, C.int(n))))
}
func (s *keychainStore) Save(c Credentials) error {
	b, err := encode(c)
	if err != nil {
		return err
	}
	a, d, done := s.strings()
	defer done()
	status := C.saveSecret(a, d, unsafe.Pointer(&b[0]), C.CFIndex(len(b)))
	if status != C.errSecSuccess {
		return keychainError(status)
	}
	return nil
}
func (s *keychainStore) Delete() error {
	a, b, done := s.strings()
	defer done()
	status := C.deleteSecret(a, b)
	if status != C.errSecSuccess && status != C.errSecItemNotFound {
		return keychainError(status)
	}
	return nil
}
