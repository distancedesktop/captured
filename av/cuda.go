//go:build linux

package av

/*
#cgo pkg-config: libavutil
#define CUDA_VERSION 11000
typedef struct CUctx_st *CUcontext;
typedef struct CUstream_st *CUstream;
#include <libavutil/hwcontext_cuda.h>
static void *cap_cuda_ctx(void *hwctx) { return ((AVCUDADeviceContext *)hwctx)->cuda_ctx; }
*/
import "C"

import "unsafe"

// CUDAContext is the CUcontext libav created for a cuda device.
func (d *HWDevice) CUDAContext() uintptr {
	return uintptr(C.cap_cuda_ctx(d.Hwctx()))
}

var _ = unsafe.Pointer(nil)
