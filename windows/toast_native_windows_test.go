//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows/registry"
)

// Exercise real WinRT ABI calls, not a mock. Do not show a notification, register
// an app, alter the user's notification history, or overwrite their clipboard.
func TestNativeToastTagGroupAndSuppression(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := toastCombase.NewProc("RoInitialize").Call(1)
	if err := hresultError(hr); err != nil {
		t.Fatal(err)
	}
	defer toastCombase.NewProc("RoUninitialize").Call()
	for _, suppress := range []bool{false, true} {
		sms := SMS{From: "Test <sender>", Text: "验证码 123456 & 测试"}
		toast, err := newTaggedToast(buildSMSNotificationXML(sms), smsToastTagFor(sms.From), suppress)
		if err != nil {
			t.Fatal(err)
		}
		properties, err := toast.QueryInterface(ole.NewGUID("9dfb9fd1-143a-490e-90bf-b9fba7132de7"))
		if err != nil {
			toast.Release()
			t.Fatal(err)
		}
		for _, tc := range []struct {
			slot int
			want string
		}{{7, smsToastTagFor(sms.From)}, {9, smsToastGroup}} {
			var value ole.HString
			if err := toastCOMCall(&properties.IUnknown, tc.slot, uintptr(unsafe.Pointer(&value))); err != nil {
				t.Fatal(err)
			}
			got := value.String()
			ole.DeleteHString(value)
			if got != tc.want {
				t.Errorf("slot %d = %q, want %q", tc.slot, got, tc.want)
			}
		}
		var quiet byte
		if err := toastCOMCall(&properties.IUnknown, 11, uintptr(unsafe.Pointer(&quiet))); err != nil {
			t.Fatal(err)
		}
		if (quiet != 0) != suppress {
			t.Errorf("suppress=%d want %v", quiet, suppress)
		}
		properties.Release()
		toast.Release()
	}
}

// Opt-in delivery check under a unique synthetic app identity. No banner, sound,
// user clipboard, production registration or existing notification is touched.
func TestNativeToastHistoryReplacement(t *testing.T) {
	if os.Getenv("MSGDOCK_TEST_TOAST_HISTORY") != "1" {
		t.Skip("opt-in Windows notification-center smoke test")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := toastCombase.NewProc("RoInitialize").Call(1)
	if err := hresultError(hr); err != nil {
		t.Fatal(err)
	}
	defer toastCombase.NewProc("RoUninitialize").Call()
	appID := fmt.Sprintf("MsgDock.QA.%d.%d", os.Getpid(), time.Now().UnixNano())
	regPath := `SOFTWARE\Classes\AppUserModelId\` + appID
	key, _, err := registry.CreateKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		key.Close()
		if err := registry.DeleteKey(registry.CURRENT_USER, regPath); err != nil {
			t.Error(err)
		}
	}()
	if err := key.SetStringValue("DisplayName", "MsgDock isolated notification test"); err != nil {
		t.Fatal(err)
	}
	manager, err := ole.RoGetActivationFactory("Windows.UI.Notifications.ToastNotificationManager", ole.NewGUID("7ab93c52-0e48-4750-ba9d-1a4113981847"))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release()
	var history *ole.IUnknown
	if err := toastCOMCall(&manager.IUnknown, 6, uintptr(unsafe.Pointer(&history))); err != nil {
		t.Fatal(err)
	}
	defer history.Release()
	id, err := ole.NewHString(appID)
	if err != nil {
		t.Fatal(err)
	}
	defer ole.DeleteHString(id)
	defer func() {
		if err := toastCOMCall(history, 12, uintptr(id)); err != nil {
			t.Error(err)
		}
	}()
	reader, err := history.QueryInterface(ole.NewGUID("3bc3d253-2f31-4092-9129-8ad5abf067da"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Release()
	count := func() uint32 {
		var items *ole.IUnknown
		if err := toastCOMCall(&reader.IUnknown, 7, uintptr(id), uintptr(unsafe.Pointer(&items))); err != nil {
			t.Fatal(err)
		}
		defer items.Release()
		var size uint32
		if err := toastCOMCall(items, 7, uintptr(unsafe.Pointer(&size))); err != nil {
			t.Fatal(err)
		}
		return size
	}
	if count() != 0 {
		t.Fatal("test identity unexpectedly has existing notifications")
	}
	for i := 0; i < 3; i++ {
		if err := pushTaggedToast(appID, buildSMSNotificationXML(SMS{From: "MsgDock test", Text: fmt.Sprintf("Synthetic message %d", i)}), smsToastTagFor("MsgDock test"), true); err != nil {
			t.Fatal(err)
		}
		var size uint32
		for attempt := 0; attempt < 20; attempt++ {
			size = count()
			if size == 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if size != 1 {
			t.Fatalf("after %d pushes, notification history count=%d, want 1", i+1, size)
		}
	}
	t.Log("Three silent native deliveries left exactly one card under the isolated test AppID; test history and registration are removed on exit")
}
