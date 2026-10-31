package main

import (
	"fmt"
	"image"
	"syscall"
	"unsafe"

	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows"
)

var (
	iconShell        = windows.NewLazySystemDLL("shell32.dll").NewProc("SHCreateItemFromParsingName")
	iconGDI          = windows.NewLazySystemDLL("gdi32.dll")
	iconGetObject    = iconGDI.NewProc("GetObjectW")
	iconDeleteObject = iconGDI.NewProc("DeleteObject")
	imageFactoryIID  = windows.GUID{Data1: 0xbcc18b79, Data2: 0xba16, Data3: 0x442f, Data4: [8]byte{0x80, 0xc4, 0x8a, 0x59, 0xc3, 0x0c, 0x46, 0x3b}}
)

type nativeBitmap struct {
	Type, Width, Height, Stride int32
	Planes, BitsPerPixel        uint16
	Bits                        unsafe.Pointer
}

type nativeDIB struct {
	Bitmap                       nativeBitmap
	HeaderSize                   uint32
	Width, Height                int32
	Planes, BitsPerPixel         uint16
	Compression, ImageSize       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ColorsUsed, ColorsImportant  uint32
	Masks                        [3]uint32
	Section                      windows.Handle
	Offset                       uint32
}

func initIconApartment() error {
	err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	// S_FALSE means this thread already has the requested apartment; it still
	// increments COM's initialization count and needs CoUninitialize.
	if err == syscall.Errno(windows.S_FALSE) {
		return nil
	}
	return err
}

// Called only by the icon worker on its own COM apartment. MyGo's FileIcon
// marshals shell extraction and PNG encoding onto the UI thread; keeping the
// native pixels here avoids both the UI stall and a PNG encode/decode round trip.
func loadFileIcon(path string, px int) (*ui.Bitmap, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var factory unsafe.Pointer
	hr, _, _ := iconShell.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&imageFactoryIID)), uintptr(unsafe.Pointer(&factory)))
	if int32(hr) < 0 || factory == nil {
		return nil, fmt.Errorf("shell icon: HRESULT %#x", uint32(hr))
	}
	vtable := *(*[4]uintptr)(*(*unsafe.Pointer)(factory))
	defer syscall.SyscallN(vtable[2], uintptr(factory)) // IUnknown.Release
	var bitmap windows.Handle
	size := uintptr(uint32(px)) | uintptr(uint32(px))<<32
	hr, _, _ = syscall.SyscallN(vtable[3], uintptr(factory), size, 4, uintptr(unsafe.Pointer(&bitmap))) // IShellItemImageFactory.GetImage, ICONONLY
	if int32(hr) < 0 || bitmap == 0 {
		return nil, fmt.Errorf("shell image: HRESULT %#x", uint32(hr))
	}
	defer iconDeleteObject.Call(uintptr(bitmap))
	var dib nativeDIB
	n, _, err := iconGetObject.Call(uintptr(bitmap), unsafe.Sizeof(dib), uintptr(unsafe.Pointer(&dib)))
	b := dib.Bitmap
	if n == 0 || b.Bits == nil || b.BitsPerPixel != 32 || b.Width <= 0 || b.Height <= 0 || b.Width > 4096 || b.Height > 4096 || b.Stride < b.Width*4 {
		return nil, fmt.Errorf("shell bitmap: %v", err)
	}
	src := unsafe.Slice((*byte)(b.Bits), int(b.Stride)*int(b.Height))
	dst := image.NewRGBA(image.Rect(0, 0, int(b.Width), int(b.Height)))
	for y := range int(b.Height) {
		row := y
		if dib.Height > 0 {
			row = int(b.Height) - 1 - y
		}
		s := src[row*int(b.Stride):]
		d := dst.Pix[y*dst.Stride:]
		for x := range int(b.Width) {
			i := x * 4
			a := uint16(s[i+3])
			d[i], d[i+1], d[i+2], d[i+3] = uint8(uint16(s[i+2])*a/255), uint8(uint16(s[i+1])*a/255), uint8(uint16(s[i])*a/255), uint8(a)
		}
	}
	return ui.NewBitmap(dst), nil
}
