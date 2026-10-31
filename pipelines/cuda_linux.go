package pipelines

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Just enough of the CUDA driver API to copy a GL texture into a CUDA frame
// that NVENC encodes, without the pixels leaving the GPU.

type cuResult int32

func (r cuResult) err(what string) error {
	if r == 0 {
		return nil
	}
	return fmt.Errorf("%s: CUDA error %d", what, int32(r))
}

const (
	cuGraphicsRegisterReadOnly = 1
	cuMemoryTypeDevice         = 2
	cuMemoryTypeArray          = 3
)

// cuMemcpy2DParams is CUDA_MEMCPY2D.
type cuMemcpy2DParams struct {
	srcXInBytes, srcY uintptr
	srcMemoryType     uint32
	srcHost           uintptr
	srcDevice         uintptr
	srcArray          uintptr
	srcPitch          uintptr
	dstXInBytes, dstY uintptr
	dstMemoryType     uint32
	dstHost           uintptr
	dstDevice         uintptr
	dstArray          uintptr
	dstPitch          uintptr
	widthInBytes      uintptr
	height            uintptr
}

var (
	cudaOnce sync.Once
	cudaErr  error

	cuCtxPushCurrent                    func(ctx uintptr) cuResult
	cuCtxPopCurrent                     func(ctx *uintptr) cuResult
	cuGraphicsGLRegisterImage           func(res *uintptr, image, target uint32, flags uint32) cuResult
	cuGraphicsUnregisterResource        func(res uintptr) cuResult
	cuGraphicsMapResources              func(n uint32, res *uintptr, stream uintptr) cuResult
	cuGraphicsUnmapResources            func(n uint32, res *uintptr, stream uintptr) cuResult
	cuGraphicsSubResourceGetMappedArray func(arr *uintptr, res uintptr, index, level uint32) cuResult
	cuMemcpy2D                          func(p *cuMemcpy2DParams) cuResult
)

func loadCUDA() error {
	cudaOnce.Do(func() {
		lib, err := purego.Dlopen("libcuda.so.1", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			cudaErr = err
			return
		}
		for name, fn := range map[string]any{
			"cuCtxPushCurrent_v2":                 &cuCtxPushCurrent,
			"cuCtxPopCurrent_v2":                  &cuCtxPopCurrent,
			"cuGraphicsGLRegisterImage":           &cuGraphicsGLRegisterImage,
			"cuGraphicsUnregisterResource":        &cuGraphicsUnregisterResource,
			"cuGraphicsMapResources":              &cuGraphicsMapResources,
			"cuGraphicsUnmapResources":            &cuGraphicsUnmapResources,
			"cuGraphicsSubResourceGetMappedArray": &cuGraphicsSubResourceGetMappedArray,
			"cuMemcpy2D_v2":                       &cuMemcpy2D,
		} {
			if _, err := purego.Dlsym(lib, name); err != nil {
				cudaErr = fmt.Errorf("libcuda lacks %s", name)
				return
			}
			purego.RegisterLibFunc(fn, lib, name)
		}
	})
	return cudaErr
}

// cudaGLTexture is a GL texture registered with CUDA, readable as an array.
type cudaGLTexture struct {
	ctx uintptr
	res uintptr
}

func registerGLTexture(ctx uintptr, tex uint32) (*cudaGLTexture, error) {
	if err := loadCUDA(); err != nil {
		return nil, err
	}
	t := &cudaGLTexture{ctx: ctx}
	if err := cuCtxPushCurrent(ctx).err("cuCtxPushCurrent"); err != nil {
		return nil, err
	}
	defer popCUDA()
	if err := cuGraphicsGLRegisterImage(&t.res, tex, glTexture2D, cuGraphicsRegisterReadOnly).err("cuGraphicsGLRegisterImage"); err != nil {
		return nil, err
	}
	return t, nil
}

func popCUDA() {
	var c uintptr
	cuCtxPopCurrent(&c)
}

// copyTo copies the texture (w x h RGBA8) into device memory at dst.
func (t *cudaGLTexture) copyTo(dst unsafe.Pointer, pitch, w, h int) error {
	if err := cuCtxPushCurrent(t.ctx).err("cuCtxPushCurrent"); err != nil {
		return err
	}
	defer popCUDA()
	if err := cuGraphicsMapResources(1, &t.res, 0).err("cuGraphicsMapResources"); err != nil {
		return err
	}
	defer cuGraphicsUnmapResources(1, &t.res, 0)
	var arr uintptr
	if err := cuGraphicsSubResourceGetMappedArray(&arr, t.res, 0, 0).err("cuGraphicsSubResourceGetMappedArray"); err != nil {
		return err
	}
	p := cuMemcpy2DParams{
		srcMemoryType: cuMemoryTypeArray,
		srcArray:      arr,
		dstMemoryType: cuMemoryTypeDevice,
		dstDevice:     uintptr(dst),
		dstPitch:      uintptr(pitch),
		widthInBytes:  uintptr(w * 4),
		height:        uintptr(h),
	}
	return cuMemcpy2D(&p).err("cuMemcpy2D")
}

func (t *cudaGLTexture) close() {
	if t == nil || t.res == 0 {
		return
	}
	if cuCtxPushCurrent(t.ctx) == 0 {
		cuGraphicsUnregisterResource(t.res)
		popCUDA()
	}
	t.res = 0
}
