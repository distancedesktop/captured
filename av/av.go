// Package av wraps the bits of libavcodec captured needs: open a (hardware)
// encoder, hand out frames to fill, and pull encoded packets back out.
package av

/*
#cgo pkg-config: libavcodec libavutil libswscale
#include "av.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// HWType names a libav hardware device type ("cuda", "vaapi", "d3d11va", ...).
type HWType string

const (
	CPU          HWType = ""
	CUDA         HWType = "cuda"
	VAAPI        HWType = "vaapi"
	D3D11        HWType = "d3d11va"
	VideoToolbox HWType = "videotoolbox"
)

// PixFmt is a libav pixel format.
type PixFmt int32

var (
	RGB0 = PixFmt(C.AV_PIX_FMT_RGB0)
	BGR0 = PixFmt(C.AV_PIX_FMT_BGR0)
	BGRA = PixFmt(C.AV_PIX_FMT_BGRA)
	NV12 = PixFmt(C.AV_PIX_FMT_NV12)
)

func hwPixFmt(t HWType) C.enum_AVPixelFormat {
	switch t {
	case CUDA:
		return C.AV_PIX_FMT_CUDA
	case VAAPI:
		return C.AV_PIX_FMT_VAAPI
	case D3D11:
		return C.AV_PIX_FMT_D3D11
	case VideoToolbox:
		return C.AV_PIX_FMT_VIDEOTOOLBOX
	}
	return C.AV_PIX_FMT_NONE
}

func avErr(what string, ret C.int) error {
	return fmt.Errorf("%s: %s", what, C.GoString(C.cap_err(ret)))
}

// HWDevice is a libav hardware device context.
type HWDevice struct {
	Type HWType
	ref  *C.AVBufferRef
}

// NewHWDevice opens a device. device is type specific ("" for the default,
// a render node for vaapi, a GPU index for cuda).
func NewHWDevice(t HWType, device string) (*HWDevice, error) {
	ct := C.CString(string(t))
	defer C.free(unsafe.Pointer(ct))
	typ := C.av_hwdevice_find_type_by_name(ct)
	if typ == C.AV_HWDEVICE_TYPE_NONE {
		return nil, fmt.Errorf("libav has no %s support", t)
	}
	var cd *C.char
	if device != "" {
		cd = C.CString(device)
		defer C.free(unsafe.Pointer(cd))
	}
	d := &HWDevice{Type: t}
	if ret := C.av_hwdevice_ctx_create(&d.ref, typ, cd, nil, 0); ret < 0 {
		return nil, avErr("open "+string(t)+" device", ret)
	}
	return d, nil
}

// Hwctx is the device's AVHWDeviceContext.hwctx (AVCUDADeviceContext etc).
func (d *HWDevice) Hwctx() unsafe.Pointer {
	return (*C.AVHWDeviceContext)(unsafe.Pointer(d.ref.data)).hwctx
}

func (d *HWDevice) Close() {
	if d != nil && d.ref != nil {
		C.av_buffer_unref(&d.ref)
	}
}

// Frame is an AVFrame the caller fills before Encode.
type Frame struct{ f *C.AVFrame }

func (f Frame) Data(i int) unsafe.Pointer { return unsafe.Pointer(f.f.data[i]) }
func (f Frame) Linesize(i int) int        { return int(f.f.linesize[i]) }

// Bytes is the tight BGRA plane of a cpu frame.
func (f Frame) Bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(f.f.data[0])), int(f.f.linesize[0])*int(f.f.height))
}

// Free drops a frame that won't be encoded.
func (f Frame) Free() { C.av_frame_free(&f.f) }

// Config describes the encoder to open.
type Config struct {
	Encoder    string
	SrcW, SrcH int // what the capture produces
	W, H       int // what gets encoded
	FPS        int
	Bitrate    int64
	GOP        int // <= 0: only on request
	Preset     string
	Options    map[string]string
	HW         *HWDevice // zero copy: caller fills frames on this device
	HWFormat   PixFmt    // sw format of those hw frames
	UploadDev  *HWDevice // cpu input into a hw-only encoder (vaapi)
}

type Packet struct {
	Data []byte // valid until the next Receive
	PTS  int64
	Key  bool
}

type Encoder struct {
	e    C.cap_enc
	Name string
	HW   HWType // set when frames go in on the GPU
}

// Open opens cfg.Encoder with low latency defaults for its family.
func Open(cfg Config) (*Encoder, error) {
	opts := lowLatency(cfg)
	var dict *C.AVDictionary
	for k, v := range opts {
		ck, cv := C.CString(k), C.CString(v)
		C.av_dict_set(&dict, ck, cv, 0)
		C.free(unsafe.Pointer(ck))
		C.free(unsafe.Pointer(cv))
	}
	defer C.av_dict_free(&dict)

	gop := cfg.GOP
	if gop <= 0 {
		gop = 1 << 30
		if isFamily(cfg.Encoder, "nvenc") {
			gop = -1
		}
	}
	enc := &Encoder{Name: cfg.Encoder}
	name := C.CString(cfg.Encoder)
	defer C.free(unsafe.Pointer(name))
	var dev *C.AVBufferRef
	hwFmt := C.enum_AVPixelFormat(C.AV_PIX_FMT_NONE)
	swFmt := C.enum_AVPixelFormat(C.AV_PIX_FMT_NONE)
	if cfg.HW != nil {
		dev, hwFmt, swFmt = cfg.HW.ref, hwPixFmt(cfg.HW.Type), C.enum_AVPixelFormat(cfg.HWFormat)
		enc.HW = cfg.HW.Type
	} else if cfg.UploadDev != nil {
		dev = cfg.UploadDev.ref
	}
	ret := C.cap_enc_open(&enc.e, name, C.int(cfg.SrcW), C.int(cfg.SrcH), C.int(cfg.W), C.int(cfg.H), C.int(cfg.FPS),
		C.int64_t(cfg.Bitrate), C.int(gop), dev, hwFmt, swFmt, &dict)
	if ret < 0 {
		C.cap_enc_close(&enc.e)
		return nil, avErr(cfg.Encoder, ret)
	}
	return enc, nil
}

func isFamily(name, fam string) bool {
	n := len(name) - len(fam)
	return n > 0 && name[n-1] == '_' && name[n:] == fam
}

// Family is the encoder's backend: "nvenc", "vaapi", "libx264", ...
func Family(name string) string {
	for _, f := range []string{"nvenc", "vaapi", "qsv", "amf", "videotoolbox", "mf"} {
		if isFamily(name, f) {
			return f
		}
	}
	return name
}

func lowLatency(cfg Config) map[string]string {
	o := map[string]string{}
	preset := cfg.Preset
	switch Family(cfg.Encoder) {
	case "nvenc":
		if preset == "" {
			preset = "p1"
		}
		o["preset"], o["tune"], o["rc"] = preset, "ull", "cbr"
		o["zerolatency"], o["delay"], o["forced-idr"] = "1", "0", "1"
	case "libx264", "libx265":
		if preset == "" {
			preset = "superfast"
		}
		o["preset"], o["tune"], o["forced-idr"] = preset, "zerolatency", "1"
	case "libsvtav1":
		if preset == "" {
			preset = "12"
		}
		// low delay prediction, CBR
		o["preset"], o["svtav1-params"] = preset, "pred-struct=1:rc=2"
	case "vaapi":
		o["rc_mode"] = "CBR"
	case "qsv":
		if preset == "" {
			preset = "veryfast"
		}
		o["preset"], o["async_depth"], o["low_delay_brc"] = preset, "1", "1"
	case "amf":
		o["usage"], o["rc"] = "ultralowlatency", "cbr"
		if preset != "" {
			o["quality"] = preset
		}
	case "videotoolbox":
		o["realtime"], o["prio_speed"] = "1", "1"
	}
	for k, v := range cfg.Options {
		o[k] = v
	}
	return o
}

// Frame returns a frame to fill: a GPU surface (HW set) or tight BGRA.
func (e *Encoder) Frame() (Frame, error) {
	f := C.cap_enc_frame(&e.e)
	if f == nil {
		return Frame{}, errors.New("out of frames")
	}
	return Frame{f}, nil
}

// Encode consumes f and calls emit for every packet that comes out.
func (e *Encoder) Encode(f Frame, pts int64, emit func(Packet) error) error {
	f.f.pts = C.int64_t(pts)
	if ret := C.cap_enc_send(&e.e, f.f); ret < 0 {
		return avErr("encode", ret)
	}
	for {
		ret := C.cap_enc_recv(&e.e)
		if ret < 0 {
			return avErr("encode", ret)
		}
		if ret == 0 {
			return nil
		}
		p := e.e.pkt
		err := emit(Packet{
			Data: unsafe.Slice((*byte)(unsafe.Pointer(p.data)), int(p.size)),
			PTS:  int64(p.pts),
			Key:  p.flags&C.AV_PKT_FLAG_KEY != 0,
		})
		if err != nil {
			return err
		}
	}
}

// Keyframe makes the next frame an IDR.
func (e *Encoder) Keyframe() { e.e.force_key = 1 }

// LiveBitrate reports whether SetBitrate works without reopening.
func (e *Encoder) LiveBitrate() bool {
	f := Family(e.Name)
	return f == "nvenc" || f == "libx264"
}

func (e *Encoder) SetBitrate(bps int64) {
	c := e.e.ctx
	c.bit_rate = C.int64_t(bps)
	if c.rc_max_rate == 0 {
		return
	}
	c.rc_max_rate = C.int64_t(bps)
	if c.framerate.num > 0 {
		c.rc_buffer_size = C.int(bps / int64(c.framerate.num))
	}
}

func (e *Encoder) Close() { C.cap_enc_close(&e.e) }

// Available reports whether libav was built with this encoder.
func Available(name string) bool {
	c := C.CString(name)
	defer C.free(unsafe.Pointer(c))
	return C.avcodec_find_encoder_by_name(c) != nil
}

// SupportsHW reports whether the encoder takes frames of this device type.
func SupportsHW(name string, t HWType) bool {
	c := C.CString(name)
	defer C.free(unsafe.Pointer(c))
	codec := C.avcodec_find_encoder_by_name(c)
	return codec != nil && C.cap_supports(codec, hwPixFmt(t)) != 0
}

// Candidates lists encoders to try for codec, hardware first.
func Candidates(codec string) []string {
	var hw, sw []string
	switch codec {
	case "h264":
		sw = []string{"libx264", "libopenh264"}
	case "hevc":
		sw = []string{"libx265"}
	case "av1":
		sw = []string{"libsvtav1"} // libaom can't keep up in real time
	}
	for _, f := range []string{"nvenc", "amf", "qsv", "videotoolbox", "vaapi", "mf"} {
		hw = append(hw, codec+"_"+f)
	}
	var out []string
	for _, n := range append(hw, sw...) {
		if Available(n) {
			out = append(out, n)
		}
	}
	return out
}
