//go:build windows

package av

/*
#cgo pkg-config: libavutil
#include <libavutil/hwcontext_d3d11va.h>
*/
import "C"

import "unsafe"

// D3D11Device is the ID3D11Device a d3d11va device context created.
func (d *HWDevice) D3D11Device() unsafe.Pointer {
	return unsafe.Pointer((*C.AVD3D11VADeviceContext)(d.Hwctx()).device)
}
